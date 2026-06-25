package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
)

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
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			model, tools, recall, cat := buildModelAndTools(context.Background(), cfg, nil, nil, log)
			if model == nil {
				t.Fatal("buildModelAndTools returned a nil model")
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

// TestRequireWebhookAuth asserts the serve-path fail-closed guard: a configured
// model with an empty webhook token must refuse to start; everything else is
// allowed. Scoped to serve only — config.Validate stays untouched so non-serve
// subcommands (e.g. `lore investigate`) with a model and no webhook still run.
func TestRequireWebhookAuth(t *testing.T) {
	// openai/vllm needs a base_url to count as configured; anthropic/gemini are
	// configured via their built-in endpoint even with an empty base_url.
	openaiModel := config.Model{Provider: "openai", BaseURL: "http://vllm:8000/v1"}
	anthropicModel := config.Model{Provider: "anthropic"} // built-in endpoint
	noModel := config.Model{}                             // unconfigured

	tests := []struct {
		name    string
		model   config.Model
		token   string
		wantErr bool
	}{
		{"model + token → ok", openaiModel, "secret", false},
		{"model + no token → refused", openaiModel, "", true},
		{"anthropic built-in + no token → refused", anthropicModel, "", true},
		{"anthropic built-in + token → ok", anthropicModel, "secret", false},
		{"no model + no token → ok (log-only)", noModel, "", false},
		{"no model + token → ok", noModel, "secret", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Model: tc.model}
			cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"
			err := requireWebhookAuth(cfg, tc.token)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireWebhookAuth err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestNewHTTPServer asserts the serving http.Server is built with every inbound
// timeout/size bound set (non-zero) — Go's defaults are zero (unlimited), the
// Slowloris/DoS gap R9(a) closes.
func TestNewHTTPServer(t *testing.T) {
	s := newHTTPServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero (unbounded slow-header read)")
	}
	if s.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero (unbounded slow-body read)")
	}
	if s.WriteTimeout == 0 {
		t.Error("WriteTimeout is zero (unbounded slow write)")
	}
	if s.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero (unbounded idle keep-alive)")
	}
	if s.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is zero (defaults to 1MB but should be explicit)")
	}
}

func TestReadyFunc(t *testing.T) {
	leaderTrue := func() bool { return true }
	leaderFalse := func() bool { return false }

	// No catalog configured → gate is pure leadership passthrough.
	if !readyFunc(leaderTrue, nil, false)() {
		t.Fatal("unconfigured + nil catalog + leader=true should be ready")
	}
	if readyFunc(leaderFalse, nil, false)() {
		t.Fatal("unconfigured + nil catalog + leader=false should not be ready")
	}

	// A catalog was CONFIGURED but failed to load (cat == nil). Never serve incident
	// traffic with no knowledge base: stay 503 even while leader. This is the bug —
	// a configured-but-failed catalog used to be indistinguishable from "unconfigured"
	// and collapsed readiness to pure leadership.
	if readyFunc(leaderTrue, nil, true)() {
		t.Fatal("configured + failed-to-load catalog (nil) must block readiness even when leader=true")
	}

	// A not-yet-warm configured catalog blocks readiness even when leader.
	cold := catalog.NewEmpty()
	if readyFunc(leaderTrue, cold, true)() {
		t.Fatal("cold catalog must block readiness even when leader=true")
	}

	// A warm catalog is ready only when also leader.
	warm, err := catalog.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !readyFunc(leaderTrue, warm, true)() {
		t.Fatal("warm catalog + leader=true should be ready")
	}
	if readyFunc(leaderFalse, warm, true)() {
		t.Fatal("warm catalog + leader=false should not be ready")
	}
}
