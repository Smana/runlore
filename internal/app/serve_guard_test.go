// SPDX-License-Identifier: Apache-2.0

package app

// TestServeGuardFailClosed locks the fail-closed invariant for RequireWebhookAuth
// on the serve startup path (internal/app/config.go): a model-configured deployment
// with an empty webhook token must refuse to start; a non-empty token must be
// accepted. This is entirely self-contained — it constructs configs in-code so it
// does not depend on any YAML file that another workstream may be editing.
//
// The invariant being locked: once a model is wired, every alert webhook POST
// drives a paid LLM call, so an anonymous webhook would let anyone in the network
// run up an arbitrary bill (or poison the investigation history). The guard lives
// on the serve path only (not in config.Validate) because `lore investigate`
// legitimately needs a model without a webhook.
import (
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func TestServeGuardFailClosed(t *testing.T) {
	t.Run("model configured, no webhook token → refused (fail closed)", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Model.Provider = "anthropic" // built-in endpoint; no base_url needed
		// Server.WebhookTokenEnv names the env var — it is set here to show intent,
		// but the resolved token (second arg) is what matters for the guard.
		cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"

		if err := RequireWebhookAuth(cfg, "" /* empty resolved token */); err == nil {
			t.Fatal("RequireWebhookAuth must refuse when model is configured and webhook token is empty")
		}
	})

	t.Run("model configured, webhook token set → accepted", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Model.Provider = "anthropic"
		cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"

		if err := RequireWebhookAuth(cfg, "some-secret-token"); err != nil {
			t.Fatalf("RequireWebhookAuth must accept when model is configured and webhook token is non-empty: %v", err)
		}
	})

	t.Run("no model, no webhook token → accepted (log-only investigator has no billing exposure)", func(t *testing.T) {
		cfg := &config.Config{} // zero Model: no provider, no base_url
		if err := RequireWebhookAuth(cfg, ""); err != nil {
			t.Fatalf("RequireWebhookAuth must accept when no model is configured: %v", err)
		}
	})
}
