package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// sample incident fields, mirroring a critical/prod alert in namespace apps.
const (
	sampleAlertName   = "HarborProbeFailure"
	sampleSeverity    = "critical"
	sampleEnvironment = "prod"
	sampleNamespace   = "apps"
)

func sampleLabels() map[string]string {
	return map[string]string{"team": "platform", "severity": "critical"}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name string
		tr   IncidentTrigger
		want bool
	}{
		{"disabled never matches", IncidentTrigger{Enabled: false}, false},
		{"empty match matches anything", IncidentTrigger{Enabled: true}, true},
		{"severity+env match", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Severity: []string{"critical"}, Environment: []string{"prod"}}}, true},
		{"severity mismatch", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Severity: []string{"warning"}}}, false},
		{"namespace glob", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Namespaces: []string{"app*"}}}, true},
		{"namespace glob miss", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Namespaces: []string{"payments"}}}, false},
		{"label subset match", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Labels: map[string]string{"team": "platform"}}}, true},
		{"label mismatch", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Labels: map[string]string{"team": "data"}}}, false},
		{"ignore excludes", IncidentTrigger{Enabled: true, Ignore: IncidentMatch{
			AlertNames: []string{"Watchdog", "HarborProbeFailure"}}}, false},
	}
	for _, c := range cases {
		got := c.tr.MatchFields(sampleAlertName, sampleSeverity, sampleEnvironment, sampleNamespace, sampleLabels())
		if got != c.want {
			t.Errorf("%s: MatchFields=%v want %v", c.name, got, c.want)
		}
	}
}

func TestDurationUnmarshal(t *testing.T) {
	var d Duration
	if err := d.UnmarshalYAML(yamlScalar("30m")); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if d.Std() != 30*time.Minute {
		t.Fatalf("got %v want 30m", d.Std())
	}
}

func yamlScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: s}
}

func TestInstantRecallTrustConfig(t *testing.T) {
	const y = `
catalog:
  instant_recall:
    enabled: true
    min_score: 1.5
    margin_gap: 1.0
    solo_floor: 4.0
    require_workload_match: false
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ir := c.Catalog.InstantRecall
	if !ir.Enabled || ir.MinScore != 1.5 || ir.MarginGap != 1.0 || ir.SoloFloor != 4.0 || ir.RequireWorkloadMatch {
		t.Fatalf("instant_recall not parsed: %+v", ir)
	}
}

func TestCurateStaleAfterParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  stale_after: 720h\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := c.Curate.StaleAfter.Std(); got != 720*time.Hour {
		t.Fatalf("curate.stale_after: want 720h, got %v", got)
	}
	// Absent ⇒ zero ⇒ the lifecycle sweep is disabled (runCurate honours 0).
	var z Config
	if err := yaml.Unmarshal([]byte("forge:\n  kb_repo: o/r\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Curate.StaleAfter.Std() != 0 {
		t.Fatalf("absent stale_after must be 0, got %v", z.Curate.StaleAfter.Std())
	}
}

// TestValidateModelDoesNotRequireWebhookToken guards the R9(c) scoping decision:
// the alert-webhook auth requirement lives on the serve path, NOT in Validate.
// Validate is shared by every subcommand, so a model-configured config with no
// webhook token must still validate clean — otherwise `lore investigate` (which
// requires a model and has no webhook) would break.
func TestValidateModelDoesNotRequireWebhookToken(t *testing.T) {
	c := &Config{Model: Model{Provider: "anthropic"}} // model set, no webhook, actions off
	if err := c.Validate(); err != nil {
		t.Fatalf("model-only config must validate clean (webhook auth is serve-scoped): %v", err)
	}
}

func TestCurateRecurrenceThresholdParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  recurrence_threshold: 5\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Curate.RecurrenceThreshold != 5 {
		t.Fatalf("recurrence_threshold: want 5, got %d", c.Curate.RecurrenceThreshold)
	}
	// Absent ⇒ zero ⇒ the pass applies its own default (3).
	var z Config
	if err := yaml.Unmarshal([]byte("curate:\n  stale_after: 240h\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Curate.RecurrenceThreshold != 0 {
		t.Fatalf("absent recurrence_threshold must be 0, got %d", z.Curate.RecurrenceThreshold)
	}
}
