// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"
)

// ModelProvider returns the configured model provider name (default "openai").
func ModelProvider(cfg *config.Config) string {
	switch cfg.Model.Provider {
	case "anthropic", "gemini":
		return cfg.Model.Provider
	default:
		return "openai"
	}
}

// ModelConfigured reports whether a usable model is configured: a provider with a
// built-in default endpoint (anthropic, gemini), or any provider with a base_url.
func ModelConfigured(cfg *config.Config) bool {
	switch cfg.Model.Provider {
	case "anthropic", "gemini":
		return true
	default:
		return cfg.Model.BaseURL != ""
	}
}

// CatalogConfigured reports whether the operator asked for a knowledge catalog
// (a mounted dir or a git-sync repo). It is independent of whether the load
// succeeded.
func CatalogConfigured(cfg *config.Config) bool {
	return cfg.Catalog.Dir != "" || cfg.Catalog.Git.URL != ""
}

// CatalogExpected reports whether readiness should gate on a built catalog: a
// catalog source AND a usable model. With a model, a configured-but-failed
// catalog (BuildCatalog returns nil) keeps the pod at /readyz 503 instead of
// collapsing readiness to pure leadership and serving incidents with no KB.
// Without a model, BuildInvestigator skips the catalog entirely (log-only
// investigator, cat=nil by design, not by failure), so gating on it would hold
// every replica — leader included — at 503 forever.
func CatalogExpected(cfg *config.Config) bool {
	return CatalogConfigured(cfg) && ModelConfigured(cfg)
}

// RequireWebhookAuth fails closed on the serve path when the LLM investigator is
// wired but the alert webhook is anonymous. The webhook's labels/annotations flow
// verbatim into the LLM prompt (and bill the model), so an unauthenticated caller
// must not reach it once a model is configured — regardless of actions.mode. This
// lives on the serve path, NOT in config.Validate: Validate is shared by every
// subcommand (e.g. `lore investigate` legitimately needs a model and has no
// webhook), so the requirement is scoped to where the webhook is actually served.
// It mirrors the approval-token fail-closed guard.
func RequireWebhookAuth(cfg *config.Config, webhookToken string) error {
	if ModelConfigured(cfg) && webhookToken == "" {
		return fmt.Errorf("model configured but server.webhook_token_env (%q) is empty: refusing to start with an unauthenticated alert webhook (fail closed)",
			cfg.Server.WebhookTokenEnv)
	}
	return nil
}

// WebhookAuthWarning decides the startup warning for an unauthenticated alert
// webhook, returning "" when no warning is warranted. Unlike RequireWebhookAuth
// (which only fails closed once a model is billed per request) and
// config.Validate (which only hard-fails actions.mode=auto), an empty token is a
// DELIBERATE fail-open default outside those cases — cluster-internal deployments
// legitimately skip it. That default must not be silent: whenever the alertmanager
// source is enabled with no token, anyone who can reach the Service can trigger
// investigations (and, under an executing rung, actions a human then approves)
// regardless of whether a model is configured yet. PagerDuty is deliberately
// excluded — it authenticates via its own X-PagerDuty-Signature scheme (see
// RequirePagerDutyAuth), so an empty shared token says nothing about its exposure.
// actions.mode=approve escalates the wording (a human is now clicking "run" off of
// webhook-supplied data) but stays a warning, not an error — auto already hard-fails
// via config.Validate, and off/suggest carry no execution risk to call out.
func WebhookAuthWarning(alertmanagerEnabled bool, webhookToken string, mode config.ActionMode) string {
	if !alertmanagerEnabled || webhookToken != "" {
		return ""
	}
	if mode == config.ActionApprove {
		return "alert webhook is UNAUTHENTICATED (server.webhook_token_env unset) while actions.mode=approve: " +
			"anyone who can reach the Service can trigger paid investigations whose actions a human then approves " +
			"— set a token unless the endpoint is provably unreachable beyond trusted networks (docs/security-model.md)"
	}
	return "alert webhook is UNAUTHENTICATED (server.webhook_token_env unset): fine for cluster-internal traffic, " +
		"but anyone who can reach the Service can trigger paid investigations — set a token if the endpoint is " +
		"reachable beyond trusted networks (docs/security-model.md)"
}

// ResolveCapableSource reports whether any ENABLED source could deliver a resolve
// event at all. It reads the adapter CONTRACT, never a hardcoded list of names, so
// it cannot drift from source.Registered(): a Webhook source's Decode returns
// source.DecodeResult, which carries Resolved; a Watcher's Watch returns a channel
// of investigation requests and has nowhere to put a resolution. A deployment whose
// only enabled sources are watchers (GitOps failures) therefore PROVABLY has no
// resolve channel — as does the re-investigate poller, which is not a source at all
// and derives its own synthetic fingerprints.
//
// "Capable", not "will": for an enabled webhook source, whether the operator wired
// resolves through (Alertmanager's send_resolved, a custom mapping's `resolved`
// field) stays outside RunLore's config and outside this answer.
func ResolveCapableSource(built []source.Built) bool {
	for _, b := range built {
		if b.Desc.Kind == source.Webhook {
			return true
		}
	}
	return false
}

// RecallDecayWarning decides the startup warning for a learning loop whose feedback
// edge cannot be shown to accumulate ground truth, returning "" when no warning is
// warranted. resolveCapable comes from ResolveCapableSource and decides how much the
// warning is entitled to claim.
//
// Instant recall's Gate 3 weighs a candidate entry by its recorded track record and
// is deliberately fail-safe: an entry the outcome ledger has never seen scores factor
// 1 and fires (absence of evidence must never block a recall). A ledger that
// accumulates no ground truth therefore degrades that gate — and the retirement pass,
// which shares the same factor and floor — but NOT uniformly, and that difference is
// the whole reason there are two messages below.
//
// Ground truth reaches the ledger through three paths, only one of which is fully
// observable from here:
//
//   - Human 👍/👎 feedback — opt-in on both notifiers, so its state IS in this config.
//   - Resolve events from the incident source. Whether a resolve can EVER arrive is
//     determinable (ResolveCapableSource, off the registry's Kind); whether it
//     actually does is not — Alertmanager's `send_resolved` lives in the operator's
//     receiver config, which RunLore never reads.
//   - Machine confirmations (outcome.Ledger.Confirm) — a FRESH investigation that
//     re-derives an existing entry's DupFingerprint. Wired unconditionally on the
//     serve path (cur.Confirmations in investigate.go), it needs no feedback channel
//     and no resolve, and folds into the same Aggregate.Factor at confirmWeight. So
//     the gate is never a no-op for literally every entry, and neither message claims
//     it is: confirms are recovery evidence only — they can lift an entry, never sink
//     one, and retirement ignores them (its observation count is recalls + votes).
//
// What actually happens when the resolve channel is silent depends on the FINGERPRINT,
// and the two cases fail in OPPOSITE directions:
//
//   - A synthetic fingerprint (GitOps failure, re-investigate poll) is recorded
//     Resolvable=false, so applyOpenLocked never counts it. The entry never enters
//     OpenCounts, outcomeGate returns (1, true), and it keeps firing at full trust
//     however wrong it has become.
//   - A REAL alert fingerprint is recorded Resolvable=true whether or not a resolve
//     ever follows: resolvability is derived from the fingerprint prefix alone
//     (`resolvable := !outcome.Derived(fp)` in investigate.go), never from
//     send_resolved, which RunLore cannot see. Each recall increments Recalls while
//     Resolved stays 0, the factor falls monotonically, and the entry is REJECTED
//     once it crosses outcome_floor — one unresolved recall already suffices at the
//     defaults (prior 2, floor 0.5 ⇒ factor 1/3). A rejected recall records nothing,
//     so it stays there; and because retirement counts recalls, the frozen count
//     never reaches min_observations either. The entry is neither used nor retired.
//
// A message naming only the first case would tell an Alertmanager operator the
// opposite of what will happen to them, so both are named whenever both are possible.
// The warning never hard-fails: a deployment whose source does send resolves is
// correctly configured, and the hedged variant says so outright.
//
// Silent when instant recall is off (nothing recalls, so there is no trust to decay)
// or when outcome.ledger_path is unset (the operator turned the learning loop off,
// which `lore curate` likewise reports as an info, not a warning).
func RecallDecayWarning(cfg *config.Config, resolveCapable bool) string {
	if !cfg.Catalog.InstantRecall.Enabled || cfg.Outcome.LedgerPath == "" {
		return ""
	}
	if cfg.Notify.Slack.FeedbackButtons || cfg.Notify.Matrix.FeedbackReactions {
		return ""
	}
	const fix = "Turn on notify.slack.feedback_buttons or notify.matrix.feedback_reactions, and keep " +
		"outcome.ledger_path on a persistent volume so what it records survives a restart " +
		"(docs/concepts/learning-loop.md)"
	if !resolveCapable {
		// No hedge is warranted here: every enabled source is a watcher, so no resolve
		// can reach the ledger by construction and only the full-trust case is reachable.
		return "instant recall is enabled with an outcome ledger, but nothing can write ground truth to " +
			"it: no feedback channel is on (notify.slack.feedback_buttons and " +
			"notify.matrix.feedback_reactions are both off) and no enabled source can deliver a resolve " +
			"event at all — watcher sources (GitOps failures) and the re-investigate poller carry " +
			"synthetic fingerprints no resolved-alert webhook can ever match. Recalled entries therefore " +
			"stay at full trust indefinitely: outcome decay never rejects one and retirement never " +
			"proposes one, so an entry that has stopped working keeps being recalled and nothing surfaces " +
			"it for review. A human 👍/👎 is the only ground truth this deployment can accumulate. " + fix
	}
	return "instant recall is enabled with an outcome ledger, but no feedback channel is on " +
		"(notify.slack.feedback_buttons and notify.matrix.feedback_reactions are both off), leaving a " +
		"resolved-alert webhook from your incident source as the only ground truth that can move an " +
		"entry's trust — and whether yours sends those is not in this config, so RunLore cannot tell " +
		"from here. If they arrive, decay works and this line is safe to ignore. If they do not, trust " +
		"goes wrong in BOTH directions. An entry recalled against a real alert fingerprint has the " +
		"recall counted and the resolve never recorded, so its factor falls until it crosses " +
		"catalog.instant_recall.outcome_floor and instant recall STOPS FIRING it — one unresolved " +
		"recall already suffices at the defaults, and a rejected recall records nothing, so it stays " +
		"there. An entry recalled from a source with no resolve channel (GitOps failures, " +
		"re-investigate polls) never enters the ledger roll-up at all and keeps being recalled at full " +
		"trust however stale it is. Neither reaches retirement, so nothing surfaces either failure for " +
		"review. " + fix
}

// RequirePagerDutyAuth is the PagerDuty analogue of RequireWebhookAuth. The
// PagerDuty source authenticates /webhook/pagerduty with its own
// X-PagerDuty-Signature verification (not the shared server.webhook_token_env
// bearer token), so the shared guard does not cover it. When the source is
// enabled and a model is configured, it must carry a signing secret — otherwise
// an unauthenticated caller could drive (and bill) the model. Fail closed on the
// serve path only, mirroring RequireWebhookAuth.
func RequirePagerDutyAuth(cfg *config.Config, enabled bool, secret string) error {
	if enabled && ModelConfigured(cfg) && secret == "" {
		return fmt.Errorf("model configured and sources.pagerduty enabled but its secret_env is empty: refusing to start with an unauthenticated PagerDuty webhook (fail closed)")
	}
	return nil
}

// OutcomeKind labels an outcome-ledger open as a recall (cache hit) or a fresh finding.
func OutcomeKind(recalled bool) string {
	if recalled {
		return "recall"
	}
	return "fresh"
}

// GitopsEngine returns the configured GitOps engine, defaulting to flux. Anything it
// does not recognise resolves to flux SILENTLY, which is why nothing may state a fact
// about the cluster from it — see UnknownGitopsEngineWarning and WiredGitopsEngine.
func GitopsEngine(cfg *config.Config) string {
	if cfg.GitOps.Engine == "argocd" {
		return "argocd"
	}
	return "flux"
}

// WiredGitopsEngine returns the engine of the GitOps provider actually built, falling
// back to the configured value when the provider does not report one (a nil provider, or
// a fake in a test).
//
// The distinction matters because the tool descriptions are read by the model on every
// turn: sourcing the engine from a config string nothing validates means "argo" or
// "ArgoCD" silently describes an Argo CD deployment as running Flux. The provider was
// selected by the same config, so this usually agrees — but when it does not, the
// provider is the one that will answer the calls.
func WiredGitopsEngine(gp providers.GitOpsProvider, cfg *config.Config) string {
	if r, ok := gp.(providers.GitOpsEngineReporter); ok {
		return string(r.GitOpsEngine())
	}
	return GitopsEngine(cfg)
}

// UnknownGitopsEngineWarning decides the startup warning for a gitops.engine value
// nothing recognises, returning "" when the value is valid or absent.
//
// GitopsEngine folds every unrecognised value into flux without a word, so a deployment
// that wrote "argo", "ArgoCD" or "argo-cd" runs the whole Flux path: the Flux provider,
// controller_logs registered, and the GitOps tools advertising Kustomization/HelmRelease
// on a cluster that has no Flux CRDs. That is runlore#503's exact setup, reached by a
// typo, and it is silent in every log. Config validation does not reject it (the field is
// free-form and the default is meaningful), so the operator gets told instead.
func UnknownGitopsEngineWarning(engine string) string {
	if engine == "" || engine == "flux" || engine == "argocd" {
		return ""
	}
	return fmt.Sprintf("gitops.engine=%q is not a recognised engine (want %q or %q) — falling back to "+
		"flux. If this cluster runs Argo CD, every GitOps tool is now scoped to Flux kinds it can "+
		"never find, and controller_logs is registered against controllers that do not exist.",
		engine, "flux", "argocd")
}

// CostCeilingWithoutPricingWarning decides the startup warning for a cost ceiling
// that cannot fire, returning "" when no warning is warranted.
//
// investigation.max_cost_per_investigation is compared against the investigation's
// running estimated spend, and that estimate exists only when model.pricing supplies
// token rates: aggregateUsage sets UsageTotals.Priced (and a CostUSD worth reading)
// off cfg.Model.Pricing alone. So a ceiling configured without pricing is not a
// stricter limit that rarely fires — it is structurally incapable of firing, for
// every investigation, forever. An operator who wrote a dollar figure into their
// config has stated an intent to cap spend; leaving that intent silently inert is the
// exact "a limit that is neither enforced nor signalled" shape this warning exists to
// break. It stays a warning rather than a hard failure because the rest of the
// deployment is perfectly valid: unpriced token accounting is a supported mode, and
// refusing to start would punish a config that is merely incomplete.
//
// Three dead-or-lying configurations, three messages, because the fix differs:
//
//   - No model.pricing at all — nothing is priced, so add the rates.
//   - model.pricing present but every rate 0 — cost is computed and is always exactly
//     $0.00, which is under any positive ceiling. This one is more dangerous than the
//     first: the finding footer prints "~$0.00" and the investigation_cost_usd
//     metric reports zero, so the deployment LOOKS instrumented for cost. A rate card
//     of all zeros is a placeholder someone meant to fill in.
//   - model.pricing present with SOME rate at 0 — the ceiling fires, but late, and by
//     an amount nothing reveals. Every token class RunLore counts is billed by every
//     provider it speaks: cost() prices uncached input, cached input and output
//     separately (internal/investigate/cost.go), and all three clients populate
//     CachedInputTokens (anthropic CacheReadInputTokens, openai
//     prompt_tokens_details.cached_tokens, gemini cachedContentTokenCount). So a rate
//     left at 0 does not mean "this class is free", it means a whole class of real
//     spend is estimated at $0 — the most commonly forgotten being
//     cached_input_usd_per_mtok, which on a cache-heavy run understates the input term
//     several-fold while the footer and the metric report a confident wrong number.
//     This is the all-zero case's own reasoning applied to a rate card that is merely
//     incomplete rather than empty.
//
// Why warn rather than refuse to price (leaving UsageTotals.Priced false, so the
// ceiling goes inert): that would trade a ceiling that binds late for no ceiling at
// all, silently, which is precisely the failure the first two messages exist to
// announce. An incomplete rate card still yields a real, comparable number — it is
// just an under-estimate — so the honest move is to keep enforcing it and tell the
// operator which rate is making it loose. A deployment whose provider genuinely does
// not bill a class separately can silence the warning by setting that rate to the one
// it shares (e.g. cached_input_usd_per_mtok = input_usd_per_mtok), which costs it
// nothing because the corresponding token count is then always 0.
//
// model.verify.pricing is deliberately not consulted: it only overrides the verify
// pass's rates and inherits model.pricing when unset, so it can neither make an
// unpriced investigation priced nor rescue an all-zero main rate card.
func CostCeilingWithoutPricingWarning(cfg *config.Config) string {
	if cfg.Investigation.MaxCostPerInvestigation <= 0 {
		return ""
	}
	const fix = "Set model.pricing (input_usd_per_mtok, output_usd_per_mtok, cached_input_usd_per_mtok) " +
		"to your provider's rates, or drop the ceiling and bound spend with " +
		"investigation.max_tokens_per_investigation instead (docs/configuration/configuration.md)"
	p := cfg.Model.Pricing
	if p == nil {
		return fmt.Sprintf("investigation.max_cost_per_investigation is set ($%g) but model.pricing is not "+
			"configured, so NO cost is ever computed and the ceiling can never fire — this deployment has no "+
			"spend limit denominated in currency. "+fix, cfg.Investigation.MaxCostPerInvestigation)
	}
	if p.InputUSDPerMTok == 0 && p.OutputUSDPerMTok == 0 && p.CachedInputUSDPerMTok == 0 {
		return fmt.Sprintf("investigation.max_cost_per_investigation is set ($%g) but every model.pricing rate "+
			"is 0, so every investigation costs exactly $0.00 and the ceiling can never fire — while the "+
			"notification footer and runlore_investigation_cost_usd both report a cost, making the deployment "+
			"look instrumented for spend when it is not. "+fix, cfg.Investigation.MaxCostPerInvestigation)
	}
	if missing := unpricedRates(p); len(missing) > 0 {
		return fmt.Sprintf("investigation.max_cost_per_investigation is set ($%g) but model.pricing leaves %s "+
			"at 0, so every token of that kind is estimated at $0.00 — the ceiling still fires, but only after "+
			"materially more real spend than its number, and the notification footer and "+
			"runlore_investigation_cost_usd both report the same under-estimate. Every provider RunLore speaks "+
			"bills all three token classes and reports them separately. "+fix,
			cfg.Investigation.MaxCostPerInvestigation, strings.Join(missing, " and "))
	}
	return ""
}

// unpricedRates names the model.pricing keys left at 0, in the order they appear in
// the config, so the warning can list every one rather than sending the operator round
// the loop once per rate.
func unpricedRates(p *config.Pricing) []string {
	var missing []string
	for _, r := range []struct {
		key  string
		rate float64
	}{
		{"input_usd_per_mtok", p.InputUSDPerMTok},
		{"output_usd_per_mtok", p.OutputUSDPerMTok},
		{"cached_input_usd_per_mtok", p.CachedInputUSDPerMTok},
	} {
		if r.rate == 0 {
			missing = append(missing, r.key)
		}
	}
	return missing
}
