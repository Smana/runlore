// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"time"
)

// Factory configures the agent-factory integration: RunLore files each finding
// as a task with the factory's API and follows it through the broker's signed
// webhooks. Secrets are env var NAMES, never values. An empty URL turns the
// whole integration off.
type Factory struct {
	URL             string          `yaml:"url"`               // factory API base URL; empty disables the integration
	TokenEnv        string          `yaml:"token_env"`         // env var holding a static Bearer token (mutually exclusive with OAuth2 fields)
	TokenURL        string          `yaml:"token_url"`         // OAuth2 token endpoint (client credentials); https or loopback/in-cluster
	CAFile          string          `yaml:"ca_file"`           // optional PEM of a private CA, trusted besides the system roots, for factory.url and token_url only
	ClientIDEnv     string          `yaml:"client_id_env"`     // env var holding the client id
	ClientSecretEnv string          `yaml:"client_secret_env"` // env var holding the client secret
	ProjectIDEnv    string          `yaml:"project_id_env"`    // env var holding the ZITADEL project id (emits ZITADEL audience scope; mutually exclusive with oauth_scope)
	OAuthScope      string          `yaml:"oauth_scope"`       // optional OAuth2 scope for generic client credentials (omitted when empty; mutually exclusive with project_id_env)
	OAuthAudience   string          `yaml:"oauth_audience"`    // optional OAuth2 audience form parameter for generic client credentials (omitted when empty)
	Repository      string          `yaml:"repository"`        // owner/name the factory works on
	MaxCostUSD      float64         `yaml:"max_cost_usd"`      // per-task cost ceiling sent with each task; must be above 0
	Webhook         FactoryWebhook  `yaml:"webhook"`           // the inbound webhook receiver
	Soak            Duration        `yaml:"soak"`              // how long after a merge before the alert is checked; 0 => 30m (ApplyDefaults), at least 5m
	Verify          FactoryVerify   `yaml:"verify"`            // the post-merge check; an empty alertmanager_url leaves verification off
	Curation        FactoryCuration `yaml:"curation"`          // defers an incident's KB entry until its fix is verified
}

// FactoryCuration configures deferring the curated entry of an incident the factory
// is fixing: the entry is drafted once the fix has an outcome, or the window ends.
type FactoryCuration struct {
	Defer  bool     `yaml:"defer"`  // hold the entry until the fix is verified; needs the ledger, Alertmanager and a GitHub KB repo
	Window Duration `yaml:"window"` // how long to wait for an outcome after the factory accepts the task; 0 => 7d (ApplyDefaults), at least 24h
}

// minCurationWindow covers a 30m soak, alert-rules' 2h and a same-day publish and merge.
const minCurationWindow = 24 * time.Hour

// validateCuration checks factory.curation against the blocks validateFactory cannot see.
func (c *Config) validateCuration() error {
	cu := c.Factory.Curation
	if cu.Window != 0 && cu.Window.Std() < minCurationWindow {
		return fmt.Errorf("factory.curation.window must be at least 24h, got %v", cu.Window.Std())
	}
	if !cu.Defer {
		return nil
	}
	for _, need := range []struct{ key, val string }{
		{"factory.url", c.Factory.URL},
		{"outcome.ledger_path", c.Outcome.LedgerPath},
		{"factory.verify.alertmanager_url", c.Factory.Verify.AlertmanagerURL},
		{"forge.kb_repo", c.Forge.KBRepo},
	} {
		if need.val == "" {
			return fmt.Errorf("factory.curation.defer needs %s", need.key)
		}
	}
	if c.Forge.Provider != "" && c.Forge.Provider != "github" {
		return fmt.Errorf("factory.curation.defer needs forge.provider github, got %q", c.Forge.Provider)
	}
	return nil
}

// FactoryVerify configures the check RunLore makes once a fix has soaked.
type FactoryVerify struct {
	AlertmanagerURL string `yaml:"alertmanager_url"` // Alertmanager base URL to ask whether the alert still fires; empty turns verification off
	MaxSteps        int    `yaml:"max_steps"`        // step ceiling of the short re-investigation; 0 => 6 (ApplyDefaults)
}

// minFactorySoak is the shortest soak accepted: a verdict taken sooner would read the
// alert before the fix could have taken effect.
const minFactorySoak = 5 * time.Minute

// FactoryWebhook configures the receiver of the broker's Standard Webhooks.
type FactoryWebhook struct {
	SecretEnv string `yaml:"secret_env"` // env var holding one or two base64 signing keys, one per line
}

// Enabled reports whether the factory integration is configured.
func (f Factory) Enabled() bool { return f.URL != "" }

// validateFactory checks the factory block when it is enabled.
func validateFactory(f Factory) error {
	if !f.Enabled() {
		return nil
	}
	if f.TokenEnv != "" {
		var conflicts []string
		for _, kv := range []struct{ name, val string }{
			{"factory.token_url", f.TokenURL},
			{"factory.client_id_env", f.ClientIDEnv},
			{"factory.client_secret_env", f.ClientSecretEnv},
			{"factory.project_id_env", f.ProjectIDEnv},
			{"factory.oauth_scope", f.OAuthScope},
			{"factory.oauth_audience", f.OAuthAudience},
		} {
			if kv.val != "" {
				conflicts = append(conflicts, kv.name)
			}
		}
		if len(conflicts) > 0 {
			return fmt.Errorf("factory.token_env cannot be combined with OAuth2 settings (%s)", strings.Join(conflicts, ", "))
		}
		if err := checkSecureKeyEndpoint("factory.url", "factory.token_env", f.URL, f.TokenEnv); err != nil {
			return err
		}
	} else {
		if f.TokenURL == "" {
			return fmt.Errorf("factory.token_url is required when factory.url is set (or set factory.token_env for a static bearer token)")
		}
		for _, kv := range []struct{ name, val string }{
			{"factory.client_id_env", f.ClientIDEnv},
			{"factory.client_secret_env", f.ClientSecretEnv},
		} {
			if kv.val == "" {
				return fmt.Errorf("%s is required when factory.url is set", kv.name)
			}
		}
		if f.ProjectIDEnv != "" && f.OAuthScope != "" {
			return fmt.Errorf("factory.project_id_env and factory.oauth_scope cannot both be set (ambiguous OAuth2 scope)")
		}
		if err := checkSecureKeyEndpoint("factory.token_url", "factory.client_secret_env", f.TokenURL, f.ClientSecretEnv); err != nil {
			return err
		}
		if err := checkSecureKeyEndpoint("factory.url", "factory.client_secret_env", f.URL, f.ClientSecretEnv); err != nil {
			return err
		}
	}
	if f.Webhook.SecretEnv == "" {
		return fmt.Errorf("factory.webhook.secret_env is required when factory.url is set")
	}
	if owner, repo, ok := strings.Cut(f.Repository, "/"); !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return fmt.Errorf("factory.repository must be owner/name, got %q", f.Repository)
	}
	if f.MaxCostUSD <= 0 {
		return fmt.Errorf("factory.max_cost_usd must be above 0, got %v", f.MaxCostUSD)
	}
	if f.Soak.Std() < minFactorySoak {
		return fmt.Errorf("factory.soak must be at least 5m, got %v", f.Soak.Std())
	}
	if u := f.Verify.AlertmanagerURL; u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("factory.verify.alertmanager_url must be http(s), got %q", u)
	}
	if f.Verify.MaxSteps <= 0 {
		return fmt.Errorf("factory.verify.max_steps must be above 0, got %d", f.Verify.MaxSteps)
	}
	return nil
}
