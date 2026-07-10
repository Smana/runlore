// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
)

type countingInv struct{ n int }

func (c *countingInv) Investigate(_ context.Context, _ Request) error { c.n++; return nil }

func workloadNS(ns string) providers.Workload { return providers.Workload{Namespace: ns} }

// drainWith runs process() for every key currently in q.reqs using the shared wq.
// Sharing the wq across calls lets NumRequeues accumulate, which is needed to
// drive the drop-path test.
func drainWith(t *testing.T, q *Queue, wq workqueue.TypedRateLimitingInterface[key]) {
	t.Helper()
	q.mu.Lock()
	n := len(q.reqs)
	for k := range q.reqs {
		wq.Add(k) // bypass any pending rate-limit delay so Get() returns immediately
	}
	q.mu.Unlock()
	for i := 0; i < n; i++ {
		k, shutdown := wq.Get()
		if shutdown {
			return
		}
		q.process(context.Background(), wq, k)
	}
}

func TestQueueRateLimitGate(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.starts = ratelimit.New(1, time.Hour) // budget 1 per hour
	q.maxRequeues = 3

	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	// process two distinct keys in one window; only one should reach Investigate.
	q.Enqueue(Request{Source: SourceAlert, Title: "A", Workload: workloadNS("a")})
	q.Enqueue(Request{Source: SourceAlert, Title: "B", Workload: workloadNS("b")})
	drainWith(t, q, wq)
	drainWith(t, q, wq)

	if inv.n != 1 {
		t.Fatalf("rate limit should allow exactly one investigation, got %d", inv.n)
	}
}

func TestQueueRateLimitDrop(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))

	starts := ratelimit.New(1, time.Hour)
	starts.Allow() // exhaust the single-slot budget before the test
	q.starts = starts
	q.maxRequeues = 2

	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	q.Enqueue(Request{Source: SourceAlert, Title: "drop-me", Workload: workloadNS("ns")})
	drainWith(t, q, wq) // budget denied → throttled; NumRequeues → 1
	drainWith(t, q, wq) // budget denied → throttled; NumRequeues → 2
	drainWith(t, q, wq) // NumRequeues(2) >= maxRequeues(2) → dropped; key deleted

	if inv.n != 0 {
		t.Fatalf("dropped investigation must never run; got %d Investigate call(s)", inv.n)
	}
	q.mu.Lock()
	remaining := len(q.reqs)
	q.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("drop path must remove key from q.reqs; %d key(s) still pending", remaining)
	}
}
