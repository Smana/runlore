package coalesce

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func inc(name, ns, sev, gk string) config.Incident {
	return config.Incident{AlertName: name, Namespace: ns, Severity: sev, GroupKey: gk,
		Labels: map[string]string{"alertname": name, "namespace": ns}}
}

func TestKeyGroupKeyDefault(t *testing.T) {
	c := New(Config{}, nil)
	if got := c.key(inc("X", "ns", "warning", "GK1")); got != "GK1" {
		t.Fatalf("default key should be groupKey, got %q", got)
	}
	// fallback when groupKey is empty
	if got := c.key(inc("X", "ns", "warning", "")); got != "ns/X" {
		t.Fatalf("fallback key should be ns/alertname, got %q", got)
	}
}

func TestKeyCorrelationLabels(t *testing.T) {
	c := New(Config{CorrelationLabels: []string{"alertname"}}, nil)
	if got := c.key(inc("X", "ns", "warning", "GK1")); got != "ns/X" {
		t.Fatalf("label key, got %q", got)
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize([]config.Incident{
		inc("X", "ns", "warning", "g"),
		inc("X", "ns", "warning", "g"),
		inc("Y", "ns", "warning", "g"),
	})
	if !strings.Contains(s, "3 correlated alerts") || !strings.Contains(s, "X×2") {
		t.Fatalf("summary: %q", s)
	}
}
