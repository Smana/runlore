// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// sample incident fields, mirroring a critical/prod alert in namespace apps.
const (
	sampleAlertName   = "HarborProbeFailure"
	sampleSeverity    = "critical"
	sampleEnvironment = "prod"
	sampleNamespace   = "apps"
)

func sampleLabels() map[string]string {
	return map[string]string{"team": "platform", "severity": "critical"}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name string
		tr   IncidentTrigger
		want bool
	}{
		{"empty match matches anything", IncidentTrigger{}, true},
		{"severity+env match", IncidentTrigger{Match: IncidentMatch{
			Severity: []string{"critical"}, Environment: []string{"prod"}}}, true},
		{"severity mismatch", IncidentTrigger{Match: IncidentMatch{
			Severity: []string{"warning"}}}, false},
		{"namespace glob", IncidentTrigger{Match: IncidentMatch{
			Namespaces: []string{"app*"}}}, true},
		{"namespace glob miss", IncidentTrigger{Match: IncidentMatch{
			Namespaces: []string{"payments"}}}, false},
		{"label subset match", IncidentTrigger{Match: IncidentMatch{
			Labels: map[string]string{"team": "platform"}}}, true},
		{"label mismatch", IncidentTrigger{Match: IncidentMatch{
			Labels: map[string]string{"team": "data"}}}, false},
		{"ignore excludes", IncidentTrigger{Ignore: IncidentMatch{
			AlertNames: []string{"Watchdog", "HarborProbeFailure"}}}, false},
	}
	for _, c := range cases {
		got := c.tr.MatchFields(sampleAlertName, sampleSeverity, sampleEnvironment, sampleNamespace, sampleLabels())
		if got != c.want {
			t.Errorf("%s: MatchFields=%v want %v", c.name, got, c.want)
		}
	}
}

// TestMatchSeverityCaseInsensitive guards against the "RunLore went deaf" failure:
// Alertmanager severity labels arrive with arbitrary casing (Critical, CRITICAL),
// so a policy configured with lowercase `critical` must still match. This also keeps
// the trigger consistent with the coalescer, which fast-paths via EqualFold("critical").
func TestMatchSeverityCaseInsensitive(t *testing.T) {
	tr := IncidentTrigger{Match: IncidentMatch{Severity: []string{"critical"}}}
	for _, alertSeverity := range []string{"critical", "Critical", "CRITICAL"} {
		got := tr.MatchFields(sampleAlertName, alertSeverity, sampleEnvironment, sampleNamespace, sampleLabels())
		if !got {
			t.Errorf("severity %q: MatchFields=false, want true (case-insensitive)", alertSeverity)
		}
	}
	// A genuine mismatch must still be rejected regardless of casing.
	if tr.MatchFields(sampleAlertName, "Warning", sampleEnvironment, sampleNamespace, sampleLabels()) {
		t.Errorf("severity %q should not match policy %q", "Warning", "critical")
	}
	// Casing in the policy itself must not matter either.
	if !(IncidentTrigger{Match: IncidentMatch{Severity: []string{"CRITICAL"}}}).
		MatchFields(sampleAlertName, "critical", sampleEnvironment, sampleNamespace, sampleLabels()) {
		t.Errorf("policy %q should match alert severity %q", "CRITICAL", "critical")
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	if err := d.UnmarshalYAML(yamlScalar("30m")); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Std() != 30*time.Minute {
		t.Fatalf("got %v want 30m", d.Std())
	}
}

func yamlScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
}

func TestInstantRecallTrustConfig(t *testing.T) {
	const y = `
catalog:
  instant_recall:
    enabled: true
    min_score: 1.5
    margin_gap: 1.0
    solo_floor: 4.0
    require_workload_match: false
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ir := c.Catalog.InstantRecall
	if !ir.Enabled || ir.MinScore != 1.5 || ir.MarginGap != 1.0 || ir.SoloFloor != 4.0 || ir.RequireWorkloadMatch {
		t.Fatalf("instant_recall not parsed: %+v", ir)
	}
}

func TestInstantRecallStaleAfterParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("catalog:\n  instant_recall:\n    enabled: true\n    stale_after: 720h\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := c.Catalog.InstantRecall.StaleAfter.Std(); got != 720*time.Hour {
		t.Fatalf("instant_recall.stale_after: want 720h, got %v", got)
	}
	// Absent ⇒ zero ⇒ age down-weighting disabled (recall honours 0 as off).
	var z Config
	if err := yaml.Unmarshal([]byte("catalog:\n  instant_recall:\n    enabled: true\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Catalog.InstantRecall.StaleAfter.Std() != 0 {
		t.Fatalf("absent instant_recall.stale_after must be 0, got %v", z.Catalog.InstantRecall.StaleAfter.Std())
	}
}

func TestCurateStaleAfterParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  stale_after: 720h\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := c.Curate.StaleAfter.Std(); got != 720*time.Hour {
		t.Fatalf("curate.stale_after: want 720h, got %v", got)
	}
	// Absent ⇒ zero ⇒ the lifecycle sweep is disabled (runCurate honours 0).
	var z Config
	if err := yaml.Unmarshal([]byte("forge:\n  kb_repo: o/r\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Curate.StaleAfter.Std() != 0 {
		t.Fatalf("absent stale_after must be 0, got %v", z.Curate.StaleAfter.Std())
	}
}

// TestValidateModelDoesNotRequireWebhookToken guards the R9(c) scoping decision:
// the alert-webhook auth requirement lives on the serve path, NOT in Validate.
// Validate is shared by every subcommand, so a model-configured config with no
// webhook token must still validate clean — otherwise `lore investigate` (which
// requires a model and has no webhook) would break.
func TestValidateModelDoesNotRequireWebhookToken(t *testing.T) {
	c := &Config{Model: Model{Provider: "anthropic"}} // model set, no webhook, actions off
	if err := c.Validate(); err != nil {
		t.Fatalf("model-only config must validate clean (webhook auth is serve-scoped): %v", err)
	}
}

// TestModelMaxTokensParse verifies model.max_tokens parses to Model.MaxTokens, an
// unset key reads as 0 (the "use the default" sentinel), and the verify override
// carries its own max_tokens (0 ⇒ inherit the parent's effective value).
func TestModelMaxTokensParse(t *testing.T) {
	const y = `
model:
  provider: anthropic
  model: claude-x
  max_tokens: 16384
  verify:
    model: claude-cheap
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Model.MaxTokens != 16384 {
		t.Fatalf("model.max_tokens: want 16384, got %d", c.Model.MaxTokens)
	}
	// A verify block with no max_tokens leaves the override at 0 (inherit the parent).
	if c.Model.Verify == nil || c.Model.Verify.MaxTokens != 0 {
		t.Fatalf("verify.max_tokens absent must be 0, got %+v", c.Model.Verify)
	}

	// Absent ⇒ zero ⇒ the wiring applies the 8192 default.
	var z Config
	if err := yaml.Unmarshal([]byte("model:\n  provider: openai\n  model: x\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Model.MaxTokens != 0 {
		t.Fatalf("absent max_tokens must be 0, got %d", z.Model.MaxTokens)
	}

	// An explicit verify.max_tokens overrides the parent.
	const yv = `
model:
  provider: anthropic
  model: claude-x
  max_tokens: 16384
  verify:
    model: claude-cheap
    max_tokens: 2048
`
	var cv Config
	if err := yaml.Unmarshal([]byte(yv), &cv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cv.Model.Verify == nil || cv.Model.Verify.MaxTokens != 2048 {
		t.Fatalf("verify.max_tokens override: want 2048, got %+v", cv.Model.Verify)
	}
}

// TestValidateRejectsNegativeMaxTokens verifies a negative model.max_tokens (or a
// negative verify or chat override) is rejected by Validate — a nonsensical value
// that would otherwise reach a provider request.
func TestValidateRejectsNegativeMaxTokens(t *testing.T) {
	c := &Config{Model: Model{Provider: "anthropic", MaxTokens: -1}}
	if err := c.Validate(); err == nil {
		t.Fatal("negative model.max_tokens must be rejected by Validate")
	}
	cv := &Config{Model: Model{Provider: "anthropic", Verify: &ModelOverride{MaxTokens: -5}}}
	if err := cv.Validate(); err == nil {
		t.Fatal("negative verify.max_tokens must be rejected by Validate")
	}
	// The chat override is the third sibling and used to be the exception: a
	// negative fell straight through chatMaxTokens to the 1024 default. This field
	// sizes BOTH the provider's output cap and the conservative charge the hourly
	// token budget applies when a provider reports no usage, so a value that
	// reaches neither is not something an operator should discover from a bill.
	cc := &Config{Model: Model{Provider: "anthropic", Chat: &ModelOverride{Model: "small-cheap-model", MaxTokens: -5}}}
	if err := cc.Validate(); err == nil {
		t.Fatal("negative model.chat.max_tokens must be rejected by Validate, like its two siblings")
	}
	// Zero and positive are fine.
	ok := &Config{Model: Model{Provider: "anthropic", MaxTokens: 0,
		Verify: &ModelOverride{MaxTokens: 4096},
		Chat:   &ModelOverride{Model: "small-cheap-model", MaxTokens: 2048}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("non-negative max_tokens must validate clean: %v", err)
	}
}

// TestValidateCompactionMode locks the compaction knob: empty (default), "elide",
// and "summarize" validate clean; anything else is rejected at startup rather than
// silently defaulting a typo to lossy elision.
func TestValidateCompactionMode(t *testing.T) {
	for _, mode := range []string{"", "elide", "summarize"} {
		c := &Config{Model: Model{Provider: "anthropic"}, Investigation: Investigation{Compaction: mode}}
		if err := c.Validate(); err != nil {
			t.Fatalf("compaction %q must validate clean: %v", mode, err)
		}
	}
	bad := &Config{Model: Model{Provider: "anthropic"}, Investigation: Investigation{Compaction: "summarise"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("an unknown compaction mode must be rejected by Validate")
	}
}

// TestValidateEffort locks in the per-provider effort vocabulary: anthropic
// low|medium|high|max, openai (and any OpenAI-compatible/unknown provider)
// minimal|low|medium|high, gemini rejected outright, and empty always fine
// (effort is opt-in — unset keeps today's requests unchanged). The verify
// override validates against its EFFECTIVE provider and effort (inherit-when-
// empty, mirroring BuildVerifyModel).
func TestValidateThinking(t *testing.T) {
	cases := []struct {
		name    string
		model   Model
		wantErr string // "" = must validate clean; otherwise a substring of the error
	}{
		{"empty thinking is fine", Model{Provider: "anthropic"}, ""},
		{"anthropic adaptive", Model{Provider: "anthropic", Thinking: "adaptive"}, ""},
		{"anthropic rejects enabled", Model{Provider: "anthropic", Thinking: "enabled"}, "not a valid thinking mode"},
		{"anthropic rejects on", Model{Provider: "anthropic", Thinking: "on"}, "model.thinking"},
		{"openai rejects thinking", Model{Provider: "openai", Thinking: "adaptive"}, "only supported for provider anthropic"},
		{"empty provider rejects thinking", Model{Thinking: "adaptive"}, "only supported for provider anthropic"},
		{"gemini rejects thinking", Model{Provider: "gemini", Thinking: "adaptive"}, "only supported for provider anthropic"},
		{
			"verify override inherits the parent thinking and provider",
			Model{Provider: "anthropic", Thinking: "adaptive", Verify: &ModelOverride{Model: "cheap"}},
			"",
		},
		{
			"verify override thinking validated",
			Model{Provider: "anthropic", Verify: &ModelOverride{Thinking: "enabled"}},
			"model.verify.thinking",
		},
		{
			"inherited parent thinking invalid for the override provider",
			Model{Provider: "anthropic", Thinking: "adaptive", Verify: &ModelOverride{Provider: "openai"}},
			"model.verify.thinking",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Model: tc.model}
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateEffort(t *testing.T) {
	cases := []struct {
		name    string
		model   Model
		wantErr string // "" = must validate clean; otherwise a substring of the error
	}{
		{"empty effort is fine", Model{Provider: "anthropic"}, ""},
		{"anthropic low", Model{Provider: "anthropic", Effort: "low"}, ""},
		{"anthropic medium", Model{Provider: "anthropic", Effort: "medium"}, ""},
		{"anthropic high", Model{Provider: "anthropic", Effort: "high"}, ""},
		{"anthropic max", Model{Provider: "anthropic", Effort: "max"}, ""},
		{"anthropic rejects minimal", Model{Provider: "anthropic", Effort: "minimal"}, "model.effort"},
		{"openai minimal", Model{Provider: "openai", Effort: "minimal"}, ""},
		{"openai high", Model{Provider: "openai", Effort: "high"}, ""},
		{"openai rejects max", Model{Provider: "openai", Effort: "max"}, "model.effort"},
		{"empty provider defaults to openai vocabulary", Model{Effort: "minimal"}, ""},
		{"unknown provider uses the openai vocabulary", Model{Provider: "vllm", Effort: "low"}, ""},
		{"gemini rejects effort", Model{Provider: "gemini", Effort: "low"}, "not supported for provider gemini"},
		{"gemini without effort is fine", Model{Provider: "gemini"}, ""},
		{
			"verify override inherits the parent effort and provider",
			Model{Provider: "anthropic", Effort: "max", Verify: &ModelOverride{Model: "cheap"}},
			"",
		},
		{
			"verify override effort validated against its own vocabulary",
			Model{Provider: "anthropic", Verify: &ModelOverride{Effort: "minimal"}},
			"model.verify.effort",
		},
		{
			"inherited parent effort invalid for the override provider",
			Model{Provider: "anthropic", Effort: "max", Verify: &ModelOverride{Provider: "openai"}},
			"model.verify.effort",
		},
		{
			"verify override to gemini rejects an inherited effort",
			Model{Provider: "anthropic", Effort: "high", Verify: &ModelOverride{Provider: "gemini"}},
			"not supported for provider gemini",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Model: tc.model}
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateRejectsCleartextKeyOnPublicHost(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		apiKeyEnv string
		wantErr   bool
	}{
		{"http public + key", "http://api.openai.com/v1", "OPENAI_API_KEY", true},
		{"https public + key", "https://api.openai.com/v1", "OPENAI_API_KEY", false},
		{"http private IP + key", "http://10.0.0.5:8000/v1", "K", false},
		{"http localhost + key", "http://localhost:8000/v1", "K", false},
		{"http single-label + key", "http://vllm:8000/v1", "K", false},
		{"http .svc + key", "http://vllm.ai.svc.cluster.local/v1", "K", false},
		{"http .svc only + key", "http://vllm.ns.svc:8000/v1", "K", false},
		{"http public no key", "http://api.openai.com/v1", "", false},
		{"empty base_url + key", "", "OPENAI_API_KEY", false},
		{"unparseable + key", "http://%zz/v1", "K", true},
		{"ftp scheme + key", "ftp://api.openai.com/v1", "K", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Model: Model{Provider: "openai", BaseURL: tc.baseURL, APIKeyEnv: tc.apiKeyEnv}}
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("base_url %q + key %q must be rejected", tc.baseURL, tc.apiKeyEnv)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("base_url %q + key %q must validate clean, got %v", tc.baseURL, tc.apiKeyEnv, err)
			}
		})
	}
}

func TestValidateCleartextKeyCoversVerifyAndEmbeddings(t *testing.T) {
	// Verify override with its OWN http public base_url + own key.
	cv := &Config{Model: Model{Provider: "anthropic",
		Verify: &ModelOverride{BaseURL: "http://api.cheap.example/v1", APIKeyEnv: "CHEAP_KEY"}}}
	if err := cv.Validate(); err == nil {
		t.Fatal("verify override with http public base_url + key must be rejected")
	}
	// Verify override with its own http public base_url but INHERITING the parent key.
	ci := &Config{Model: Model{Provider: "anthropic", APIKeyEnv: "PARENT_KEY",
		Verify: &ModelOverride{BaseURL: "http://api.cheap.example/v1"}}}
	if err := ci.Validate(); err == nil {
		t.Fatal("verify override over http public, inheriting the parent key, must be rejected")
	}
	// Keyless parent with http public base_url + verify override that supplies its OWN key
	// and inherits the parent's insecure base_url. This was the fail-open bug: the parent
	// check passes (no key), the old verify check was gated on v.BaseURL != "" so it was
	// also skipped. The effective resolved endpoint (http://api.public.example/v1 + VERIFY_KEY)
	// must be caught.
	ck := &Config{Model: Model{Provider: "openai", BaseURL: "http://api.public.example/v1",
		Verify: &ModelOverride{APIKeyEnv: "VERIFY_KEY"}}}
	if err := ck.Validate(); err == nil {
		t.Fatal("keyless parent over http public + verify with own key (inheriting base_url) must be rejected")
	}
	// Same as above but the parent uses https — the inherited base is safe, must validate clean.
	cks := &Config{Model: Model{Provider: "openai", BaseURL: "https://api.public.example/v1",
		Verify: &ModelOverride{APIKeyEnv: "VERIFY_KEY"}}}
	if err := cks.Validate(); err != nil {
		t.Fatalf("keyless parent over https + verify with own key must validate clean, got %v", err)
	}
	// Embeddings with http public base_url + key.
	ce := &Config{Model: Model{Provider: "anthropic",
		Embeddings: &Embeddings{BaseURL: "http://emb.example/v1", APIKeyEnv: "EMB_KEY"}}}
	if err := ce.Validate(); err == nil {
		t.Fatal("embeddings with http public base_url + key must be rejected")
	}
	// All-https equivalents validate clean.
	ok := &Config{Model: Model{Provider: "anthropic", APIKeyEnv: "PARENT_KEY",
		Verify:     &ModelOverride{BaseURL: "https://api.cheap.example/v1"},
		Embeddings: &Embeddings{BaseURL: "https://emb.example/v1", APIKeyEnv: "EMB_KEY"}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("https verify+embeddings must validate clean, got %v", err)
	}
}

func TestCurateRecurrenceThresholdParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  recurrence_threshold: 5\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Curate.RecurrenceThreshold != 5 {
		t.Fatalf("recurrence_threshold: want 5, got %d", c.Curate.RecurrenceThreshold)
	}
	// Absent ⇒ zero ⇒ the pass applies its own default (3).
	var z Config
	if err := yaml.Unmarshal([]byte("curate:\n  stale_after: 240h\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Curate.RecurrenceThreshold != 0 {
		t.Fatalf("absent recurrence_threshold must be 0, got %d", z.Curate.RecurrenceThreshold)
	}
}

func TestValidateMCPServers(t *testing.T) {
	good := &Config{MCP: MCP{Servers: []MCPServer{
		{Name: "steampipe", Endpoint: Endpoint{URL: "https://mcp.example/x"}},
		{Name: "k8s", Endpoint: Endpoint{URL: "http://k8s-mcp.ai.svc:8080"}},
	}}}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid MCP servers must pass: %v", err)
	}
	for _, tc := range []struct {
		name string
		s    MCPServer
	}{
		{"missing name", MCPServer{Endpoint: Endpoint{URL: "https://x"}}},
		{"missing url", MCPServer{Name: "a"}},
		{"double underscore in name", MCPServer{Name: "a__b", Endpoint: Endpoint{URL: "https://x"}}},
		{"cleartext token on public http", MCPServer{Name: "a", Endpoint: Endpoint{URL: "http://api.public.example/x", TokenEnv: "T"}}},
		{"non-http scheme ws", MCPServer{Name: "a", Endpoint: Endpoint{URL: "ws://x"}}},
	} {
		c := &Config{MCP: MCP{Servers: []MCPServer{tc.s}}}
		if err := c.Validate(); err == nil {
			t.Fatalf("%s must be rejected", tc.name)
		}
	}
	dup := &Config{MCP: MCP{Servers: []MCPServer{{Name: "a", Endpoint: Endpoint{URL: "https://x"}}, {Name: "a", Endpoint: Endpoint{URL: "https://y"}}}}}
	if err := dup.Validate(); err == nil {
		t.Fatal("duplicate server names must be rejected")
	}
}

func TestValidateMCPToolAllowlist(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.MCP.Servers = []MCPServer{{Name: "kb", Endpoint: Endpoint{URL: "https://mcp.example/mcp"}}}
		return c
	}
	t.Run("empty tool name rejected", func(t *testing.T) {
		c := base()
		c.MCP.Servers[0].Tools = []string{""}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mcp.servers[kb].tools") {
			t.Fatalf("want tools validation error, got %v", err)
		}
	})
	t.Run("whitespace tool name rejected", func(t *testing.T) {
		c := base()
		c.MCP.Servers[0].Tools = []string{"a b"}
		if err := c.Validate(); err == nil {
			t.Fatal("want error for whitespace tool name")
		}
	})
	t.Run("duplicate tool name rejected", func(t *testing.T) {
		c := base()
		c.MCP.Servers[0].Tools = []string{"query", "query"}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("want duplicate error, got %v", err)
		}
	})
	t.Run("require_allowlist without tools fails closed", func(t *testing.T) {
		c := base()
		c.MCP.RequireAllowlist = true
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "require_allowlist") {
			t.Fatalf("want require_allowlist error, got %v", err)
		}
	})
	t.Run("require_allowlist with tools passes", func(t *testing.T) {
		c := base()
		c.MCP.RequireAllowlist = true
		c.MCP.Servers[0].Tools = []string{"query"}
		if err := c.Validate(); err != nil {
			t.Fatalf("valid allowlisted config rejected: %v", err)
		}
	})
}

// TestValidateApproveRequiresAuditLog asserts approve mode is held to the same
// audit requirement as auto: an executing rung that mutates the cluster must have
// an audit_log_path (so the hash chain is verified fail-closed on open). Without
// it, approve would silently fall back to a Nop auditor.
func TestValidateApproveRequiresAuditLog(t *testing.T) {
	// approve with the token but NO audit_log_path → rejected.
	missing := &Config{}
	missing.Actions.Mode = ActionApprove
	missing.Actions.ApprovalTokenEnv = "RUNLORE_APPROVAL_TOKEN"
	if err := missing.Validate(); err == nil {
		t.Fatal("approve without actions.audit_log_path must be rejected")
	}

	// approve WITH both the token and an audit_log_path → validates.
	ok := &Config{}
	ok.Actions.Mode = ActionApprove
	ok.Actions.ApprovalTokenEnv = "RUNLORE_APPROVAL_TOKEN"
	ok.Actions.AuditLogPath = "/var/lib/runlore/audit.jsonl"
	if err := ok.Validate(); err != nil {
		t.Fatalf("approve with token + audit_log_path must validate clean, got: %v", err)
	}
}

// TestForgeSkipVerdictsParse verifies forge.skip_verdicts parses into the string
// list and that an absent key reads as nil (empty ⇒ draft every verdict, the
// backward-compatible default).
func TestForgeSkipVerdictsParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("forge:\n  kb_repo: o/r\n  skip_verdicts: [no_action, inconclusive]\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := c.Forge.SkipVerdicts; len(got) != 2 || got[0] != "no_action" || got[1] != "inconclusive" {
		t.Fatalf("forge.skip_verdicts: want [no_action inconclusive], got %v", got)
	}
	// Absent ⇒ nil ⇒ every verdict is eligible to draft a PR (pre-gate behaviour).
	var z Config
	if err := yaml.Unmarshal([]byte("forge:\n  kb_repo: o/r\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Forge.SkipVerdicts != nil {
		t.Fatalf("absent skip_verdicts must be nil, got %v", z.Forge.SkipVerdicts)
	}
}

// TestValidateSkipVerdicts guards the verdict gate's config surface: known verdict
// values validate clean, an unknown value is rejected at startup (rather than
// silently ignoring a typo that would leave benign PRs flowing), and an empty list
// validates clean.
func TestValidateSkipVerdicts(t *testing.T) {
	ok := &Config{}
	ok.Forge.SkipVerdicts = []string{"no_action", "action_suggested", "action_required", "inconclusive"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("all known verdicts must validate clean, got: %v", err)
	}

	empty := &Config{}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty skip_verdicts must validate clean, got: %v", err)
	}

	bad := &Config{}
	bad.Forge.SkipVerdicts = []string{"no_action", "definitely_not_a_verdict"}
	if err := bad.Validate(); err == nil {
		t.Fatal("an unknown verdict in forge.skip_verdicts must be rejected by Validate")
	}
}

// TestValidateFeedbackButtons guards the opt-in contract of the Slack feedback
// loop end to end: enabling notify.slack.feedback_buttons requires the signing
// secret (clicks arrive on the exposed /slack/interactions endpoint and must be
// signature-verified), the outcome ledger (a rendered button whose click cannot
// be recorded would be a lie) AND a delivery target — a button only exists on a
// message the Slack notifier actually delivered, and that notifier is only built
// from an incoming webhook or a bot token + channel. Both are genuine targets:
// Slack dispatches block_actions for interactive components posted either way,
// and the click is answered via the payload's response_url, which needs no bot
// token. Off (the default) validates clean with none of it.
func TestValidateFeedbackButtons(t *testing.T) {
	const (
		secretEnv  = "SLACK_SIGNING_SECRET"
		ledgerPath = "/var/lib/runlore/outcomes.jsonl"
	)
	// on returns a config with feedback_buttons and everything BUT the delivery
	// target, so each case only spells out the target combination under test.
	on := func() *Config {
		c := &Config{}
		c.Notify.Slack.FeedbackButtons = true
		c.Notify.Slack.SigningSecretEnv = secretEnv
		c.Outcome.LedgerPath = ledgerPath
		return c
	}
	tests := []struct {
		name    string
		cfg     func() *Config
		wantErr string // substring the rejection must name; "" ⇒ must validate clean
	}{
		{name: "off by default", cfg: func() *Config { return &Config{} }},
		{
			name: "on without the signing secret",
			cfg: func() *Config {
				c := on()
				c.Notify.Slack.SigningSecretEnv = ""
				c.Notify.Slack.WebhookURLEnv = "SLACK_WEBHOOK_URL"
				return c
			},
			wantErr: "notify.slack.signing_secret_env",
		},
		{
			name: "on without the outcome ledger",
			cfg: func() *Config {
				c := on()
				c.Outcome.LedgerPath = ""
				c.Notify.Slack.WebhookURLEnv = "SLACK_WEBHOOK_URL"
				return c
			},
			wantErr: "outcome.ledger_path",
		},
		{
			name:    "on with no delivery target at all",
			cfg:     on,
			wantErr: "notify.slack.webhook_url_env",
		},
		{
			name: "on with a bot token but no channel",
			cfg: func() *Config {
				c := on()
				c.Notify.Slack.BotTokenEnv = "SLACK_BOT_TOKEN"
				return c
			},
			wantErr: "notify.slack.channel",
		},
		{
			name: "on with an incoming webhook",
			cfg: func() *Config {
				c := on()
				c.Notify.Slack.WebhookURLEnv = "SLACK_WEBHOOK_URL"
				return c
			},
		},
		{
			name: "on with a bot token and channel",
			cfg: func() *Config {
				c := on()
				c.Notify.Slack.BotTokenEnv = "SLACK_BOT_TOKEN"
				c.Notify.Slack.Channel = "#alerts"
				return c
			},
		},
		{
			name: "on with both targets",
			cfg: func() *Config {
				c := on()
				c.Notify.Slack.WebhookURLEnv = "SLACK_WEBHOOK_URL"
				c.Notify.Slack.BotTokenEnv = "SLACK_BOT_TOKEN"
				c.Notify.Slack.Channel = "#alerts"
				return c
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg().Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("must validate clean, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("must be rejected (error naming %q), got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error must name the missing key %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidateRecurrenceCooldown: the suppression gate reads the outcome
// ledger's trigger index, so a cooldown without a ledger would silently never
// suppress — rejected at startup. Negative durations are misconfigurations; 0
// (off) and a properly-backed positive value validate clean.
func TestValidateRecurrenceCooldown(t *testing.T) {
	off := &Config{}
	if err := off.Validate(); err != nil {
		t.Fatalf("cooldown off must validate clean, got: %v", err)
	}

	noLedger := &Config{}
	noLedger.Investigation.RecurrenceCooldown = Duration(30 * time.Minute)
	if err := noLedger.Validate(); err == nil {
		t.Fatal("recurrence_cooldown without outcome.ledger_path must be rejected")
	}

	neg := &Config{}
	neg.Investigation.RecurrenceCooldown = Duration(-time.Minute)
	neg.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
	if err := neg.Validate(); err == nil {
		t.Fatal("a negative recurrence_cooldown must be rejected")
	}

	ok := &Config{}
	ok.Investigation.RecurrenceCooldown = Duration(30 * time.Minute)
	ok.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
	if err := ok.Validate(); err != nil {
		t.Fatalf("cooldown with a ledger must validate clean, got: %v", err)
	}
}

// TestValidateMatrixFeedbackReactions guards the Matrix feedback opt-in: the
// reaction listener syncs the configured room and records into the ledger, so
// enabling it without the notifier fields or without a ledger would silently
// listen to nothing / record nowhere — rejected at startup instead.
func TestValidateMatrixFeedbackReactions(t *testing.T) {
	off := &Config{}
	if err := off.Validate(); err != nil {
		t.Fatalf("feedback_reactions off must validate clean, got: %v", err)
	}

	noMatrix := &Config{}
	noMatrix.Notify.Matrix.FeedbackReactions = true
	noMatrix.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
	if err := noMatrix.Validate(); err == nil {
		t.Fatal("feedback_reactions without the matrix notifier config must be rejected")
	}

	noLedger := &Config{}
	noLedger.Notify.Matrix.FeedbackReactions = true
	noLedger.Notify.Matrix.Homeserver = "https://matrix.example.org"
	noLedger.Notify.Matrix.RoomID = "!r:example.org"
	noLedger.Notify.Matrix.AccessTokenEnv = "MATRIX_TOKEN"
	if err := noLedger.Validate(); err == nil {
		t.Fatal("feedback_reactions without outcome.ledger_path must be rejected")
	}

	ok := &Config{}
	ok.Notify.Matrix.FeedbackReactions = true
	ok.Notify.Matrix.Homeserver = "https://matrix.example.org"
	ok.Notify.Matrix.RoomID = "!r:example.org"
	ok.Notify.Matrix.AccessTokenEnv = "MATRIX_TOKEN"
	ok.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
	if err := ok.Validate(); err != nil {
		t.Fatalf("feedback_reactions fully configured must validate clean, got: %v", err)
	}
}

// TestValidateMatrixThreadCapture guards the opt-in contract of
// notify.matrix.thread_capture: it requires the same notifier fields as
// feedback_reactions (homeserver, room_id, access_token_env — the listener
// long-polls the configured room and authenticates as the bot) plus
// outcome.ledger_path, because the thread registry lives beside the ledger
// and must survive a restart and a leader failover. Off (the default)
// validates clean with none of it.
func TestValidateMatrixThreadCapture(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Notify.Matrix = MatrixNotify{
			Homeserver:     "https://matrix.example.org",
			RoomID:         "!r:example.org",
			AccessTokenEnv: "MATRIX_TOKEN",
			ThreadCapture:  true,
		}
		c.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"off needs nothing", func(c *Config) {
			c.Notify.Matrix = MatrixNotify{ThreadCapture: false}
		}, ""},
		{"missing homeserver", func(c *Config) {
			c.Notify.Matrix.Homeserver = ""
		}, "homeserver"},
		{"missing room_id", func(c *Config) {
			c.Notify.Matrix.RoomID = ""
		}, "room_id"},
		{"missing access_token_env", func(c *Config) {
			c.Notify.Matrix.AccessTokenEnv = ""
		}, "access_token_env"},
		{"missing ledger path", func(c *Config) {
			c.Outcome.LedgerPath = ""
		}, "ledger_path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate() = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestGitOpsMirrorConfig covers the gitops.mirror block: zero-value defaults to
// enabled (the Rerank *bool idiom), an explicit false disables, and a negative
// max is rejected by Validate.
func TestGitOpsMirrorConfig(t *testing.T) {
	var m GitOpsMirror
	if !m.IsEnabled() {
		t.Fatal("zero-value mirror config must default to enabled")
	}
	off := false
	m.Enabled = &off
	if m.IsEnabled() {
		t.Fatal("enabled:false must disable")
	}
	c := &Config{Model: Model{Provider: "anthropic"}} // minimal valid config
	c.GitOps.Mirror.Max = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative gitops.mirror.max must fail validation")
	}
}

func TestVectorCacheConfigDefaults(t *testing.T) {
	var vc VectorCache
	if !vc.IsEnabled() {
		t.Error("zero-value vector_cache must be enabled (persistence only ever helps)")
	}
	off := false
	vc = VectorCache{Enabled: &off}
	if vc.IsEnabled() {
		t.Error("explicit enabled:false must disable")
	}
}

// TestLogsProviderValidate: logs.provider is an enum — "" (auto-detect),
// "victorialogs", "loki". Anything else must abort startup loudly (the Load
// philosophy: a typo'd key never fails silently), because a silent fallback to
// victorialogs against a Loki endpoint would break every logs tool at runtime.
func TestLogsProviderValidate(t *testing.T) {
	for _, ok := range []string{"", "victorialogs", "loki", "elasticsearch", "opensearch"} {
		c := &Config{}
		c.Logs.Provider = ok
		if err := c.Validate(); err != nil {
			t.Fatalf("provider %q must validate, got %v", ok, err)
		}
	}
	c := &Config{}
	c.Logs.Provider = "grafana"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "logs.provider") {
		t.Fatalf("unknown provider must fail with a logs.provider error, got %v", err)
	}
}

// TestValidateForgeProviderGitLab pins the fail-closed contract for the GitLab
// forge provider: a missing token_env, and an obviously-invalid kb_repo, both
// abort Validate rather than let `serve` come up with curation silently
// disabled — the exact failure mode a self-hosted GitLab team would otherwise
// discover only when no PR ever appears.
func TestValidateForgeProviderGitLab(t *testing.T) {
	ok := &Config{}
	ok.Forge.Provider = "gitlab"
	ok.Forge.KBRepo = "my-group/runlore-kb"
	ok.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
	if err := ok.Validate(); err != nil {
		t.Fatalf("a well-formed gitlab forge config must validate clean, got: %v", err)
	}

	nested := &Config{}
	nested.Forge.Provider = "gitlab"
	nested.Forge.KBRepo = "my-group/subgroup/runlore-kb"
	nested.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
	if err := nested.Validate(); err != nil {
		t.Fatalf("a nested-group kb_repo must validate clean, got: %v", err)
	}

	noToken := &Config{}
	noToken.Forge.Provider = "gitlab"
	noToken.Forge.KBRepo = "my-group/runlore-kb"
	if err := noToken.Validate(); err == nil || !strings.Contains(err.Error(), "forge.gitlab.token_env") {
		t.Fatalf("gitlab provider with no token_env must be rejected, got: %v", err)
	}

	badRepo := &Config{}
	badRepo.Forge.Provider = "gitlab"
	badRepo.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
	badRepo.Forge.KBRepo = "not-a-project-path"
	if err := badRepo.Validate(); err == nil || !strings.Contains(err.Error(), "forge.kb_repo") {
		t.Fatalf("kb_repo without a namespace must be rejected, got: %v", err)
	}

	unknown := &Config{}
	unknown.Forge.Provider = "bitbucket"
	if err := unknown.Validate(); err == nil || !strings.Contains(err.Error(), "forge.provider") {
		t.Fatalf("an unknown forge.provider must be rejected, got: %v", err)
	}

	// Backward compatibility: an absent (empty) provider is "github" and
	// requires none of the gitlab fields.
	def := &Config{}
	if err := def.Validate(); err != nil {
		t.Fatalf("an empty forge.provider must validate clean (defaults to github), got: %v", err)
	}
}

// TestValidateForgeGitHost pins the startup contract for the host RunLore's
// forge credential may be cloned with.
//
// The credential is confined to that ONE host (a GitOps repoURL is cluster
// state, so anyone who can create an Argo CD Application would otherwise pick
// where the token goes). Confining is only safe if the host is KNOWN, and there
// is exactly one shape where it is not: GitHub Enterprise with subdomain
// isolation serves the API from api.HOSTNAME and git from HOSTNAME, so the API
// URL cannot answer the question. Guessing api.HOSTNAME would withhold the
// credential from every GitOps repo — the silent data gap of RunLore #495,
// traded for a leak. So that shape is refused at config load, loudly, until the
// operator names the git host.
func TestValidateForgeGitHost(t *testing.T) {
	t.Run("api-subdomain forge api url is refused without git_host", func(t *testing.T) {
		c := &Config{}
		c.Forge.GitHubAPIURL = "https://api.ghe.example.com"
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), "forge.git_host") {
			t.Fatalf("a subdomain-isolated GHE api url must fail load naming forge.git_host, got: %v", err)
		}
	})

	t.Run("naming the git host resolves it", func(t *testing.T) {
		c := &Config{}
		c.Forge.GitHubAPIURL = "https://api.ghe.example.com"
		c.Forge.GitHost = "ghe.example.com"
		if err := c.Validate(); err != nil {
			t.Fatalf("an explicit git host must validate clean, got: %v", err)
		}
	})

	// Every unambiguous shape keeps working with no new config: this must not
	// become a key every operator has to learn.
	for _, apiURL := range []string{
		"",                                // github.com
		"https://api.github.com",          // github.com, spelled out
		"https://ghe.example.com/api/v3",  // GHE without subdomain isolation
		"https://api-gateway.example.com", // "api" is a label PREFIX here, not a label
		"not a url",
	} {
		t.Run("unambiguous api url "+apiURL, func(t *testing.T) {
			c := &Config{}
			c.Forge.GitHubAPIURL = apiURL
			if err := c.Validate(); err != nil {
				t.Fatalf("github_api_url %q must validate clean without forge.git_host, got: %v", apiURL, err)
			}
		})
	}

	// git_host is compared against a clone URL's host, which arrives already
	// reduced to a bare hostname. Anything that is not one would never match, so
	// it would withhold the credential from every clone in silence.
	for _, bad := range []string{
		"https://ghe.example.com", // a URL, not a host
		"ghe.example.com/api/v3",  // a path
		"ghe.example.com:8443",    // a port (a clone URL's host is compared portless)
		"git@ghe.example.com",     // userinfo
		"ghe example.com",         // whitespace
		"   ",                     // blank
		"gİthub.com",              // non-ASCII: ToLower and IDNA disagree about which host this is
	} {
		t.Run("rejected git_host "+bad, func(t *testing.T) {
			c := &Config{}
			c.Forge.GitHost = bad
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), "forge.git_host") {
				t.Fatalf("forge.git_host %q must be refused at load, got: %v", bad, err)
			}
		})
	}

	// A GitLab forge derives its git host from base_url and needs no override,
	// but may still set one.
	t.Run("gitlab needs no git_host", func(t *testing.T) {
		c := &Config{}
		c.Forge.Provider = "gitlab"
		c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
		c.Forge.GitLab.BaseURL = "https://gitlab.example.com"
		if err := c.Validate(); err != nil {
			t.Fatalf("a gitlab forge must validate clean without forge.git_host, got: %v", err)
		}
	})
}

// TestValidateRefusesANonASCIIDerivedForgeHost is the contract the rejected
// git_host cases above already had, applied to the keys that ACTUALLY produce
// the host in most deployments.
//
// forge.git_host has been refused for non-ASCII since the confinement shipped.
// But it is an OVERRIDE: leave it unset — as nearly every install does — and the
// boundary is derived from forge.github_api_url or forge.gitlab.base_url, and
// neither was checked. A base_url of "https://gİtlab.example.com" loaded
// clean, and RunLore lowercased it into the credential boundary
// "gitlab.example.com" — a SEPARATELY REGISTRABLE name that now collects the
// forge token, while the operator's own instance (which resolves through IDNA to
// xn--gtlab-56a.example.com) is refused the credential it owns and produces the
// empty what_changed of RunLore #495. Both halves silent, on both providers.
//
// The remedy has to be LOUD. Quietly withholding IS the #495 failure mode, so
// this fails config load naming the key the operator has to edit.
func TestValidateRefusesANonASCIIDerivedForgeHost(t *testing.T) {
	// U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE: strings.ToLower maps it to a
	// plain ASCII 'i', idna.Lookup.ToASCII does not. Written as an escape so this
	// test does not itself contain the confusable it is about.
	const dottedCapitalI = "\u0130"

	for _, tc := range []struct {
		name, key string
		apply     func(c *Config)
	}{
		{
			name: "gitlab base_url",
			key:  "forge.gitlab.base_url",
			apply: func(c *Config) {
				c.Forge.Provider = "gitlab"
				c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
				c.Forge.GitLab.BaseURL = "https://g" + dottedCapitalI + "tlab.example.com"
			},
		},
		{
			name: "github api url",
			key:  "forge.github_api_url",
			apply: func(c *Config) {
				c.Forge.GitHubAPIURL = "https://g" + dottedCapitalI + "he.example.com/api/v3"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			tc.apply(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("a non-ASCII host in %s loaded clean; RunLore lowercases it into the "+
					"credential boundary, so the forge token goes to the ASCII name that fold "+
					"produces and is withheld from the operator's own forge", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("the refusal must name the key the operator has to edit (%s), got: %v",
					tc.key, err)
			}
		})
	}

	// The other direction, and the one this refusal must not cost: every ASCII
	// forge spelling an operator legitimately runs still loads. Over-refusing
	// here would be #495 again, arriving as a startup crash instead of an empty
	// result.
	for _, tc := range []struct {
		name  string
		apply func(c *Config)
	}{
		{"github.com default", func(*Config) {}},
		{"github.com spelled out", func(c *Config) { c.Forge.GitHubAPIURL = "https://api.github.com" }},
		{"github enterprise", func(c *Config) { c.Forge.GitHubAPIURL = "https://ghe.example.com/api/v3" }},
		{"github enterprise on a port", func(c *Config) {
			c.Forge.GitHubAPIURL = "https://ghe.example.com:8443/api/v3"
		}},
		{"gitlab.com", func(c *Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
		}},
		{"self-hosted gitlab", func(c *Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
			c.Forge.GitLab.BaseURL = "https://gitlab.example.com"
		}},
		// The derived keys get an ASCII refusal, NOT the full bareHost shape the
		// explicit git_host gets: url.Hostname() has already stripped what
		// bareHost exists to reject, and these two spellings fold identically
		// under every normaliser, so refusing them would break real deployments
		// for no security gain.
		{"self-hosted gitlab with an underscore label", func(c *Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
			c.Forge.GitLab.BaseURL = "https://git_lab.internal"
		}},
		{"self-hosted gitlab on an IPv6 literal", func(c *Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
			c.Forge.GitLab.BaseURL = "https://[2001:db8::1]:8443"
		}},
		// Punycode IS the ASCII spelling of an internationalised forge, so an
		// operator who runs one has a way through that does not need git_host.
		{"punycode idn forge", func(c *Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.GitLab.TokenEnv = "GITLAB_TOKEN"
			c.Forge.GitLab.BaseURL = "https://xn--gtlab-56a.example.com"
		}},
	} {
		t.Run("accepted "+tc.name, func(t *testing.T) {
			c := &Config{}
			tc.apply(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("a legitimate ASCII forge must still load, got: %v", err)
			}
		})
	}
}

// TestLogsIndexValidate: logs.index is interpolated into the Elasticsearch
// request PATH. The client escapes it so a malformed value can no longer change
// the request's SHAPE, but escaping alone turns a typo into a puzzling 4xx from
// the cluster mid-investigation instead of a startup failure naming the config
// key — the "fails closed and loudly" contract every other key here honours.
func TestLogsIndexValidate(t *testing.T) {
	for _, ok := range []string{
		"",                        // unset ⇒ the logs-* default
		"logs-*",                  // the shipped default, spelled out
		"logs-app-*,logs-infra-*", // multi-index list
		"filebeat-8.11.3-2026.08.02",
		"remote_cluster:logs-*", // cross-cluster search
		".ds-logs-generic-default",
	} {
		c := &Config{}
		c.Logs.Index = ok
		if err := c.Validate(); err != nil {
			t.Fatalf("logs.index %q must validate, got %v", ok, err)
		}
	}
	for _, bad := range []struct{ index, want string }{
		{"a/b", "forbid"},             // used to add a path segment
		{"logs-*?pretty", "forbid"},   // used to land in the query string
		{"logs *", "forbid"},          // whitespace
		{"logs-#1", "forbid"},         // fragment
		{"logs-app-*,", "empty"},      // trailing comma in the list
		{"logs-app-*,,x", "empty"},    // doubled comma
		{"-logs-*", "must not start"}, // ES exclusion syntax; matches nothing
		{"_logs", "must not start"},   // ES-reserved
		{"Logs-*", "must be lowercase"}} {
		c := &Config{}
		c.Logs.Index = bad.index
		err := c.Validate()
		if err == nil {
			t.Fatalf("logs.index %q must be rejected at startup", bad.index)
		}
		if !strings.Contains(err.Error(), "logs.index") || !strings.Contains(err.Error(), bad.want) {
			t.Fatalf("logs.index %q: error must name the key and the reason %q, got %v", bad.index, bad.want, err)
		}
	}
}

// TestCommonsCatalogValidation pins the fail-closed cases for the shared catalog.
//
// Both are silent-degradation bugs otherwise: a missing dir leaves the commons
// simply absent (an operator who configured it sees nothing and no error), and a
// shared dir lets an upstream sync write into the operator's own checkout — which
// with git-sync enabled means the two fight on every reconcile.
func TestCommonsCatalogValidation(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Catalog.Dir = "/var/lib/runlore/catalog"
		c.Catalog.Commons.URL = "https://github.com/Smana/runlore-kb-commons"
		return c
	}

	t.Run("url without dir is rejected", func(t *testing.T) {
		c := base()
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "catalog.commons.dir") {
			t.Fatalf("want a commons.dir error, got: %v", err)
		}
	})

	t.Run("dir equal to catalog.dir is rejected", func(t *testing.T) {
		c := base()
		c.Catalog.Commons.Dir = c.Catalog.Dir
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must differ") {
			t.Fatalf("want a distinct-directory error, got: %v", err)
		}
	})

	t.Run("distinct dir validates", func(t *testing.T) {
		c := base()
		c.Catalog.Commons.Dir = "/var/lib/runlore/commons"
		if err := c.Validate(); err != nil {
			t.Fatalf("a well-formed commons config must validate: %v", err)
		}
	})

	t.Run("absent commons is unaffected", func(t *testing.T) {
		c := &Config{}
		c.Catalog.Dir = "/var/lib/runlore/catalog"
		if err := c.Validate(); err != nil {
			t.Fatalf("no commons configured must stay valid: %v", err)
		}
	})
}

// TestCommonsIntervalDefaultsToTheDocumentedRate: the commons syncer polls a
// SHARED, public upstream repo, and its documented default is 24h. Nothing filled
// it, so an unset interval reached Syncer.Run as 0 and fell through to the
// syncer's generic 5-minute fallback — 288x the documented rate against someone
// else's GitHub repo — while the startup log printed "interval=0s".
func TestCommonsIntervalDefaultsToTheDocumentedRate(t *testing.T) {
	c := &Config{}
	c.Catalog.Dir = "/var/lib/runlore/catalog"
	c.Catalog.Commons.URL = "https://github.com/Smana/runlore-kb-commons"
	c.Catalog.Commons.Dir = "/var/lib/runlore/commons"
	ApplyDefaults(c)
	if got := c.Catalog.Commons.Interval.Std(); got != 24*time.Hour {
		t.Fatalf("commons interval = %v, want 24h (the documented default; 0 falls through to the syncer's 5m)", got)
	}
	if c.Catalog.Commons.Branch != "main" {
		t.Fatalf("commons branch = %q, want main", c.Catalog.Commons.Branch)
	}
}

// TestCommonsDirMustNotNestInsideCatalogDir: the loader walks recursively, so a
// commons root nested under the operator's catalog is indexed TWICE — once
// prefixed and marked as shared, once as the operator's own, unmarked and
// competing in the tie-break as a local entry. Exact string equality missed it,
// and missed a trailing slash too.
func TestCommonsDirMustNotNestInsideCatalogDir(t *testing.T) {
	for _, tc := range []struct{ name, own, commons string }{
		{"commons nested in catalog", "/var/lib/runlore/catalog", "/var/lib/runlore/catalog/commons"},
		{"catalog nested in commons", "/var/lib/runlore/commons/own", "/var/lib/runlore/commons"},
		{"trailing slash", "/var/lib/runlore/catalog", "/var/lib/runlore/catalog/"},
		{"unclean path", "/var/lib/runlore/catalog", "/var/lib/runlore/catalog/../catalog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.Catalog.Dir = tc.own
			c.Catalog.Commons.URL = "https://example.com/kb"
			c.Catalog.Commons.Dir = tc.commons
			ApplyDefaults(c)
			if err := c.Validate(); err == nil {
				t.Fatalf("want a validation error for catalog.dir=%q commons.dir=%q — a shared root must never overlap the operator's own", tc.own, tc.commons)
			}
		})
	}

	// Control: genuinely separate roots must still validate.
	c := &Config{}
	c.Catalog.Dir = "/var/lib/runlore/catalog"
	c.Catalog.Commons.URL = "https://example.com/kb"
	c.Catalog.Commons.Dir = "/var/lib/runlore/commons"
	ApplyDefaults(c)
	if err := c.Validate(); err != nil {
		t.Fatalf("separate roots must validate, got: %v", err)
	}
}

// TestValidateThreadCapture guards the opt-in contract of
// notify.slack.thread_capture: it requires the signing secret (mentions arrive
// on the exposed POST /slack/events endpoint and must be signature-verified),
// the bot-token delivery path (bot_token_env + channel) — a webhook returns
// no message ts, so there is no thread root to attribute a reply to — and
// outcome.ledger_path, because the thread registry lives beside the ledger.
// Off (the default) validates clean with none of it.
func TestValidateThreadCapture(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Notify.Slack = SlackNotify{
			BotTokenEnv:      "SLACK_BOT_TOKEN",
			Channel:          "C1",
			SigningSecretEnv: "SLACK_SIGNING_SECRET",
			ThreadCapture:    true,
		}
		c.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"off needs nothing", func(c *Config) {
			c.Notify.Slack = SlackNotify{ThreadCapture: false}
		}, ""},
		{"missing signing secret", func(c *Config) {
			c.Notify.Slack.SigningSecretEnv = ""
		}, "signing_secret_env"},
		{"webhook-only delivery", func(c *Config) {
			c.Notify.Slack.BotTokenEnv = ""
			c.Notify.Slack.Channel = ""
			c.Notify.Slack.WebhookURLEnv = "SLACK_WEBHOOK_URL"
		}, "bot_token_env"},
		{"bot token without a channel", func(c *Config) {
			c.Notify.Slack.Channel = ""
		}, "channel"},
		{"missing ledger path", func(c *Config) {
			c.Outcome.LedgerPath = ""
		}, "ledger_path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate() = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateModelChat guards the model.chat inheritance-safety decision: unlike
// model.verify (which runs once per investigation, on a path RunLore itself
// initiates), model.chat runs once per addressed thread message, on a path any
// channel or room member can trigger. Silently inheriting the investigation model
// there would make the cheapest way to enable the feature (`model.chat: {}`) the
// most expensive way to run it, so Validate requires the model be named
// explicitly — the one deliberate divergence from Verify's cmp.Or-everything
// inheritance. Every other field (provider, base_url, api_key_env, effort,
// thinking) inherits exactly as model.verify's does, so this also exercises
// those checks against model.chat, mirroring TestValidateEffort/
// TestValidateThinking/TestValidateCleartextKeyCoversVerifyAndEmbeddings.
//
// The thread-capture-off case is deliberately a warning, not an error: the
// operator may be staging config (thread_capture next), and a coherent-but-
// pointless block should not block startup while it settles into place.
func TestValidateModelChat(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Model.Provider = "anthropic"
		c.Model.Model = "claude-big"
		c.Model.Chat = &ModelOverride{Model: "claude-cheap"}
		// A fully-satisfied Slack thread_capture (see TestValidateThreadCapture): the
		// mention-listening channel model.chat needs to ever fire.
		c.Notify.Slack = SlackNotify{
			BotTokenEnv:      "SLACK_BOT_TOKEN",
			Channel:          "C1",
			SigningSecretEnv: "SLACK_SIGNING_SECRET",
			ThreadCapture:    true,
		}
		c.Outcome.LedgerPath = "/var/lib/runlore/outcomes.jsonl"
		return c
	}

	tests := []struct {
		name     string
		mutate   func(*Config)
		wantErr  string // "" = must validate clean
		wantWarn string // "" = ChatWithoutCaptureWarning must return ""; else a substring it must contain
	}{
		{"unset is valid, feature off", func(c *Config) { c.Model.Chat = nil }, "", ""},
		{"valid model is valid, no warning (capture is on)", func(*Config) {}, "", ""},
		{"empty model rejected", func(c *Config) { c.Model.Chat.Model = "" }, "model.chat.model", ""},
		{"whitespace-only model rejected", func(c *Config) { c.Model.Chat.Model = "   " }, "model.chat.model", ""},
		{"neither capture channel on: valid but warned", func(c *Config) {
			c.Notify.Slack.ThreadCapture = false
		}, "", "model.chat"},
		{"matrix capture alone silences the warning", func(c *Config) {
			c.Notify.Slack.ThreadCapture = false
			c.Notify.Matrix = MatrixNotify{
				Homeserver:     "https://matrix.example.org",
				RoomID:         "!room:example.org",
				AccessTokenEnv: "MATRIX_ACCESS_TOKEN",
				ThreadCapture:  true,
			}
		}, "", ""},
		{"chat inherits the parent's effort and thinking when unset", func(c *Config) {
			c.Model.Effort = "max"        // valid for anthropic, the shared provider
			c.Model.Thinking = "adaptive" // valid for anthropic, the shared provider
		}, "", ""},
		{"chat's own effort validated against its own vocabulary", func(c *Config) {
			c.Model.Chat.Effort = "minimal" // not in the anthropic vocabulary
		}, "model.chat.effort", ""},
		{"inherited parent effort invalid for the override's own provider", func(c *Config) {
			c.Model.Effort = "max" // valid for anthropic, not for openai
			c.Model.Chat.Provider = "openai"
		}, "model.chat.effort", ""},
		{"chat's own thinking mode validated", func(c *Config) {
			c.Model.Chat.Thinking = "enabled" // not a valid mode
		}, "model.chat.thinking", ""},
		{"inherited parent thinking invalid for the override's own provider", func(c *Config) {
			c.Model.Thinking = "adaptive" // valid for anthropic, not for openai
			c.Model.Chat.Provider = "openai"
		}, "model.chat.thinking", ""},
		{"chat cleartext key over its own public http base_url rejected", func(c *Config) {
			c.Model.Chat.BaseURL = "http://api.public.example/v1"
			c.Model.Chat.APIKeyEnv = "CHAT_KEY"
		}, "model.chat.base_url", ""},
		{"chat inherits an insecure parent base_url with its own key: rejected", func(c *Config) {
			c.Model.BaseURL = "http://api.public.example/v1"
			c.Model.Chat.APIKeyEnv = "CHAT_KEY"
		}, "model.chat.base_url", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate() = %v, want it to mention %q", err, tt.wantErr)
			}
			if tt.wantErr != "" {
				return // an invalid config's warning text is not meaningful
			}
			warn := ChatWithoutCaptureWarning(c)
			switch {
			case tt.wantWarn == "" && warn != "":
				t.Fatalf("ChatWithoutCaptureWarning() = %q, want \"\"", warn)
			case tt.wantWarn != "" && !strings.Contains(warn, tt.wantWarn):
				t.Fatalf("ChatWithoutCaptureWarning() = %q, want it to contain %q", warn, tt.wantWarn)
			}
		})
	}
}

// TestNotifyThreadRoundTrip pins Gap 2: every notify.thread key must parse
// into ThreadNotify, and an explicit value must win over its default.
func TestNotifyThreadRoundTrip(t *testing.T) {
	y := `
notify:
  thread:
    max_notes_per_thread: 5
    forge_writes_per_hour: 3
    registry_ttl: 48h
    registry_max: 100
    max_note_bytes: 4096
    chat_calls_per_hour: 45
    chat_tokens_per_hour: 123456
    announce_kb_updates: true
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th := c.Notify.Thread
	if th.MaxNotesPerThread != 5 || th.ForgeWritesPerHour != 3 || th.RegistryTTL.Std() != 48*time.Hour ||
		th.RegistryMax != 100 || th.MaxNoteBytes != 4096 ||
		th.ChatCallsPerHour != 45 || th.ChatTokensPerHour != 123456 || !th.AnnounceKBUpdates.On() {
		t.Fatalf("notify.thread not parsed: %+v", th)
	}
	if got := th.EffectiveMaxNotesPerThread(); got != 5 {
		t.Fatalf("EffectiveMaxNotesPerThread() = %d, want the explicit 5", got)
	}
	if got := th.EffectiveForgeWritesPerHour(); got != 3 {
		t.Fatalf("EffectiveForgeWritesPerHour() = %d, want the explicit 3", got)
	}
	if got := th.EffectiveRegistryTTL(); got != 48*time.Hour {
		t.Fatalf("EffectiveRegistryTTL() = %v, want the explicit 48h", got)
	}
	if got := th.EffectiveRegistryMax(); got != 100 {
		t.Fatalf("EffectiveRegistryMax() = %d, want the explicit 100", got)
	}
	if got := th.EffectiveMaxNoteBytes(); got != 4096 {
		t.Fatalf("EffectiveMaxNoteBytes() = %d, want the explicit 4096", got)
	}
	if got := th.EffectiveChatCallsPerHour(); got != 45 {
		t.Fatalf("EffectiveChatCallsPerHour() = %d, want the explicit 45", got)
	}
	if got := th.EffectiveChatTokensPerHour(); got != 123456 {
		t.Fatalf("EffectiveChatTokensPerHour() = %d, want the explicit 123456", got)
	}
}

// TestAnnounceKBUpdatesIsOptIn pins the DEFAULT of
// notify.thread.announce_kb_updates, which is the whole decision recorded for
// it: absent means off, and explicitly false means off, because the
// announcement adds notification volume to channels nobody asked to have it in
// — the thread reply is already the direct acknowledgement to the person who
// typed — and, since the announcement carries note content, turning it on sends
// a note written in one thread to every configured sink. That is an operator's
// call to make knowingly, so the safe state is the unconfigured state.
//
// All three states are asserted separately rather than just "absent": a switch
// whose zero value is the default reads as covered the moment anything asserts
// off, and the case that would actually reveal a wrong-way default — an
// operator writing the key out explicitly to turn it OFF — is the one a
// zero-value-only test never exercises.
//
// It asserts On() rather than the raw value on purpose: On() is what every
// consumer branches on, and it is the property "the unconfigured state does not
// broadcast" is a claim about. The exact mode each spelling resolves to is
// TestAnnounceModeAcceptsBooleansAndNames' job.
func TestAnnounceKBUpdatesIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want bool
	}{
		{
			name: "absent from a notify.thread block that sets other keys",
			yaml: "notify:\n  thread:\n    max_note_bytes: 4096\n",
		},
		{
			name: "no notify block at all",
			yaml: "model:\n  provider: anthropic\n",
		},
		{
			name: "explicitly false",
			yaml: "notify:\n  thread:\n    announce_kb_updates: false\n",
		},
		{
			name: "explicitly off",
			yaml: "notify:\n  thread:\n    announce_kb_updates: \"off\"\n",
		},
		{
			name: "explicitly true",
			yaml: "notify:\n  thread:\n    announce_kb_updates: true\n",
			want: true,
		},
		{
			name: "explicitly thread-routed",
			yaml: "notify:\n  thread:\n    announce_kb_updates: thread\n",
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := yaml.Unmarshal([]byte(tc.yaml), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := c.Notify.Thread.AnnounceKBUpdates.On(); got != tc.want {
				t.Fatalf("notify.thread.announce_kb_updates = %q, On() = %v, want %v — the announcement fans a note "+
					"written in one thread out to every configured sink, so the unconfigured state must be off",
					string(c.Notify.Thread.AnnounceKBUpdates), got, tc.want)
			}
			// The key must land on the named Thread field, never in Notify.Extra,
			// for the same reason every other notify.thread key must: an inline
			// map entry would shadow a notifier registered under that name (see
			// TestNotifyThreadDoesNotShadowARegisteredNotifier).
			if _, ok := c.Notify.Extra["thread"]; ok {
				t.Fatal("notify.thread must not also land in Notify.Extra")
			}
		})
	}
}

// TestAnnounceModeAcceptsBooleansAndNames is the BACKWARD-COMPATIBILITY pin for
// notify.thread.announce_kb_updates.
//
// The key shipped and was documented as a plain YAML boolean before it grew
// destinations, so every file already in an operator's repository must keep
// meaning exactly what it meant. That is a stronger claim than "true still
// parses": `true` must still resolve to the CHANNEL destination — the message,
// the sink set and the place it lands, all unchanged — because an operator who
// upgrades and finds their announcements silently moved into threads has had
// their configuration reinterpreted under them.
//
// YAML 1.1's `on`/`off`/`yes`/`no` are covered too, and not as a courtesy: this
// field was a Go bool, and yaml.v3 resolves all of those into a bool target, so
// they are values that WORK today whether or not any page documents them. A
// string-first unmarshaler would silently turn `off` into an unknown mode and
// fail an operator's existing file at startup.
func TestAnnounceModeAcceptsBooleansAndNames(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want AnnounceMode
	}{
		// The two shipped spellings. These are the compatibility contract.
		{val: "true", want: AnnounceChannel},
		{val: "false", want: AnnounceOff},
		// YAML 1.1 booleans, which the bool field accepted before this type existed.
		{val: "on", want: AnnounceChannel},
		{val: "off", want: AnnounceOff},
		{val: "yes", want: AnnounceChannel},
		{val: "no", want: AnnounceOff},
		{val: "TRUE", want: AnnounceChannel},
		// An empty value is a null node. yaml.v3 short-circuits null BEFORE it
		// looks for an Unmarshaler, so UnmarshalYAML is never called and the
		// field keeps its zero value — which is why "" has to be off in its own
		// right rather than by being normalised to AnnounceOff on the way in.
		// The bool field it replaces resolved a null to false, so this is the
		// same answer by a different route.
		{val: "", want: ""},
		{val: "~", want: ""},
		{val: "null", want: ""},
		// The names.
		{val: "channel", want: AnnounceChannel},
		{val: "thread", want: AnnounceThread},
		{val: "both", want: AnnounceBoth},
		{val: `"off"`, want: AnnounceOff},
		{val: "Thread", want: AnnounceThread},
		{val: `"  both  "`, want: AnnounceBoth},
	} {
		t.Run(tc.val, func(t *testing.T) {
			var c Config
			y := "notify:\n  thread:\n    announce_kb_updates: " + tc.val + "\n"
			if err := yaml.Unmarshal([]byte(y), &c); err != nil {
				t.Fatalf("unmarshal %q: %v", tc.val, err)
			}
			if got := c.Notify.Thread.AnnounceKBUpdates; got != tc.want {
				t.Fatalf("announce_kb_updates: %s parsed as %q, want %q", tc.val, string(got), string(tc.want))
			}
			if err := c.Validate(); err != nil {
				t.Fatalf("announce_kb_updates: %s must validate clean: %v", tc.val, err)
			}
			// On() is the property every consumer branches on, so both spellings
			// of off must answer it the same way. "" reaches here only via the
			// null path above, where no unmarshaler ran to normalise it.
			if want := tc.want != AnnounceOff && tc.want != ""; c.Notify.Thread.AnnounceKBUpdates.On() != want {
				t.Fatalf("announce_kb_updates: %s parsed as %q, On() = %v, want %v",
					tc.val, string(tc.want), !want, want)
			}
		})
	}
}

// TestAnnounceModeDeliveryIsTotal pins the mapping from the operator's word onto
// the destination the notifiers act on (providers.KBDelivery).
//
// The two enums are separate because only config has an OFF state — On() is what
// decides an announcer exists at all — and a mapping that is written once and
// read nowhere else is exactly the kind that quietly loses a case. A mode added
// here without a Delivery arm would fall through to the channel and turn a new
// destination into a silent no-op, which is the failure the default arm of a
// switch is very good at hiding.
func TestAnnounceModeDeliveryIsTotal(t *testing.T) {
	for mode, want := range map[AnnounceMode]providers.KBDelivery{
		AnnounceChannel: providers.KBDeliverChannel,
		AnnounceThread:  providers.KBDeliverThread,
		AnnounceBoth:    providers.KBDeliverBoth,
		// Off has no delivery; it must resolve to the harmless zero value rather
		// than to a route.
		AnnounceOff: providers.KBDeliverChannel,
		"":          providers.KBDeliverChannel,
	} {
		if got := mode.Delivery(); got != want {
			t.Errorf("AnnounceMode(%q).Delivery() = %q, want %q", string(mode), string(got), string(want))
		}
	}
	// The routing modes must be distinguishable from the shipped one, or the
	// mapping above is satisfied by a function that returns the zero value.
	if AnnounceThread.Delivery() == AnnounceChannel.Delivery() {
		t.Error("thread and channel resolve to the same delivery — routing would be inert")
	}
	if !AnnounceThread.Delivery().IntoThread() || !AnnounceBoth.Delivery().IntoThread() {
		t.Error("thread/both must resolve to a delivery that asks for the originating thread")
	}
	if AnnounceChannel.Delivery().IntoThread() {
		t.Error("channel must not ask for the originating thread — that is the shipped behaviour of `true`")
	}
}

// TestAnnounceModeRejectsAnUnknownName pins that a typo fails LOAD rather than
// falling back.
//
// Silently reading "treads" as the channel would leave an operator believing the
// echo they configured away is gone while every write still restates itself in
// the channel — the exact complaint the routing exists to answer, now invisible.
// The message must name the accepted values, because the operator who typed a
// wrong one has no other way to learn what the right ones are.
func TestAnnounceModeRejectsAnUnknownName(t *testing.T) {
	for _, val := range []string{"treads", "chanel", "room", `"true"`, "thread,channel"} {
		t.Run(val, func(t *testing.T) {
			var c Config
			y := "notify:\n  thread:\n    announce_kb_updates: " + val + "\n"
			if err := yaml.Unmarshal([]byte(y), &c); err != nil {
				t.Fatalf("unmarshal %q must not fail — Validate is what reports it: %v", val, err)
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("announce_kb_updates: %s validated clean; an unknown destination must fail at load", val)
			}
			msg := err.Error()
			if !strings.Contains(msg, "announce_kb_updates") {
				t.Errorf("the error must name the key, got: %v", err)
			}
			for _, want := range []string{"off", "channel", "thread", "both"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the error must name the valid value %q so the operator can fix it, got: %v", want, err)
				}
			}
		})
	}
}

// TestNotifyThreadDefaultsWhenAbsent pins "an absent notify.thread block
// changes nothing": every Effective* method must resolve to the exact
// pre-existing hardcoded constant it replaces.
func TestNotifyThreadDefaultsWhenAbsent(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("notify:\n  slack:\n    channel: C1\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th := c.Notify.Thread
	if got := th.EffectiveMaxNotesPerThread(); got != thread.DefaultMaxNotesPerThread {
		t.Fatalf("EffectiveMaxNotesPerThread() = %d, want the default %d", got, thread.DefaultMaxNotesPerThread)
	}
	if got := th.EffectiveForgeWritesPerHour(); got != thread.DefaultForgeWritesPerHour {
		t.Fatalf("EffectiveForgeWritesPerHour() = %d, want the default %d", got, thread.DefaultForgeWritesPerHour)
	}
	if got := th.EffectiveRegistryTTL(); got != thread.DefaultRegistryTTL {
		t.Fatalf("EffectiveRegistryTTL() = %v, want the default %v", got, thread.DefaultRegistryTTL)
	}
	if got := th.EffectiveRegistryMax(); got != thread.DefaultRegistryMax {
		t.Fatalf("EffectiveRegistryMax() = %d, want the default %d", got, thread.DefaultRegistryMax)
	}
	if got := th.EffectiveMaxNoteBytes(); got != thread.DefaultMaxNoteBytes {
		t.Fatalf("EffectiveMaxNoteBytes() = %d, want the default %d", got, thread.DefaultMaxNoteBytes)
	}
	if got := th.EffectiveChatCallsPerHour(); got != thread.DefaultChatCallsPerHour {
		t.Fatalf("EffectiveChatCallsPerHour() = %d, want the default %d", got, thread.DefaultChatCallsPerHour)
	}
	if got := th.EffectiveChatTokensPerHour(); got != thread.DefaultChatTokensPerHour {
		t.Fatalf("EffectiveChatTokensPerHour() = %d, want the default %d", got, thread.DefaultChatTokensPerHour)
	}
}

// TestNotifyThreadZeroIsAlsoTheDefault pins the explicit-zero case
// separately from "absent": an operator who writes `max_note_bytes: 0`
// (rather than omitting the key) must get the identical default, not a
// distinct "unlimited" meaning — see ThreadNotify's doc comment for why
// unlimited is not offered for any key in this block.
func TestNotifyThreadZeroIsAlsoTheDefault(t *testing.T) {
	var c Config
	y := "notify:\n  thread:\n    max_notes_per_thread: 0\n    forge_writes_per_hour: 0\n" +
		"    registry_ttl: 0h\n    registry_max: 0\n    max_note_bytes: 0\n" +
		"    chat_calls_per_hour: 0\n    chat_tokens_per_hour: 0\n"
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	th := c.Notify.Thread
	if got := th.EffectiveMaxNoteBytes(); got != thread.DefaultMaxNoteBytes {
		t.Fatalf("explicit max_note_bytes: 0 must mean the default (%d), got %d", thread.DefaultMaxNoteBytes, got)
	}
	if got := th.EffectiveForgeWritesPerHour(); got != thread.DefaultForgeWritesPerHour {
		t.Fatalf("explicit forge_writes_per_hour: 0 must mean the default (%d), got %d", thread.DefaultForgeWritesPerHour, got)
	}
	if got := th.EffectiveChatCallsPerHour(); got != thread.DefaultChatCallsPerHour {
		t.Fatalf("explicit chat_calls_per_hour: 0 must mean the default (%d), got %d", thread.DefaultChatCallsPerHour, got)
	}
	if got := th.EffectiveChatTokensPerHour(); got != thread.DefaultChatTokensPerHour {
		t.Fatalf("explicit chat_tokens_per_hour: 0 must mean the default (%d), got %d", thread.DefaultChatTokensPerHour, got)
	}
	// Asserted unconditionally, and on an otherwise-valid config. The earlier
	// version nested this inside `if err != nil` out of a worry that an unset
	// Model.Provider would fail Validate on its own — it does not, a zero
	// Config validates clean, so err was always nil and the whole block was
	// dead. Provider is set anyway, matching TestNotifyThreadNegativeRejected,
	// so a future required field cannot resurrect that ambiguity.
	c.Model.Provider = "anthropic"
	if err := (&c).Validate(); err != nil {
		t.Fatalf("an all-zero notify.thread block means \"use the defaults\" and must validate clean, got: %v", err)
	}
}

// TestThreadDefaultsHaveTheirDocumentedValues pins the NUMBERS.
//
// The Effective* tests above cannot: each asserts EffectiveX() == thread.DefaultX
// while EffectiveX()'s whole body is `return thread.DefaultX`, so both sides move
// together and the assertion holds for any value — change DefaultRegistryTTL from
// seven days to zero and they still pass. What they genuinely catch is a fallback
// wired to the WRONG constant, which is worth keeping; what they cannot catch is
// a constant drifting.
//
// These are operator-facing: they appear in the configuration docs and they decide
// what an unconfigured deployment spends. Changing one should be a deliberate edit
// to this list, not a side effect of something else.
func TestThreadDefaultsHaveTheirDocumentedValues(t *testing.T) {
	if got, want := thread.DefaultRegistryTTL, 7*24*time.Hour; got != want {
		t.Errorf("DefaultRegistryTTL = %v, want %v", got, want)
	}
	if got, want := thread.DefaultMaxNoteBytes, 8<<10; got != want {
		t.Errorf("DefaultMaxNoteBytes = %d, want %d", got, want)
	}
	// The rest are bounds whose exact value is a tuning choice, but every one of
	// them must be positive: 0 means "use the default" on this block, so a zero
	// default is a fallback that resolves to itself, and a negative one is what
	// Validate rejects.
	for _, tc := range []struct {
		name string
		got  int
	}{
		{"DefaultMaxNotesPerThread", thread.DefaultMaxNotesPerThread},
		{"DefaultForgeWritesPerHour", thread.DefaultForgeWritesPerHour},
		{"DefaultRegistryMax", thread.DefaultRegistryMax},
		{"DefaultChatCallsPerHour", thread.DefaultChatCallsPerHour},
	} {
		if tc.got <= 0 {
			t.Errorf("%s = %d, want a positive bound", tc.name, tc.got)
		}
	}
	// DefaultChatTokensPerHour is NOT in that loop, because ">0" is exactly the
	// assertion that let it ship wrong. It is the one default nobody types: it is
	// DERIVED (maxChatCallTokens x DefaultChatCallsPerHour x 2/3), so an unrelated
	// edit to a byte cap in internal/thread moves it silently, and the docs — which
	// quote it as a number an operator budgets against — do not move with it. That
	// is not hypothetical: the shipped docs said 200000, the value from before the
	// derivation landed, while the constant was really 107720. An operator sizing a
	// deployment for 200000 hit the real ceiling at ~54% of the planned volume, and
	// one who pasted `chat_tokens_per_hour: 200000` as "the default" raised the true
	// ceiling by 1.86x.
	//
	// Pinning the literal makes the arithmetic visible in a diff. Changing a byte
	// cap upstream now fails HERE, with this number in the message, and
	// docsguard.TestThreadDefaultsMatchTheDocs fails alongside it naming every page
	// that must be re-stated. Update both together, never one.
	if got, want := thread.DefaultChatTokensPerHour, int64(109480); got != want {
		t.Errorf("DefaultChatTokensPerHour = %d, want %d — if this was a deliberate retune, "+
			"restate the new number everywhere the docs quote it (docsguard will list them)", got, want)
	}
}

// TestNotifyThreadNegativeRejected pins the zero/negative choice documented
// on ThreadNotify: 0 means "use the default", a negative value is a
// misconfiguration Validate must reject — diverging deliberately from
// ratelimit.Window's own <= 0-means-unlimited convention, since "unlimited"
// would defeat the safety property each of these bounds exists to enforce.
func TestNotifyThreadNegativeRejected(t *testing.T) {
	base := func() *Config { return &Config{Model: Model{Provider: "anthropic"}} } // minimal valid config, see TestGitOpsMirrorConfig

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"max_notes_per_thread", func(c *Config) { c.Notify.Thread.MaxNotesPerThread = -1 }, "notify.thread.max_notes_per_thread"},
		{"forge_writes_per_hour", func(c *Config) { c.Notify.Thread.ForgeWritesPerHour = -1 }, "notify.thread.forge_writes_per_hour"},
		{"registry_ttl", func(c *Config) { c.Notify.Thread.RegistryTTL = -1 }, "notify.thread.registry_ttl"},
		{"registry_max", func(c *Config) { c.Notify.Thread.RegistryMax = -1 }, "notify.thread.registry_max"},
		{"max_note_bytes", func(c *Config) { c.Notify.Thread.MaxNoteBytes = -1 }, "notify.thread.max_note_bytes"},
		{"chat_calls_per_hour", func(c *Config) { c.Notify.Thread.ChatCallsPerHour = -1 }, "notify.thread.chat_calls_per_hour"},
		{"chat_tokens_per_hour", func(c *Config) { c.Notify.Thread.ChatTokensPerHour = -1 }, "notify.thread.chat_tokens_per_hour"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestNotifyThreadDoesNotShadowARegisteredNotifier pins the collision guard
// Notify.Extra's doc comment calls out: a notify.thread block must land on
// the named Thread field, never in Extra, or it would silently shadow a
// notifier registered under that name.
func TestNotifyThreadDoesNotShadowARegisteredNotifier(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("notify:\n  thread:\n    max_notes_per_thread: 7\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Notify.Thread.MaxNotesPerThread != 7 {
		t.Fatalf("notify.thread must parse onto the named Thread field, got %+v", c.Notify.Thread)
	}
	if _, ok := c.Notify.Extra["thread"]; ok {
		t.Fatal("notify.thread must not also land in Notify.Extra — it would shadow a notifier registered as \"thread\"")
	}
}

// TestMaxNoteBytesTooSmallToKeepAnyNoteIsRejected closes a misconfiguration that
// degraded silently in the one direction this whole feature exists to prevent:
// the human being told their words were saved when nothing of them was.
//
// capNoteText reserves the truncation marker INSIDE the budget, because a cap
// that the act of marking the cut then breaks is not a cap. Below the marker's
// own width there is no room for both, and the cap wins: the entry body, the
// generated title and the quote in the reply all become fragments of
// "…_(truncated —" and nothing else. The reply still said "📝 Opened
// knowledge-base PR #7 with your note" (reproduced at max_note_bytes: 20).
//
// Refused at LOAD rather than degraded more honestly at write time, because
// there is no honest degradation available: every byte of the note is gone
// either way, and the only question left is whether an operator finds out at
// startup or a stranger in a chat thread finds out after losing their words.
func TestMaxNoteBytesTooSmallToKeepAnyNoteIsRejected(t *testing.T) {
	base := func() *Config { return &Config{Model: Model{Provider: "anthropic"}} }

	for _, tt := range []struct {
		name  string
		value int
		want  bool // want an error
	}{
		{"zero still means the default", 0, false},
		{"negative, as before", -1, true},
		{"the width of the marker alone", 45, true},
		{"one below the floor", thread.MinMaxNoteBytes - 1, true},
		{"the floor itself is accepted", thread.MinMaxNoteBytes, false},
		{"a realistic operator value", 4096, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			c.Notify.Thread.MaxNoteBytes = tt.value
			err := c.Validate()
			if tt.want && err == nil {
				t.Fatalf("max_note_bytes: %d was accepted; nothing of a note survives it", tt.value)
			}
			if !tt.want && err != nil {
				t.Fatalf("max_note_bytes: %d was rejected: %v", tt.value, err)
			}
			if tt.want && !strings.Contains(err.Error(), "notify.thread.max_note_bytes") {
				t.Fatalf("Validate() = %v, want it to name notify.thread.max_note_bytes", err)
			}
		})
	}
}
