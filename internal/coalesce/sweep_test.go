package coalesce

import (
	"testing"
	"time"
)

func TestSweepFlushesAfterDebounce(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: 30 * time.Second, MaxWait: 2 * time.Minute, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK"))

	now = now.Add(10 * time.Second)
	c.sweep() // still within debounce → no flush
	if len(s.batches) != 0 {
		t.Fatal("should not flush before debounce elapses")
	}
	now = now.Add(30 * time.Second)
	c.sweep() // quiet for >30s → flush
	if len(s.batches) != 1 {
		t.Fatalf("should flush after debounce, batches=%d", len(s.batches))
	}
}

func TestSweepMaxWaitCap(t *testing.T) {
	now := time.Unix(0, 0)
	s := &sink{}
	c := newAt(Config{Debounce: time.Minute, MaxWait: 90 * time.Second, MaxBatch: 100}, s, &now)
	c.Add(inc("X", "ns", "warning", "GK"))
	// keep it "active" so debounce never elapses, but MaxWait should still cap it
	for i := 0; i < 3; i++ {
		now = now.Add(40 * time.Second)
		c.Add(inc("X", "ns", "warning", "GK"))
		c.sweep()
	}
	if len(s.batches) == 0 {
		t.Fatal("MaxWait should force a flush despite continued activity")
	}
}
