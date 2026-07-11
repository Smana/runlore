// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// fakeExecutor is a no-op action.Executor for wiring tests that need a non-nil
// cluster executor without a real cluster.
type fakeExecutor struct{}

func (fakeExecutor) Execute(context.Context, providers.Action) error { return nil }

// discardLog returns a logger that drops every record, keeping wiring tests quiet
// and deterministic.
func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuildModelAndToolsSmoke is a wiring smoke test for the shared
// model+tools+recall assembly used by serve/investigate. With a representative
// minimal config — a configured model, no gitops, no catalog, no
// metrics/logs/network/cloud — it must build a non-nil model and a (possibly
// empty) tool slice without panicking, and not enable instant recall absent a
// catalog. KUBECONFIG is pointed at a nonexistent file so the in-cluster/kube
// probe fails fast and deterministically (the cluster-backed tools are simply
// omitted) rather than depending on the host's ambient kube context.
func TestBuildModelAndToolsSmoke(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))

	for _, provider := range []string{"openai", "anthropic", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{Model: config.Model{Provider: provider, BaseURL: "http://vllm:8000/v1", Model: "test-model"}}
			log := discardLog()

			model, tools, recall, cat := BuildModelAndTools(context.Background(), cfg, nil, nil, log)
			if model == nil {
				t.Fatal("BuildModelAndTools returned a nil model")
			}
			// A nil/empty tool slice is acceptable (nothing wired here); each present
			// tool must just be usable.
			for i, tl := range tools {
				if tl == nil {
					t.Fatalf("tool %d is nil", i)
				}
				if tl.Name() == "" {
					t.Fatalf("tool %d has an empty name", i)
				}
			}
			if recall != nil {
				t.Fatal("instant recall must be off without a catalog")
			}
			if cat != nil {
				t.Fatal("no catalog configured, want nil catalog")
			}
		})
	}
}

// toolNames collects the registered tool names into a set for membership assertions.
func toolNames(tools []investigate.Tool) map[string]bool {
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	return names
}

// TestDiscoveryToolsGatedByProvider asserts the three new investigation tools appear
// EXACTLY when their backing provider is configured: discover_metrics with the metrics
// backend, and logs_error_summary + discover_log_fields with the logs backend. With
// neither configured they must be absent; wiring only one backend must not enable the
// other's tools. KUBECONFIG is pointed at a nonexistent file so cluster-backed tools are
// deterministically omitted and don't perturb the assertions.
func TestDiscoveryToolsGatedByProvider(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))
	log := discardLog()
	base := config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model"}

	tests := []struct {
		name        string
		metricsURL  string
		logsURL     string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:       "no backends -> no discovery tools",
			wantAbsent: []string{"discover_metrics", "logs_error_summary", "discover_log_fields"},
		},
		{
			name:        "metrics only -> discover_metrics present, log tools absent",
			metricsURL:  "http://metrics:9090",
			wantPresent: []string{"discover_metrics", "query_metrics"},
			wantAbsent:  []string{"logs_error_summary", "discover_log_fields"},
		},
		{
			name:        "logs only -> log discovery tools present, discover_metrics absent",
			logsURL:     "http://logs:9428",
			wantPresent: []string{"logs_error_summary", "discover_log_fields", "query_logs"},
			wantAbsent:  []string{"discover_metrics"},
		},
		{
			name:        "both -> all discovery tools present",
			metricsURL:  "http://metrics:9090",
			logsURL:     "http://logs:9428",
			wantPresent: []string{"discover_metrics", "logs_error_summary", "discover_log_fields"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Model: base}
			cfg.Metrics.URL = tc.metricsURL
			cfg.Logs.URL = tc.logsURL
			_, tools, _, _ := BuildModelAndTools(context.Background(), cfg, nil, nil, log)
			names := toolNames(tools)
			for _, w := range tc.wantPresent {
				if !names[w] {
					t.Errorf("tool %q must be present, got %v", w, names)
				}
			}
			for _, w := range tc.wantAbsent {
				if names[w] {
					t.Errorf("tool %q must be absent, got %v", w, names)
				}
			}
		})
	}
}

// TestIncidentTimelineGatedByProviders asserts the P1 wiring: incident_timeline is
// registered when at least one contributing source is wired (here the GitOps provider),
// and absent when none are (no gitops, no cloud, and — via a nonexistent KUBECONFIG — no
// cluster reader). It fans out to whichever sources are present, so a GitOps-only
// deployment still gets the fused timeline.
func TestIncidentTimelineGatedByProviders(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))
	log := discardLog()
	base := config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model"}

	// No contributing sources → absent.
	cfg := &config.Config{Model: base}
	_, tools, _, _ := BuildModelAndTools(context.Background(), cfg, nil, nil, log)
	if toolNames(tools)["incident_timeline"] {
		t.Errorf("incident_timeline must be absent with no contributing providers, got %v", toolNames(tools))
	}

	// GitOps wired → present.
	_, tools, _, _ = BuildModelAndTools(context.Background(), cfg, fakeGitOps{}, nil, log)
	if !toolNames(tools)["incident_timeline"] {
		t.Errorf("incident_timeline must be present when a GitOps provider is wired, got %v", toolNames(tools))
	}
}

// fakeClusterReader is a no-op clusterReader for the engine-gating test: it needs no
// live cluster, only to satisfy the LogReader+KubeReader interfaces so clusterTools can
// build the tool structs.
type fakeClusterReader struct{}

func (fakeClusterReader) PodLogs(context.Context, providers.PodLogQuery) (providers.LogResult, error) {
	return providers.LogResult{}, nil
}

func (fakeClusterReader) PodStatuses(context.Context, string, string) ([]providers.PodStatus, error) {
	return nil, nil
}

func (fakeClusterReader) Events(context.Context, string, string, bool) ([]providers.KubeEvent, error) {
	return nil, nil
}

// TestClusterToolsControllerLogsGatedByEngine asserts the Task-2 wiring fix:
// controller_logs is Flux-only (it enumerates the Flux controllers in flux-system), so
// it must be registered ONLY when the configured GitOps engine is Flux — including the
// empty/default engine, which resolves to flux — and absent for ArgoCD. The other
// cluster tools (pod_logs, pod_status, kube_events) are engine-agnostic and must always
// be present.
func TestClusterToolsControllerLogsGatedByEngine(t *testing.T) {
	tests := []struct {
		name   string
		engine string
		want   bool
	}{
		{"flux -> controller_logs present", "flux", true},
		{"default (empty) -> flux -> present", "", true},
		{"argocd -> controller_logs absent", "argocd", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.GitOps.Engine = tc.engine
			names := toolNames(clusterTools(fakeClusterReader{}, cfg))
			if names["controller_logs"] != tc.want {
				t.Errorf("controller_logs present=%v, want %v (engine=%q)", names["controller_logs"], tc.want, tc.engine)
			}
			// Engine-agnostic cluster tools are always registered.
			for _, always := range []string{"pod_logs", "pod_status", "kube_events"} {
				if !names[always] {
					t.Errorf("engine-agnostic tool %q must always be present, got %v", always, names)
				}
			}
		})
	}
}

// TestBuildInvestigatorSelectsImplementation asserts the central wiring decision:
// no configured model yields the read-only LogInvestigator (with a nil catalog),
// while a configured model yields the LLM ReAct LoopInvestigator. KUBECONFIG is
// pointed at a nonexistent file so the configured-model path doesn't depend on an
// ambient cluster.
func TestBuildInvestigatorSelectsImplementation(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))
	log := discardLog()

	t.Run("no model -> LogInvestigator", func(t *testing.T) {
		cfg := &config.Config{} // no model configured
		inv, cat, err := BuildInvestigator(context.Background(), cfg, nil, nil, nil, nil, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := inv.(investigate.LogInvestigator); !ok {
			t.Fatalf("want LogInvestigator, got %T", inv)
		}
		if cat != nil {
			t.Fatal("LogInvestigator path must return a nil catalog")
		}
	})

	t.Run("model -> LoopInvestigator", func(t *testing.T) {
		cfg := &config.Config{Model: config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model"}}
		inv, _, err := BuildInvestigator(context.Background(), cfg, nil, nil, nil, nil, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := inv.(*investigate.LoopInvestigator); !ok {
			t.Fatalf("want *LoopInvestigator, got %T", inv)
		}
	})

	t.Run("per-tool timeout: default when unset, explicit respected", func(t *testing.T) {
		// Unset tool_timeout (0) ⇒ the 60s default is applied at construction, mirroring
		// the other investigation defaults.
		cfg := &config.Config{Model: config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "test-model"}}
		inv, _, err := BuildInvestigator(context.Background(), cfg, nil, nil, nil, nil, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		li, ok := inv.(*investigate.LoopInvestigator)
		if !ok {
			t.Fatalf("want *LoopInvestigator, got %T", inv)
		}
		if li.ToolTimeout != defaultToolTimeout {
			t.Fatalf("unset tool_timeout must default to %v, got %v", defaultToolTimeout, li.ToolTimeout)
		}

		// Explicit tool_timeout flows through unchanged.
		cfg.Investigation.ToolTimeout = config.Duration(5 * time.Second)
		inv2, _, err := BuildInvestigator(context.Background(), cfg, nil, nil, nil, nil, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		li2 := inv2.(*investigate.LoopInvestigator)
		if li2.ToolTimeout != 5*time.Second {
			t.Fatalf("explicit tool_timeout not wired: got %v, want 5s", li2.ToolTimeout)
		}
	})
}

// TestBuildAuto asserts rung-3 wiring: nil unless action mode is "auto" AND a
// cluster executor is available. The auto-on path is only reached with a non-nil
// executor (no cluster needed — a fake suffices).
func TestBuildAuto(t *testing.T) {
	log := discardLog()

	tests := []struct {
		name    string
		mode    config.ActionMode
		exec    action.Executor
		wantNil bool
	}{
		{"off mode -> nil", config.ActionOff, fakeExecutor{}, true},
		{"approve mode -> nil", config.ActionApprove, fakeExecutor{}, true},
		{"auto mode, no executor -> nil", config.ActionAuto, nil, true},
		{"auto mode + executor -> non-nil", config.ActionAuto, fakeExecutor{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Actions.Mode = tc.mode
			got := BuildAuto(cfg, tc.exec, audit.Nop{}, log)
			if (got == nil) != tc.wantNil {
				t.Fatalf("BuildAuto nil=%v, want nil=%v", got == nil, tc.wantNil)
			}
		})
	}
}

// TestBuildApprovals asserts rung-2 wiring: non-nil only in "approve" mode with a
// cluster executor; nil otherwise.
func TestBuildApprovals(t *testing.T) {
	log := discardLog()

	tests := []struct {
		name    string
		mode    config.ActionMode
		exec    action.Executor
		wantNil bool
	}{
		{"off mode -> nil", config.ActionOff, fakeExecutor{}, true},
		{"auto mode -> nil", config.ActionAuto, fakeExecutor{}, true},
		{"approve mode, no executor -> nil", config.ActionApprove, nil, true},
		{"approve mode + executor -> non-nil", config.ActionApprove, fakeExecutor{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Actions.Mode = tc.mode
			got := BuildApprovals(cfg, tc.exec, audit.Nop{}, log)
			if (got == nil) != tc.wantNil {
				t.Fatalf("BuildApprovals nil=%v, want nil=%v", got == nil, tc.wantNil)
			}
		})
	}
}

// TestAppendMCPToolsSkipsUnreachable verifies failure-isolation: a healthy MCP server
// contributes its namespaced tools, while a broken server (500) is skipped so the
// investigation loop still starts with the healthy server's tools.
func TestAppendMCPToolsSkipsUnreachable(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		switch req.Method {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{{"name": "query", "description": "d"}}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer healthy.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }))
	defer broken.Close()

	cfg := &config.Config{MCP: config.MCP{Servers: []config.MCPServer{
		{Name: "good", Endpoint: config.Endpoint{URL: healthy.URL}},
		{Name: "bad", Endpoint: config.Endpoint{URL: broken.URL}},
	}}}
	var tools []investigate.Tool
	tools = appendMCPTools(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), tools)

	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	if len(names) != 1 || names[0] != "good__query" {
		t.Fatalf("want only good__query (bad server skipped), got %v", names)
	}
}

// TestBuildVerifyModel asserts the verify-model override wiring: nil when no
// model.verify is configured (verify then runs on the main model), non-nil when an
// override is present.
func TestBuildVerifyModel(t *testing.T) {
	noOverride := &config.Config{Model: config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "main"}}
	if got := BuildVerifyModel(noOverride); got != nil {
		t.Fatalf("BuildVerifyModel without override = %T, want nil", got)
	}

	withOverride := &config.Config{Model: config.Model{
		Provider: "openai", BaseURL: "http://vllm:8000/v1", Model: "main",
		Verify: &config.ModelOverride{Model: "cheaper"},
	}}
	if got := BuildVerifyModel(withOverride); got == nil {
		t.Fatal("BuildVerifyModel with override = nil, want a non-nil model")
	}
}

// A configured-but-unreachable metrics/logs backend must WARN loudly at startup
// (the silent-half-blind failure this guards against); a reachable one — even one
// answering 404 — must stay quiet.
func TestWarnIfBackendUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // any HTTP response ⇒ reachable
	}))
	defer srv.Close()

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	warnIfBackendUnreachable(context.Background(), log, "metrics", srv.URL)
	if strings.Contains(buf.String(), "UNREACHABLE") {
		t.Fatalf("reachable backend must not warn:\n%s", buf.String())
	}

	buf.Reset()
	warnIfBackendUnreachable(context.Background(), log, "logs", "http://127.0.0.1:1")
	if !strings.Contains(buf.String(), "UNREACHABLE") || !strings.Contains(buf.String(), "logs") {
		t.Fatalf("unreachable backend must warn with its kind:\n%s", buf.String())
	}

	// Empty URL and nil logger are no-ops (no panic).
	warnIfBackendUnreachable(context.Background(), log, "metrics", "")
	warnIfBackendUnreachable(context.Background(), nil, "metrics", "http://127.0.0.1:1")
}
