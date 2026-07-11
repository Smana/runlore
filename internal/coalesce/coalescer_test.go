// SPDX-License-Identifier: Apache-2.0

package coalesce

import (
	"fmt"
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

// wl builds a Request naming a specific workload (ns/name), for constituent tests.
func wl(title, ns, name string) investigate.Request {
	return investigate.Request{Title: title, Workload: providers.Workload{Namespace: ns, Name: name}}
}

// A coalesced batch of 3 distinct workloads surfaces all constituents OTHER than
// the representative (batch[0]), so the seed can investigate the whole blast radius.
func TestConstituentsDistinctWorkloads(t *testing.T) {
	got := Constituents([]investigate.Request{
		wl("A", "apps", "web"),      // representative — excluded
		wl("B", "apps", "worker"),   // constituent
		wl("C", "data", "postgres"), // constituent
	})
	want := []string{"apps/worker", "data/postgres"}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v (stable order), got %v", want, got)
		}
	}
}

// A singleton batch (a single alert, not coalesced) surfaces nothing — the
// representative's own Workload already fully describes it.
func TestConstituentsSingletonIsNil(t *testing.T) {
	if got := Constituents([]investigate.Request{wl("A", "apps", "web")}); got != nil {
		t.Fatalf("singleton batch must yield nil constituents, got %v", got)
	}
	if got := Constituents(nil); got != nil {
		t.Fatalf("empty batch must yield nil constituents, got %v", got)
	}
}

// Duplicate constituent workloads and the representative's own ref are de-duped:
// the same "ns/name" appearing twice, or matching batch[0], is surfaced at most once.
func TestConstituentsDeduplicates(t *testing.T) {
	got := Constituents([]investigate.Request{
		wl("A", "apps", "web"),    // representative
		wl("B", "apps", "worker"), // constituent
		wl("C", "apps", "worker"), // dup of the above → dropped
		wl("D", "apps", "web"),    // same as representative → dropped
	})
	want := []string{"apps/worker"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// An alert without a workload falls back to its title so it is still surfaced.
func TestConstituentsFallsBackToTitle(t *testing.T) {
	got := Constituents([]investigate.Request{
		wl("A", "apps", "web"),  // representative
		{Title: "NodeNotReady"}, // no workload → title
	})
	if len(got) != 1 || got[0] != "NodeNotReady" {
		t.Fatalf("want [NodeNotReady], got %v", got)
	}
}

// The constituent list is capped so a pathological storm can't blow up the seed.
func TestConstituentsCapped(t *testing.T) {
	batch := []investigate.Request{wl("rep", "apps", "rep")}
	for i := 0; i < maxConstituents+50; i++ {
		batch = append(batch, wl("A", "apps", fmt.Sprintf("w%d", i)))
	}
	if got := len(Constituents(batch)); got != maxConstituents {
		t.Fatalf("constituents must be capped at %d, got %d", maxConstituents, got)
	}
}
