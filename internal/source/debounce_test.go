// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
)

// fakeClock is a manually advanced clock whose After channel is released on
// demand, so the incident debouncer's hold can be fired deterministically
// without real sleeps.
type fakeClock struct{ after chan time.Time }

func newFakeClock() *fakeClock { return &fakeClock{after: make(chan time.Time, 1)} }

func (c *fakeClock) After(time.Duration) <-chan time.Time { return c.after }

// release fires the pending timer so the debouncer proceeds to enqueue.
func (c *fakeClock) release() { c.after <- time.Now() }

func TestIncidentDebouncerZeroWindowEnqueuesImmediately(t *testing.T) {
	enq := &capEnq{}
	d := newIncidentDebouncer(0, nil)
	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued immediately with debounce=0, got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerEnqueuesWhenStillActive(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	// No resolved webhook arrives; releasing the timer must enqueue the survivor.
	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	clk.release()
	d.waitIdle()

	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued (still active at window end), got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerDropsResolvedWithinWindow(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	// Hold the alert, then a matching resolved arrives inside the window → drop.
	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	d.Cancel("f1")
	d.waitIdle()

	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (resolved within window), got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerCancelIgnoresUnrelatedFingerprint(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	d.Cancel("other") // a resolve for a different alert must not drop this hold
	clk.release()
	d.waitIdle()

	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued (unrelated resolve ignored), got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerContextCancelDrops(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	ctx, cancel := context.WithCancel(context.Background())
	d.Hold(ctx, investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	cancel()
	d.waitIdle()

	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (context cancelled during hold), got %d", len(enq.reqs))
	}
}
