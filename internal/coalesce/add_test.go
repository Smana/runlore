package coalesce

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
)

type sink struct{ batches [][]config.Incident }

func (s *sink) out(b []config.Incident) { s.batches = append(s.batches, b) }

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
	if c.suppressed["GK"] != 4 {
		t.Fatalf("expected 4 suppressed after the first flush, got %d", c.suppressed["GK"])
	}
}
