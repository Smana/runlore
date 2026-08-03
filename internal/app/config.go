// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"

	"github.com/Smana/runlore/internal/config"
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

// RecallDecayWarning decides the startup warning for a learning loop whose
// feedback edge cannot be shown to accumulate ground truth, returning "" when no
// warning is warranted.
//
// Instant recall's Gate 3 weighs a candidate entry by its recorded track record,
// and is deliberately fail-safe: an entry the outcome ledger has never seen scores
// factor 1 and fires (absence of evidence must never block a recall). The
// corollary is that a ledger which never accumulates ground truth turns that gate
// into a silent no-op for EVERY entry — and the retirement pass, which shares the
// same factor and floor, into one that can never propose anything. Trust stops
// being derived from whether the entry actually worked, which is the claim the
// whole learning loop rests on.
//
// Ground truth reaches the ledger through two channels, and only one of them is
// observable from here:
//
//   - Human 👍/👎 feedback — opt-in on both notifiers, so its state IS in this
//     config.
//   - Resolve events from the incident source — NOT determinable at startup.
//     `sources` records which adapters are enabled, not whether they emit
//     resolves, and Alertmanager's `send_resolved` lives in the operator's
//     receiver config, which RunLore never reads. Resolvability is decided
//     per event from the fingerprint at record time (see the ledger.Open call in
//     investigate.go), a runtime fact this function cannot anticipate.
//
// So the warning names the risk it can actually establish — neither feedback
// channel is on, leaving an unverifiable resolve channel as the only remaining
// source of truth — and explicitly does not claim resolves never arrive. It stays
// a warning, never a hard failure: a deployment whose Alertmanager does send
// resolves is correctly configured.
//
// Silent when instant recall is off (nothing recalls, so there is no trust to
// decay) or when outcome.ledger_path is unset (the operator turned the learning
// loop off, which `lore curate` likewise reports as an info, not a warning).
func RecallDecayWarning(cfg *config.Config) string {
	if !cfg.Catalog.InstantRecall.Enabled || cfg.Outcome.LedgerPath == "" {
		return ""
	}
	if cfg.Notify.Slack.FeedbackButtons || cfg.Notify.Matrix.FeedbackReactions {
		return ""
	}
	return "instant recall is enabled with an outcome ledger, but no feedback channel is on " +
		"(notify.slack.feedback_buttons and notify.matrix.feedback_reactions are both off): the only " +
		"remaining way an entry can earn or lose trust is a resolved-alert webhook from your incident " +
		"source, and whether yours sends those is not in this config — RunLore cannot tell from here. " +
		"Where they do not arrive, recall confidence never moves and no entry is ever proposed for " +
		"retirement: a knowledge entry that has stopped working keeps being recalled at full trust. " +
		"Turn on notify.slack.feedback_buttons or notify.matrix.feedback_reactions, and keep " +
		"outcome.ledger_path on a persistent volume so what it records survives a restart " +
		"(docs/concepts/learning-loop.md)"
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

// GitopsEngine returns the configured GitOps engine, defaulting to flux.
func GitopsEngine(cfg *config.Config) string {
	if cfg.GitOps.Engine == "argocd" {
		return "argocd"
	}
	return "flux"
}
