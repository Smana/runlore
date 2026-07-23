// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countPass counts Agent runs (each sweep runs every pass once).
type countPass struct{ n atomic.Int32 }

func (c *countPass) Run(context.Context) error { c.n.Add(1); return nil }

func TestSweeperRunsOnIntervalAndStopsOnCancel(t *testing.T) {
	p := &countPass{}
	s := Sweeper{Agent: Agent{Passes: []Pass{p}, Log: discardLog()}, Interval: 5 * time.Millisecond, Log: discardLog()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for p.n.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("sweeper did not sweep twice in time, got %d", p.n.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done // Run must return promptly on cancel
}

func TestSweeperNeverSweepsBeforeFirstInterval(t *testing.T) {
	// Leadership flaps re-enter startWork; an immediate first sweep would stampede
	// the forge listings on every flap. The first sweep waits one full interval.
	p := &countPass{}
	s := Sweeper{Agent: Agent{Passes: []Pass{p}, Log: discardLog()}, Interval: time.Hour, Log: discardLog()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if got := p.n.Load(); got != 0 {
		t.Fatalf("sweep before the first interval: got %d runs, want 0", got)
	}
}
