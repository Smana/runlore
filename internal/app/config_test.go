// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/source"
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

// webhookSource / watcherSource are the two adapter shapes ResolveCapableSource
// distinguishes. Only Desc.Kind is read, so the Impl may be nil.
var (
	webhookSource = source.Built{Desc: source.Descriptor{Name: "alertmanager", Kind: source.Webhook}}
	watcherSource = source.Built{Desc: source.Descriptor{Name: "gitops", Kind: source.Watcher}}
)

// TestResolveCapableSource pins the capability read to the adapter CONTRACT rather
// than to a list of source names. A Webhook source's Decode returns a DecodeResult
// carrying Resolved; a Watcher's Watch returns a request channel with nowhere to put
// a resolution — so "can a resolve ever arrive?" is answerable from Kind alone, and
// stays answerable when a new source registers.
func TestResolveCapableSource(t *testing.T) {
	tests := []struct {
		name  string
		built []source.Built
		want  bool
	}{
		{"no sources at all → nothing can resolve", nil, false},
		{"watcher only (GitOps failures) → nothing can resolve", []source.Built{watcherSource}, false},
		{"webhook only → a resolve is possible", []source.Built{webhookSource}, true},
		{"webhook + watcher → a resolve is possible", []source.Built{watcherSource, webhookSource}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveCapableSource(tc.built); got != tc.want {
				t.Errorf("ResolveCapableSource(%v) = %v, want %v", tc.built, got, tc.want)
			}
		})
	}
}

// TestRecallDecayWarning covers the (instant recall × outcome ledger × feedback
// channel) matrix behind the learning loop's feedback edge.
//
// Gate 3 (outcome decay) is fail-safe: an entry absent from the ledger's OpenCounts
// returns factor 1 and fires. That is correct — absence of evidence must never block
// a recall — but it means a ledger that accumulates no ground truth stops deriving
// trust from whether an entry actually worked, and the retirement pass (same factor,
// same floor) loses the evidence it proposes on.
//
// Human feedback is the one ground-truth path fully readable from this config, so the
// warning fires on the combination that is knowably at risk: recall on, a ledger
// configured to hold the evidence, and neither feedback channel enabled. Recall off ⇒
// nothing recalls, so there is no trust to decay. Ledger unset ⇒ the operator turned
// the learning loop off deliberately (the `lore curate` precedent treats that as an
// info, not a warning), so nagging about it would be noise. The predicate is
// independent of resolveCapable — that only decides the WORDING.
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
		{"recall + ledger + no feedback → warns (the edge is unverifiable)", true, ledger, false, false, true},
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
		for _, capable := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s [resolve_capable=%v]", tc.name, capable), func(t *testing.T) {
				cfg := &config.Config{}
				cfg.Catalog.InstantRecall.Enabled = tc.recall
				cfg.Outcome.LedgerPath = tc.ledgerPath
				cfg.Notify.Slack.FeedbackButtons = tc.slack
				cfg.Notify.Matrix.FeedbackReactions = tc.matrix

				got := RecallDecayWarning(cfg, capable)
				if (got != "") != tc.wantWarn {
					t.Fatalf("RecallDecayWarning(recall=%v, ledger=%q, slack=%v, matrix=%v, capable=%v) = %q, wantWarn = %v",
						tc.recall, tc.ledgerPath, tc.slack, tc.matrix, capable, got, tc.wantWarn)
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
			})
		}
	}
}

// TestRecallDecayWarningStatesWhatActuallyHappens is a DRIFT GUARD over the real
// ledger: it MEASURES what a silent resolve channel does to an entry, then requires
// the warning to say that and not its opposite.
//
// This exists because the first version of the warning claimed one consequence —
// "keeps being recalled at full trust" — for both fingerprint shapes, and that is
// only true for one of them. The measurement below is the evidence:
//
//   - a REAL alert fingerprint is recorded Resolvable=true (resolvability comes from
//     outcome.Derived(fp), never from send_resolved), so Recalls climbs while Resolved
//     stays 0 and the factor drops UNDER the default floor after a single recall. The
//     entry stops being recalled. Telling that operator their entry keeps firing at
//     full trust is precisely backwards.
//   - a DERIVED fingerprint (GitOps, re-investigate) is Resolvable=false and never
//     enters OpenCounts at all, so outcomeGate's fail-safe returns factor 1: that one
//     really does keep firing at full trust.
//
// The keyword assertions are only as good as the measurement they hang off, which is
// the point: if the resolvable rule, the Factor formula or the defaults change so that
// a real fingerprint no longer decays, the MEASUREMENT half fails first and forces
// whoever changed it to revisit the prose.
func TestRecallDecayWarningStatesWhatActuallyHappens(t *testing.T) {
	cfg := &config.Config{}
	cfg.Catalog.InstantRecall.Enabled = true
	cfg.Outcome.LedgerPath = "/var/lib/runlore/catalog/outcomes.jsonl"
	config.ApplyDefaults(cfg)
	prior, floor := cfg.Catalog.InstantRecall.OutcomePrior, cfg.Catalog.InstantRecall.OutcomeFloor

	led, err := outcome.New(filepath.Join(t.TempDir(), "outcomes.jsonl"))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	now := time.Now()
	open := func(fp, entry string) {
		t.Helper()
		resolvable := !outcome.Derived(fp) // exactly what investigate.go stamps
		if err := led.Open(outcome.Event{
			Fingerprint: fp, Kind: "recall", Entry: entry,
			Resolvable: &resolvable, At: now, StartedAt: now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("open %s: %v", fp, err)
		}
	}

	// A real alert fingerprint, recalled once, never resolved.
	const realEntry = "kb/real.md"
	open("a1b2c3d4e5f6a7b8", realEntry)
	counts, err := led.OpenCounts()
	if err != nil {
		t.Fatalf("open counts: %v", err)
	}
	agg, seen := counts[realEntry]
	if !seen {
		t.Fatalf("a resolvable recall must enter OpenCounts; got %v", counts)
	}
	factor := agg.Factor(prior)
	if factor >= floor {
		t.Fatalf("MEASUREMENT CHANGED: one unresolved recall now scores %.4f, at or above the "+
			"floor %.2f (prior %.1f) — the warning's 'stops firing' claim may no longer hold; "+
			"re-measure before editing the prose", factor, floor, prior)
	}

	// A derived fingerprint, recalled once: never counted, so never decays.
	const derivedEntry = "kb/gitops.md"
	open(outcome.DeriveFingerprint(outcome.GitOpsFingerprintPrefix, "ns/app|Failed"), derivedEntry)
	counts, err = led.OpenCounts()
	if err != nil {
		t.Fatalf("open counts: %v", err)
	}
	if _, seen := counts[derivedEntry]; seen {
		t.Fatalf("MEASUREMENT CHANGED: a derived-fingerprint recall now enters OpenCounts — the "+
			"warning's 'full trust' claim may no longer hold; got %v", counts[derivedEntry])
	}

	// Now hold the prose to both measured outcomes. Both are reachable wherever a
	// webhook source is enabled (a deployment may run Alertmanager AND the GitOps
	// watcher), so the hedged variant must name both.
	hedged := RecallDecayWarning(cfg, true)
	if hedged == "" {
		t.Fatal("expected the warning to fire for recall + ledger + no feedback")
	}
	for _, want := range []string{
		"outcome_floor", // the decay-to-rejection case, measured above
		"STOPS FIRING",
		"full trust", // the derived-fingerprint case, also measured above
	} {
		if !strings.Contains(hedged, want) {
			t.Errorf("hedged warning must state the measured consequence %q: %q", want, hedged)
		}
	}
	// The failure this guard was written for: naming ONLY the benign case. A message
	// that says trust never moves is the opposite of the measurement above.
	lower := strings.ToLower(hedged)
	for _, forbidden := range []string{
		"confidence never moves",
		"trust never moves",
		"never decays",
		"no entry is ever proposed for retirement: a knowledge entry that has stopped working keeps being recalled at full trust",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("warning claims %q, contradicting the measured decay-to-rejection case: %q", forbidden, hedged)
		}
	}

	// With no webhook source enabled, the decay case is UNREACHABLE — every
	// fingerprint is derived — so the strong variant must not threaten the operator
	// with a rejection that cannot happen to them.
	strong := RecallDecayWarning(cfg, false)
	if !strings.Contains(strong, "full trust") {
		t.Errorf("watcher-only warning must state the full-trust consequence: %q", strong)
	}
	if strings.Contains(strong, "STOPS FIRING") || strings.Contains(strong, "outcome_floor") {
		t.Errorf("watcher-only warning must not threaten a floor rejection that cannot occur "+
			"without a resolvable fingerprint: %q", strong)
	}
}

// TestRecallDecayWarningHedgesOnlyWhereItMust splits the honesty question in two.
//
// Where a webhook source IS enabled, whether resolves actually arrive is unknowable:
// Alertmanager's `send_resolved` lives in the operator's receiver config, which
// RunLore never reads, and a custom mapping's `resolved` field is per-instance. The
// message may say the resolve channel is the only one LEFT and that we cannot see it;
// it must not claim resolves never arrive. Telling a correctly-configured operator
// their loop is broken is the lie that trains people to ignore the line.
//
// Where NO webhook source is enabled the opposite applies: a watcher's Watch returns
// investigation requests only, so no resolve can arrive by construction. Hedging there
// understates a fact RunLore can prove, and hedged warnings get ignored too.
func TestRecallDecayWarningHedgesOnlyWhereItMust(t *testing.T) {
	cfg := &config.Config{}
	cfg.Catalog.InstantRecall.Enabled = true
	cfg.Outcome.LedgerPath = "/var/lib/runlore/catalog/outcomes.jsonl"

	hedged := RecallDecayWarning(cfg, true)
	if !strings.Contains(hedged, "cannot tell from here") {
		t.Errorf("with a webhook source enabled the warning must hedge on the resolve channel "+
			"it cannot observe: %q", hedged)
	}
	if !strings.Contains(hedged, "safe to ignore") {
		t.Errorf("the hedged warning must tell an operator whose source DOES resolve that they "+
			"are fine, or it reads as a false alarm: %q", hedged)
	}
	// Phrasings that would state the unobservable as fact. These target claims about
	// whether resolves ARRIVE, which is what RunLore cannot see. They deliberately do
	// NOT forbid "no resolve channel": that describes GitOps/re-investigate sources,
	// whose synthetic fingerprints no resolve can match by construction — a proven
	// fact, and one the message needs. A blunter blacklist ("no resolve") rejected the
	// true statement along with the false ones.
	lower := strings.ToLower(hedged)
	for _, forbidden := range []string{
		"resolves never arrive",
		"no resolves arrive",
		"sends no resolves",
		"does not send resolves",
		"your source will not",
		"will stay empty",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("warning asserts %q, which this process cannot determine: %q", forbidden, hedged)
		}
	}

	// The watcher-only variant is entitled to assert it, and should: the hedge would
	// be false modesty about something the source contract settles.
	strong := RecallDecayWarning(cfg, false)
	if strings.Contains(strong, "cannot tell from here") {
		t.Errorf("with no webhook source enabled, resolvability IS determinable — the warning "+
			"must not hedge: %q", strong)
	}
	if !strings.Contains(strong, "no enabled source can deliver a resolve event") {
		t.Errorf("watcher-only warning must state the proven fact it rests on: %q", strong)
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

// TestCostCeilingWithoutPricingWarning covers the (ceiling set × pricing shape)
// matrix. Every rate card that cannot price what the provider actually bills warns;
// a complete rate card, and no ceiling at all, stay silent — a warning that fired on a
// correct config would be muted within a day and take the real ones with it.
//
// The PARTIAL cases are what the all-zero check missed. cached_input_usd_per_mtok is
// the most commonly forgotten of the three, and all three model providers RunLore
// speaks report cache reads (anthropic CacheReadInputTokens, openai
// prompt_tokens_details.cached_tokens, gemini cachedContentTokenCount), so leaving it
// at 0 prices every cache read at $0: on a cache-heavy run that under-estimates the
// input term several-fold, the ceiling permits materially more than its number, and
// the footer plus runlore_investigation_cost_usd report a confident wrong figure. Same
// "looks instrumented for spend when it is not" failure the all-zero warning catches.
func TestCostCeilingWithoutPricingWarning(t *testing.T) {
	rates := &config.Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CachedInputUSDPerMTok: 0.3}
	zeroed := &config.Pricing{}
	noCached := &config.Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15}

	tests := []struct {
		name     string
		ceiling  float64
		pricing  *config.Pricing
		wantWarn bool
		wantSays string // a phrase the message must carry, naming the specific dead config
	}{
		{"no ceiling, no pricing", 0, nil, false, ""},
		{"no ceiling, with pricing", 0, rates, false, ""},
		{"no ceiling, partial rates", 0, noCached, false, ""},
		{"ceiling with a complete rate card", 2.5, rates, false, ""},
		{"ceiling, no pricing at all", 2.5, nil, true, "model.pricing is not configured"},
		{"ceiling, all rates zero", 2.5, zeroed, true, "every model.pricing rate is 0"},
		{"ceiling, cached rate omitted", 2.5, noCached, true, "cached_input_usd_per_mtok"},
		{"ceiling, input rate omitted", 2.5,
			&config.Pricing{OutputUSDPerMTok: 15, CachedInputUSDPerMTok: 0.3}, true, "input_usd_per_mtok"},
		{"ceiling, output rate omitted", 2.5,
			&config.Pricing{InputUSDPerMTok: 3, CachedInputUSDPerMTok: 0.3}, true, "output_usd_per_mtok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Investigation.MaxCostPerInvestigation = tc.ceiling
			cfg.Model.Pricing = tc.pricing
			got := CostCeilingWithoutPricingWarning(cfg)
			if (got != "") != tc.wantWarn {
				t.Fatalf("CostCeilingWithoutPricingWarning(ceiling=%v, pricing=%+v) = %q, wantWarn = %v",
					tc.ceiling, tc.pricing, got, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			if !strings.Contains(got, tc.wantSays) {
				t.Fatalf("message must name the specific dead configuration (%q): %q", tc.wantSays, got)
			}
			// Every warning must carry the key at issue and a way out; a paragraph
			// that only says "this is wrong" costs an operator a docs hunt.
			for _, want := range []string{"max_cost_per_investigation", "model.pricing"} {
				if !strings.Contains(got, want) {
					t.Fatalf("message missing %q — it must name the key, the missing input and the consequence: %q", want, got)
				}
			}
		})
	}
}

// TestPartialPricingWarningNamesEveryMissingRate pins that the message lists ALL the
// rates left at 0, not just the first one found, and accuses none that are set. An
// operator who fixes the one rate the warning named and restarts into the same warning
// learns to distrust it.
func TestPartialPricingWarningNamesEveryMissingRate(t *testing.T) {
	cfg := &config.Config{}
	cfg.Investigation.MaxCostPerInvestigation = 2.5
	cfg.Model.Pricing = &config.Pricing{OutputUSDPerMTok: 15}
	got := CostCeilingWithoutPricingWarning(cfg)
	// The accusation clause must list both unset rates and neither the set one. Pinned
	// as one phrase rather than three Contains calls because the remedy sentence names
	// all three keys by design — only the accusation may be selective.
	const want = "leaves input_usd_per_mtok and cached_input_usd_per_mtok at 0"
	if !strings.Contains(got, want) {
		t.Fatalf("the warning must accuse exactly the rates left at 0, want it to say %q: %q", want, got)
	}
}

// TestCostCeilingWarningIgnoresVerifyPricing pins that model.verify.pricing cannot
// silence the warning. It only overrides the VERIFY pass's rates and inherits
// model.pricing when unset, so it can neither price an otherwise-unpriced
// investigation nor rescue an all-zero main rate card — a deployment that sets it
// alone still has a ceiling that never fires.
func TestCostCeilingWarningIgnoresVerifyPricing(t *testing.T) {
	cfg := &config.Config{}
	cfg.Investigation.MaxCostPerInvestigation = 2.5
	cfg.Model.Verify = &config.ModelOverride{Pricing: &config.Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15}}
	if got := CostCeilingWithoutPricingWarning(cfg); got == "" {
		t.Fatal("model.verify.pricing alone leaves the loop unpriced — the ceiling still cannot fire, so the warning must stand")
	}
}
