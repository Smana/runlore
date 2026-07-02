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
// succeeded: ReadyFunc uses it to keep a configured-but-failed catalog (which
// BuildCatalog returns as nil) from collapsing readiness to pure leadership and
// serving incident traffic with no knowledge base.
func CatalogConfigured(cfg *config.Config) bool {
	return cfg.Catalog.Dir != "" || cfg.Catalog.Git.URL != ""
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
