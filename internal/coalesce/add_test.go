// SPDX-License-Identifier: Apache-2.0

package coalesce

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

type sink struct{ batches [][]investigate.Request }

func (s *sink) out(b []investigate.Request) { s.batches = append(s.batches, b) }

func newAt(cfg Config, s *sink, now *time.Time) *Coalescer {
	c := New(cfg, s.out)
	c.now = func() time.Time { return *now }
	return c
}

func TestAddBuffersUntilMaxBatch(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 2}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK")) // buffered
	if len(s.batches) != 0 {
		t.Fatal("first alert must buffer, not flush")
	}
	c.Add(inc("X", "ns", "warning", "GK")) // hits MaxBatch=2 → flush
	if len(s.batches) != 1 || len(s.batches[0]) != 2 {
		t.Fatalf("MaxBatch should flush 2, got %v", s.batches)
	}
}

func TestAddCooldownSuppresses(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxBatch: 1, Cooldown: 10 * time.Minute}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK")) // MaxBatch=1 → immediate flush, seeds recent[GK]
	now = now.Add(time.Minute)
	c.Add(inc("X", "ns", "warning", "GK")) // within cooldown → suppressed
	if len(s.batches) != 1 {
		t.Fatalf("second alert should be suppressed, batches=%d", len(s.batches))
	}
}

func TestAddCriticalFastPath(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "critical", "GK")) // critical bypasses debounce → immediate flush
	if len(s.batches) != 1 {
		t.Fatalf("critical should flush immediately, batches=%d", len(s.batches))
	}
}

// A storm of critical alerts for one key flushes the first immediately (no debounce
// wait) and suppresses the rest within the cooldown — one investigation, not N.
func TestAddCriticalStormSuppressedAfterFirst(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100, Cooldown: 10 * time.Minute}, s, &now)
	for i := 0; i < 5; i++ {
		c.Add(inc("X", "ns", "critical", "GK"))
	}
	if len(s.batches) != 1 || len(s.batches[0]) != 1 {
		t.Fatalf("critical storm should flush once (the first), got batches=%v", s.batches)
	}
	if len(c.pending) != 0 {
		t.Fatalf("storm alerts after the first should be suppressed, not buffered (pending=%d)", len(c.pending))
	}
}

// During cooldown, a critical with an alertname not yet seen for the key is a
// genuinely new problem and must flush — it must not be swallowed as storm
// noise. Same-key correlation (CorrelationLabels) is what makes two distinct
// alertnames share a key.
func TestAddNewCriticalDuringCooldownFlushes(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 100, Cooldown: 10 * time.Minute,
		CorrelationLabels: []string{"app"}}, s, &now)
	first := investigate.Request{Title: "CrashLoop", Workload: providers.Workload{Namespace: "ns"}, Severity: "critical",
		Labels: map[string]string{"app": "web"}}
	second := investigate.Request{Title: "PodNotReady", Workload: providers.Workload{Namespace: "ns"}, Severity: "critical",
		Labels: map[string]string{"app": "web"}} // same key (app=web), different alertname

	c.Add(first) // flush #1, seeds recent + seen{CrashLoop}
	now = now.Add(time.Minute)
	c.Add(second) // within cooldown but a NEW alertname → flush #2
	if len(s.batches) != 2 {
		t.Fatalf("a new critical alertname during cooldown must flush, batches=%d", len(s.batches))
	}
	// A repeat of the same alertname during cooldown is still suppressed.
	now = now.Add(time.Minute)
	c.Add(second) // PodNotReady again → suppressed
	if len(s.batches) != 2 {
		t.Fatalf("repeat critical alertname during cooldown must be suppressed, batches=%d", len(s.batches))
	}
}

// A warning repeat during cooldown stays suppressed — only genuinely new
// criticals get the cooldown bypass.
func TestAddWarningDuringCooldownSuppressed(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Hour, MaxBatch: 1, Cooldown: 10 * time.Minute,
		CorrelationLabels: []string{"app"}}, s, &now)
	first := investigate.Request{Title: "A", Workload: providers.Workload{Namespace: "ns"}, Severity: "warning",
		Labels: map[string]string{"app": "web"}}
	newWarn := investigate.Request{Title: "B", Workload: providers.Workload{Namespace: "ns"}, Severity: "warning",
		Labels: map[string]string{"app": "web"}} // same key, new alertname, but warning
	c.Add(first) // MaxBatch=1 → flush, seeds cooldown
	now = now.Add(time.Minute)
	c.Add(newWarn) // new alertname but only warning → still suppressed
	if len(s.batches) != 1 {
		t.Fatalf("a new warning during cooldown must stay suppressed, batches=%d", len(s.batches))
	}
}
