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
	// max_events absent ⇒ nil (the wiring applies the default).
	if c.Outcome.MaxEvents != nil {
		t.Fatalf("absent max_events must be nil, got %v", *c.Outcome.MaxEvents)
	}
}

// TestOutcomeMaxEventsThreeState pins the tri-state max_events knob: an explicit 0
// (compaction disabled) must be distinguishable from an absent key (nil ⇒ default).
func TestOutcomeMaxEventsThreeState(t *testing.T) {
	var zero Config
	if err := yaml.Unmarshal([]byte("outcome:\n  max_events: 0\n"), &zero); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if zero.Outcome.MaxEvents == nil || *zero.Outcome.MaxEvents != 0 {
		t.Fatalf("explicit max_events: 0 must parse to &0, got %v", zero.Outcome.MaxEvents)
	}
	var n Config
	if err := yaml.Unmarshal([]byte("outcome:\n  max_events: 1000\n"), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.Outcome.MaxEvents == nil || *n.Outcome.MaxEvents != 1000 {
		t.Fatalf("max_events: 1000 must parse to &1000, got %v", n.Outcome.MaxEvents)
	}
}
