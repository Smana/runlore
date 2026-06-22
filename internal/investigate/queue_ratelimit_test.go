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

// drainOnce synchronously runs process() for every key currently in q.reqs,
// using a fresh workqueue so the test doesn't need a goroutine.
func drainOnce(t *testing.T, q *Queue) {
	t.Helper()
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	q.mu.Lock()
	for k := range q.reqs {
		wq.Add(k)
	}
	q.mu.Unlock()
	wq.ShutDown() // after adding; Get() drains existing items before signalling done
	for {
		k, shutdown := wq.Get()
		if shutdown {
			break
		}
		q.process(context.Background(), wq, k)
	}
}

func TestQueueRateLimitGate(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.starts = ratelimit.New(1, time.Hour) // budget 1 per hour
	q.maxRequeues = 3

	// process two distinct keys in one window; only one should reach Investigate.
	q.Enqueue(Request{Source: SourceAlert, Title: "A", Workload: workloadNS("a")})
	q.Enqueue(Request{Source: SourceAlert, Title: "B", Workload: workloadNS("b")})
	drainOnce(t, q) // helper runs process() for each queued key synchronously
	drainOnce(t, q)

	if inv.n != 1 {
		t.Fatalf("rate limit should allow exactly one investigation, got %d", inv.n)
	}
}
