// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestServerMCPNeedsCatalog(t *testing.T) {
	if _, err := loadString(t, "server: {mcp: {enabled: true}}\n"); err == nil || !strings.Contains(err.Error(), "server.mcp.enabled needs catalog.dir") {
		t.Fatalf("got %v", err)
	}
	c, err := loadString(t, "catalog: {dir: /var/lib/runlore/catalog}\nserver: {mcp: {enabled: true, token_env: KB_TOKEN}}\n")
	if err != nil || !c.Server.MCP.Enabled || c.Server.MCP.TokenEnv != "KB_TOKEN" {
		t.Fatalf("%v %+v", err, c.Server.MCP)
	}
}

const curationDeps = factoryBlock + `  verify: {alertmanager_url: "http://am:9093"}
outcome: {ledger_path: /var/lib/runlore/outcome.jsonl}
forge: {kb_repo: Smana/kb}
`

func TestFactoryCuration(t *testing.T) {
	load := func(curation, deps string) (*Config, error) {
		return loadString(t, strings.Replace(deps, "  webhook:", "  curation: "+curation+"\n  webhook:", 1))
	}
	t.Run("defaults", func(t *testing.T) {
		c, err := load("{defer: true}", curationDeps)
		if err != nil || !c.Factory.Curation.Defer || c.Factory.Curation.Window.Std() != 168*time.Hour {
			t.Fatalf("%v %+v", err, c.Factory.Curation)
		}
	})
	t.Run("absent", func(t *testing.T) {
		c, err := loadString(t, curationDeps)
		if err != nil || c.Factory.Curation.Defer {
			t.Fatalf("%v %+v", err, c.Factory.Curation)
		}
	})
	for in, want := range map[string]time.Duration{"7d": 168 * time.Hour, "7d12h": 180 * time.Hour, "24h": 24 * time.Hour} {
		t.Run("window "+in, func(t *testing.T) {
			c, err := load("{defer: true, window: "+in+"}", curationDeps)
			if err != nil || c.Factory.Curation.Window.Std() != want {
				t.Fatalf("%v %+v", err, c.Factory.Curation)
			}
		})
	}
	t.Run("window too short", func(t *testing.T) {
		if _, err := load("{defer: true, window: 12h}", curationDeps); err == nil || !strings.Contains(err.Error(), "factory.curation.window must be at least 24h") {
			t.Fatalf("got %v", err)
		}
	})
	for name, tc := range map[string]struct{ from, to, want string }{
		"no factory url":  {"  url: http://agent-factory.agent-system.svc.cluster.local:8082\n", "", "factory.curation.defer needs factory.url"},
		"no ledger":       {"outcome: {ledger_path: /var/lib/runlore/outcome.jsonl}\n", "", "factory.curation.defer needs outcome.ledger_path"},
		"no alertmanager": {`  verify: {alertmanager_url: "http://am:9093"}`, "", "factory.curation.defer needs factory.verify.alertmanager_url"},
		"no kb repo":      {"forge: {kb_repo: Smana/kb}\n", "", "factory.curation.defer needs forge.kb_repo"},
		"gitlab":          {"forge: {kb_repo: Smana/kb}", "forge: {provider: gitlab, kb_repo: g/kb, gitlab: {token_env: GL}}", "factory.curation.defer needs forge.provider github"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load("{defer: true}", strings.Replace(curationDeps, tc.from, tc.to, 1)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	t.Run("off needs nothing", func(t *testing.T) {
		if _, err := loadString(t, "factory: {curation: {defer: false}}\n"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDurationDays(t *testing.T) {
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "720h": 720 * time.Hour, "7d": 168 * time.Hour, "1d": 24 * time.Hour, "7d12h": 180 * time.Hour} {
		var d Duration
		if err := yaml.Unmarshal([]byte(in), &d); err != nil || d.Std() != want {
			t.Errorf("%s: %v %v", in, err, d.Std())
		}
	}
	for _, in := range []string{"1.5d", "d", "-1d", "7d5", "1d1d"} {
		var d Duration
		if err := yaml.Unmarshal([]byte(`"`+in+`"`), &d); err == nil {
			t.Errorf("%s accepted as %v", in, d.Std())
		}
	}
}
