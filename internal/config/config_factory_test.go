// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const factoryBlock = `factory:
  url: http://agent-factory.agent-system.svc.cluster.local:8082
  token_url: https://auth.example.com/oauth/v2/token
  client_id_env: FACTORY_CLIENT_ID
  client_secret_env: FACTORY_CLIENT_SECRET
  project_id_env: FACTORY_PROJECT_ID
  repository: Smana/cloud-native-ref
  max_cost_usd: 2
  webhook: {secret_env: FACTORY_WEBHOOK_KEY}
`

func loadString(t *testing.T, s string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runlore.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestFactoryConfig(t *testing.T) {
	c, err := loadString(t, factoryBlock)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Factory.Enabled() || c.Factory.MaxCostUSD != 2 || c.Factory.Webhook.SecretEnv != "FACTORY_WEBHOOK_KEY" {
		t.Fatalf("%+v", c.Factory)
	}
	if c, err := loadString(t, "logging: {format: json}\n"); err != nil || c.Factory.Enabled() {
		t.Fatalf("absent is off: %v", err)
	}
	for name, tc := range map[string]struct{ from, to, want string }{
		"no token url":      {"  token_url: https://auth.example.com/oauth/v2/token\n", "", "factory.token_url is required"},
		"a plain token url": {"https://auth.example.com/oauth", "http://auth.example.com/oauth", "factory.token_url must be https"},
		"no repository":     {"  repository: Smana/cloud-native-ref\n", "", "factory.repository must be owner/name"},
		"no ceiling":        {"max_cost_usd: 2", "max_cost_usd: 0", "factory.max_cost_usd must be above 0"},
		"no webhook key":    {"  webhook: {secret_env: FACTORY_WEBHOOK_KEY}\n", "", "factory.webhook.secret_env is required"},
		"an unknown key":    {"max_cost_usd: 2", "max_cost_usd: 2\n  maxCost: 2", "field maxCost not found"},
		"a short soak":      {"max_cost_usd: 2", "max_cost_usd: 2\n  soak: 2m", "factory.soak must be at least 5m"},
		"a plain-ftp am":    {"max_cost_usd: 2", "max_cost_usd: 2\n  verify: {alertmanager_url: \"ftp://x\"}", "factory.verify.alertmanager_url must be http(s)"},
		"negative steps":    {"max_cost_usd: 2", "max_cost_usd: 2\n  verify: {max_steps: -1}", "factory.verify.max_steps must be above 0"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadString(t, strings.Replace(factoryBlock, tc.from, tc.to, 1)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestFactoryVerifyDefaults(t *testing.T) {
	c, err := loadString(t, factoryBlock)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Factory.Soak.Std(); got != 30*time.Minute {
		t.Fatalf("soak default %v, want 30m", got)
	}
	if c.Factory.Verify.MaxSteps != 6 || c.Factory.Verify.AlertmanagerURL != "" {
		t.Fatalf("verify defaults %+v: max_steps 6, verification off", c.Factory.Verify)
	}
	c, err = loadString(t, strings.Replace(factoryBlock, "max_cost_usd: 2", "max_cost_usd: 2\n  soak: 1h\n  verify: {alertmanager_url: \"http://am:9093\", max_steps: 3}", 1))
	if err != nil {
		t.Fatal(err)
	}
	if c.Factory.Soak.Std() != time.Hour || c.Factory.Verify.MaxSteps != 3 || c.Factory.Verify.AlertmanagerURL != "http://am:9093" {
		t.Fatalf("explicit values kept: %+v", c.Factory)
	}
	if c, err := loadString(t, "logging: {format: json}\n"); err != nil || c.Factory.Soak != 0 || c.Factory.Verify.MaxSteps != 0 {
		t.Fatalf("a disabled block is left alone: %v %+v", err, c.Factory)
	}
}
