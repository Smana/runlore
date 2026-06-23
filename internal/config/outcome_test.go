package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOutcomeConfigParse(t *testing.T) {
	const y = "outcome:\n  ledger_path: /var/lib/runlore/catalog/outcomes.jsonl\n"
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Outcome.LedgerPath != "/var/lib/runlore/catalog/outcomes.jsonl" {
		t.Fatalf("ledger_path: %q", c.Outcome.LedgerPath)
	}
}
