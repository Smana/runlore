package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func sampleIncident() Incident {
	return Incident{
		AlertName:   "HarborProbeFailure",
		Severity:    "critical",
		Environment: "prod",
		Namespace:   "apps",
		Labels:      map[string]string{"team": "platform", "severity": "critical"},
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name string
		tr   IncidentTrigger
		inc  Incident
		want bool
	}{
		{"disabled never matches", IncidentTrigger{Enabled: false}, sampleIncident(), false},
		{"empty match matches anything", IncidentTrigger{Enabled: true}, sampleIncident(), true},
		{"severity+env match", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Severity: []string{"critical"}, Environment: []string{"prod"}}}, sampleIncident(), true},
		{"severity mismatch", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Severity: []string{"warning"}}}, sampleIncident(), false},
		{"namespace glob", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Namespaces: []string{"app*"}}}, sampleIncident(), true},
		{"namespace glob miss", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Namespaces: []string{"payments"}}}, sampleIncident(), false},
		{"label subset match", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Labels: map[string]string{"team": "platform"}}}, sampleIncident(), true},
		{"label mismatch", IncidentTrigger{Enabled: true, Match: IncidentMatch{
			Labels: map[string]string{"team": "data"}}}, sampleIncident(), false},
		{"ignore excludes", IncidentTrigger{Enabled: true, Ignore: IncidentMatch{
			AlertNames: []string{"Watchdog", "HarborProbeFailure"}}}, sampleIncident(), false},
	}
	for _, c := range cases {
		if got := c.tr.Matches(c.inc); got != c.want {
			t.Errorf("%s: Matches=%v want %v", c.name, got, c.want)
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
