package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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
// negative verify override) is rejected by Validate — a nonsensical value that
// would otherwise reach a provider request.
func TestValidateRejectsNegativeMaxTokens(t *testing.T) {
	c := &Config{Model: Model{Provider: "anthropic", MaxTokens: -1}}
	if err := c.Validate(); err == nil {
		t.Fatal("negative model.max_tokens must be rejected by Validate")
	}
	cv := &Config{Model: Model{Provider: "anthropic", Verify: &ModelOverride{MaxTokens: -5}}}
	if err := cv.Validate(); err == nil {
		t.Fatal("negative verify.max_tokens must be rejected by Validate")
	}
	// Zero and positive are fine.
	ok := &Config{Model: Model{Provider: "anthropic", MaxTokens: 0, Verify: &ModelOverride{MaxTokens: 4096}}}
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
