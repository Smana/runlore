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
