// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// fakeClock is a manually advanced clock with a controllable After channel, so
// the debouncer's wait can be released deterministically without real sleeps.
type fakeClock struct {
	after chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{after: make(chan time.Time, 1)} }

func (c *fakeClock) After(time.Duration) <-chan time.Time { return c.after }

// release fires the pending timer so the debouncer proceeds to the re-check.
func (c *fakeClock) release() { c.after <- time.Now() }

func wl(name string) providers.Workload {
	return providers.Workload{Kind: "Kustomization", Name: name, Namespace: "flux-system"}
}

func TestDebouncerEnqueuesWhenStillFailing(t *testing.T) {
	clk := newFakeClock()
	enq := &collectEnqueuer{}
	// Predicate: the workload is still Ready=False at re-check time.
	stillFailing := func(context.Context, providers.Workload) (bool, error) { return true, nil }

	d := NewDebouncer(60*time.Second, stillFailing, discardLog)
	d.clock = clk

	done := make(chan struct{})
	go func() {
		d.Debounce(context.Background(), FromFailureEvent(providers.FailureEvent{Workload: wl("apps"), Reason: "BuildFailed"}), enq)
		close(done)
	}()
	clk.release()
	<-done

	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued (still failing), got %d", len(enq.reqs))
	}
}

func TestDebouncerDropsRecoveredTransient(t *testing.T) {
	clk := newFakeClock()
	enq := &collectEnqueuer{}
	// Predicate: the workload recovered (Ready=True) by re-check time → drop.
	recovered := func(context.Context, providers.Workload) (bool, error) { return false, nil }

	d := NewDebouncer(60*time.Second, recovered, discardLog)
	d.clock = clk

	done := make(chan struct{})
	go func() {
		d.Debounce(context.Background(), FromFailureEvent(providers.FailureEvent{Workload: wl("apps"), Reason: "ArtifactFailed"}), enq)
		close(done)
	}()
	clk.release()
	<-done

	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (transient recovered within window), got %d", len(enq.reqs))
	}
}

func TestDebouncerZeroWindowEnqueuesImmediately(t *testing.T) {
	enq := &collectEnqueuer{}
	// A predicate that would say "recovered" — it must NOT be consulted when the
	// window is 0 (today's immediate behavior is preserved).
	neverCalled := func(context.Context, providers.Workload) (bool, error) {
		t.Fatal("predicate must not be called with debounce=0")
		return false, nil
	}
	d := NewDebouncer(0, neverCalled, discardLog)

	d.Debounce(context.Background(), FromFailureEvent(providers.FailureEvent{Workload: wl("apps"), Reason: "BuildFailed"}), enq)

	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued immediately with debounce=0, got %d", len(enq.reqs))
	}
}
