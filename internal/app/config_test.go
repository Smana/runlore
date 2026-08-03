// SPDX-License-Identifier: Apache-2.0

package app

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

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
			err := RequireWebhookAuth(cfg, tc.token)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RequireWebhookAuth err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestWebhookAuthWarning covers the (source enabled × token set × actions mode)
// matrix: a warning fires only when the alertmanager source is enabled AND the
// token is empty, regardless of whether a model is configured (unlike
// RequireWebhookAuth, which only fail-closes once a model is billed per request).
// actions.mode=approve gets the stronger wording; auto is deliberately NOT covered
// here — config.Validate already hard-fails an empty token under auto, so this
// helper never has to warn about it.
func TestWebhookAuthWarning(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		token    string
		mode     config.ActionMode
		wantWarn bool
		wantLoud bool // stronger approve-mode wording
	}{
		{"disabled + no token + off → silent (source not reachable)", false, "", config.ActionOff, false, false},
		{"disabled + no token + approve → silent (source not reachable)", false, "", config.ActionApprove, false, false},
		{"enabled + token + off → silent (authenticated)", true, "secret", config.ActionOff, false, false},
		{"enabled + token + approve → silent (authenticated)", true, "secret", config.ActionApprove, false, false},
		{"enabled + no token + off → warns", true, "", config.ActionOff, true, false},
		{"enabled + no token + suggest → warns", true, "", config.ActionSuggest, true, false},
		{"enabled + no token + approve → warns louder", true, "", config.ActionApprove, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := WebhookAuthWarning(tc.enabled, tc.token, tc.mode)
			if (got != "") != tc.wantWarn {
				t.Fatalf("WebhookAuthWarning(%v, %q, %s) = %q, wantWarn = %v", tc.enabled, tc.token, tc.mode, got, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			isLoud := strings.Contains(got, "actions.mode=approve")
			if isLoud != tc.wantLoud {
				t.Fatalf("WebhookAuthWarning(%v, %q, %s) loud = %v, want %v (msg: %q)", tc.enabled, tc.token, tc.mode, isLoud, tc.wantLoud, got)
			}
			if !strings.Contains(got, "server.webhook_token_env") || !strings.Contains(got, "docs/security-model.md") {
				t.Fatalf("WebhookAuthWarning message missing risk/fix pointers: %q", got)
			}
		})
	}
}

// TestRequirePagerDutyAuth mirrors TestRequireWebhookAuth for the PagerDuty
// source: its X-PagerDuty-Signature verification replaces the shared bearer
// token on /webhook/pagerduty, so once a model is configured an enabled
// PagerDuty source must carry a signing secret.
func TestRequirePagerDutyAuth(t *testing.T) {
	model := config.Model{Provider: "anthropic"} // built-in endpoint counts as configured
	tests := []struct {
		name    string
		model   config.Model
		enabled bool
		secret  string
		wantErr bool
	}{
		{"enabled + model + secret → ok", model, true, "s", false},
		{"enabled + model + no secret → refused", model, true, "", true},
		{"enabled + no model + no secret → ok (log-only)", config.Model{}, true, "", false},
		{"disabled + model + no secret → ok", model, false, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RequirePagerDutyAuth(&config.Config{Model: tc.model}, tc.enabled, tc.secret)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RequirePagerDutyAuth err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestRecallDecayWarning covers the (instant recall × outcome ledger × feedback
// channel) matrix behind the learning loop's feedback edge.
//
// Gate 3 (outcome decay) is fail-safe: an entry absent from the ledger's
// OpenCounts returns factor 1 and fires. That is correct — absence of evidence
// must never block a recall — but it means a ledger that never accumulates ground
// truth turns the gate into a silent no-op for EVERY entry, and the retirement
// pass (same factor, same floor) into one that can never propose anything.
//
// Ground truth reaches the ledger through exactly two channels, and human
// feedback is the only one this process can observe from its own config. So the
// warning fires on the one combination that is knowably at risk: recall on, a
// ledger configured to hold the evidence, and neither feedback channel enabled.
// Recall off ⇒ nothing recalls, so there is no trust to decay. Ledger unset ⇒ the
// operator turned the learning loop off deliberately (the `lore curate` precedent
// treats that as an info, not a warning), so nagging about it would be noise.
func TestRecallDecayWarning(t *testing.T) {
	const ledger = "/var/lib/runlore/catalog/outcomes.jsonl"
	tests := []struct {
		name       string
		recall     bool
		ledgerPath string
		slack      bool
		matrix     bool
		wantWarn   bool
	}{
		{"recall + ledger + no feedback → warns (the edge is inert)", true, ledger, false, false, true},
		{"recall + ledger + slack buttons → silent", true, ledger, true, false, false},
		{"recall + ledger + matrix reactions → silent", true, ledger, false, true, false},
		{"recall + ledger + both channels → silent", true, ledger, true, true, false},
		{"recall + no ledger → silent (learning loop off by choice)", true, "", false, false, false},
		{"recall + no ledger + slack buttons → silent", true, "", true, false, false},
		{"no recall + ledger + no feedback → silent (nothing recalls)", false, ledger, false, false, false},
		{"no recall + ledger + both channels → silent", false, ledger, true, true, false},
		{"no recall + no ledger → silent", false, "", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Catalog.InstantRecall.Enabled = tc.recall
			cfg.Outcome.LedgerPath = tc.ledgerPath
			cfg.Notify.Slack.FeedbackButtons = tc.slack
			cfg.Notify.Matrix.FeedbackReactions = tc.matrix

			got := RecallDecayWarning(cfg)
			if (got != "") != tc.wantWarn {
				t.Fatalf("RecallDecayWarning(recall=%v, ledger=%q, slack=%v, matrix=%v) = %q, wantWarn = %v",
					tc.recall, tc.ledgerPath, tc.slack, tc.matrix, got, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			// The message has to be actionable: name both fixes (a feedback channel,
			// and persisting the ledger) and the doc that explains the edge.
			for _, want := range []string{
				"notify.slack.feedback_buttons",
				"notify.matrix.feedback_reactions",
				"outcome.ledger_path",
				"persistent volume",
				"docs/concepts/learning-loop.md",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("warning is missing the fix pointer %q: %q", want, got)
				}
			}
			// …and it has to state the CONSEQUENCE an operator cares about, not the
			// mechanism: a stale entry keeps being trusted.
			if !strings.Contains(got, "full trust") {
				t.Errorf("warning does not state the operator-visible consequence: %q", got)
			}
		})
	}
}

// TestRecallDecayWarningDoesNotAssertTheResolveChannelIsDead is the honesty guard.
//
// Nothing in RunLore's configuration says whether the incident source actually
// emits resolves: `sources` records enablement only, and Alertmanager's
// `send_resolved` lives in the operator's receiver config, which RunLore never
// reads. (internal/app/investigate.go decides resolvability per event, from the
// fingerprint — a runtime fact, not a startup one.) So the warning may say the
// resolve channel is the only one LEFT, and that we cannot see it from here; it
// must not claim resolves never arrive. A deployment whose Alertmanager does send
// them is fine, and telling that operator their loop is broken would be a lie
// that trains them to ignore the line.
func TestRecallDecayWarningDoesNotAssertTheResolveChannelIsDead(t *testing.T) {
	cfg := &config.Config{}
	cfg.Catalog.InstantRecall.Enabled = true
	cfg.Outcome.LedgerPath = "/var/lib/runlore/catalog/outcomes.jsonl"

	got := RecallDecayWarning(cfg)
	if got == "" {
		t.Fatal("expected the warning to fire for recall + ledger + no feedback")
	}
	if !strings.Contains(got, "cannot tell from here") {
		t.Errorf("warning must hedge on the resolve channel it cannot observe: %q", got)
	}
	// Phrasings that would state the unobservable as fact.
	for _, forbidden := range []string{
		"no resolve",
		"never resolve",
		"resolves never",
		"will stay empty",
	} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Errorf("warning asserts %q, which this process cannot determine: %q", forbidden, got)
		}
	}
}

// TestModelProvider locks in the provider-name normalization: anthropic/gemini
// pass through; everything else (including "" and unknown) defaults to "openai".
func TestModelProvider(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{"anthropic", "anthropic"},
		{"gemini", "gemini"},
		{"openai", "openai"},
		{"", "openai"},
		{"vllm", "openai"},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			cfg := &config.Config{Model: config.Model{Provider: tc.provider}}
			if got := ModelProvider(cfg); got != tc.want {
				t.Fatalf("ModelProvider(%q) = %q, want %q", tc.provider, got, tc.want)
			}
		})
	}
}

// TestModelConfigured locks in usable-model detection: anthropic/gemini are
// configured via their built-in endpoint; every other provider needs a base_url.
func TestModelConfigured(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		baseURL  string
		want     bool
	}{
		{"anthropic built-in", "anthropic", "", true},
		{"gemini built-in", "gemini", "", true},
		{"openai with base_url", "openai", "http://vllm:8000/v1", true},
		{"openai without base_url", "openai", "", false},
		{"empty provider with base_url", "", "http://vllm:8000/v1", true},
		{"empty provider without base_url", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Model: config.Model{Provider: tc.provider, BaseURL: tc.baseURL}}
			if got := ModelConfigured(cfg); got != tc.want {
				t.Fatalf("ModelConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCatalogExpected locks in the readiness-gate predicate: a catalog is only
// expected when BOTH a catalog source and a usable model are configured. Without
// a model, BuildInvestigator deliberately skips the catalog (log-only
// investigator), so gating readiness on it would hold even the leader at 503
// forever (issue #251).
func TestCatalogExpected(t *testing.T) {
	tests := []struct {
		name     string
		dir      string
		provider string
		want     bool
	}{
		{"catalog and model", "/var/lib/runlore/catalog", "anthropic", true},
		{"catalog without model", "/var/lib/runlore/catalog", "", false},
		{"model without catalog", "", "anthropic", false},
		{"neither", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Model: config.Model{Provider: tc.provider}}
			cfg.Catalog.Dir = tc.dir
			if got := CatalogExpected(cfg); got != tc.want {
				t.Fatalf("CatalogExpected = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCatalogConfigured locks in the catalog-configured predicate: a mounted dir
// OR a git-sync URL counts as configured; neither does not.
func TestCatalogConfigured(t *testing.T) {
	tests := []struct {
		name string
		dir  string
		url  string
		want bool
	}{
		{"neither", "", "", false},
		{"dir only", "/var/lib/runlore/catalog", "", true},
		{"git url only", "", "https://github.com/x/kb", true},
		{"both", "/var/lib/runlore/catalog", "https://github.com/x/kb", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Catalog.Dir = tc.dir
			cfg.Catalog.Git.URL = tc.url
			if got := CatalogConfigured(cfg); got != tc.want {
				t.Fatalf("CatalogConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGitopsEngine locks in the engine selection: "argocd" passes through; every
// other value (including "" and unknown) defaults to "flux".
func TestGitopsEngine(t *testing.T) {
	tests := []struct {
		engine string
		want   string
	}{
		{"argocd", "argocd"},
		{"flux", "flux"},
		{"", "flux"},
		{"unknown", "flux"},
	}
	for _, tc := range tests {
		t.Run(tc.engine, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.GitOps.Engine = tc.engine
			if got := GitopsEngine(cfg); got != tc.want {
				t.Fatalf("GitopsEngine(%q) = %q, want %q", tc.engine, got, tc.want)
			}
		})
	}
}

// TestOutcomeKind locks in the recall/fresh labelling of an outcome-ledger open.
func TestOutcomeKind(t *testing.T) {
	if got := OutcomeKind(true); got != "recall" {
		t.Fatalf("OutcomeKind(true) = %q, want %q", got, "recall")
	}
	if got := OutcomeKind(false); got != "fresh" {
		t.Fatalf("OutcomeKind(false) = %q, want %q", got, "fresh")
	}
}
