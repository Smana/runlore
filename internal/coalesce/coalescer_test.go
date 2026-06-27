package coalesce

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

func inc(name, ns, sev, gk string) investigate.Request {
	return investigate.Request{Title: name, Workload: providers.Workload{Namespace: ns}, Severity: sev, GroupKey: gk,
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

// When every configured correlation label is absent from an incident, the key
// must NOT collapse to "ns/" (which would coalesce unrelated incidents). It
// falls back to GroupKey, else ns/alertname.
func TestKeyAllEmptyCorrelationLabelsFallBack(t *testing.T) {
	c := New(Config{CorrelationLabels: []string{"app", "team"}}, nil)
	// Two unrelated incidents in the same namespace, neither carrying the
	// correlation labels, must not share a key.
	a := investigate.Request{Title: "DiskFull", Workload: providers.Workload{Namespace: "ns"}, GroupKey: "gk-a"}
	b := investigate.Request{Title: "OOMKill", Workload: providers.Workload{Namespace: "ns"}, GroupKey: "gk-b"}
	if ka, kb := c.key(a), c.key(b); ka == kb {
		t.Fatalf("all-empty correlation labels must not collapse unrelated incidents, both = %q", ka)
	}
	// Falls back to GroupKey when present.
	if got := c.key(a); got != "gk-a" {
		t.Fatalf("all-empty labels should fall back to groupKey, got %q", got)
	}
	// And to ns/alertname when GroupKey is also empty.
	noGK := investigate.Request{Title: "DiskFull", Workload: providers.Workload{Namespace: "ns"}}
	if got := c.key(noGK); got != "ns/DiskFull" {
		t.Fatalf("all-empty labels + no groupKey should fall back to ns/alertname, got %q", got)
	}
}

// Partial presence of correlation labels is a legitimate key and must still
// correlate (not fall back).
func TestKeyPartialCorrelationLabelsStillCorrelate(t *testing.T) {
	c := New(Config{CorrelationLabels: []string{"app", "team"}}, nil)
	withApp := investigate.Request{Title: "X", Workload: providers.Workload{Namespace: "ns"}, GroupKey: "gk1",
		Labels: map[string]string{"app": "web"}}
	// "team" is absent → partial. Key must use the labels, not fall back to GroupKey.
	if got := c.key(withApp); got != "ns/web/" {
		t.Fatalf("partial labels should still correlate on label values, got %q", got)
	}
}

func TestSummarize(t *testing.T) {
	s := Summarize([]investigate.Request{
		inc("X", "ns", "warning", "g"),
		inc("X", "ns", "warning", "g"),
		inc("Y", "ns", "warning", "g"),
	})
	if !strings.Contains(s, "3 correlated alerts") || !strings.Contains(s, "X×2") {
		t.Fatalf("summary: %q", s)
	}
}
