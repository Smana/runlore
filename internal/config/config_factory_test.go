// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadString(t, strings.Replace(factoryBlock, tc.from, tc.to, 1)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
