// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/trigger"
)

// fakeWatcher is a WatcherSource that sends items from a pre-built slice then closes.
type fakeWatcher struct {
	items []investigate.Request
}

func (f fakeWatcher) Watch(_ context.Context) (<-chan investigate.Request, error) {
	ch := make(chan investigate.Request, len(f.items))
	for _, r := range f.items {
		ch <- r
	}
	close(ch)
	return ch, nil
}

// chanEnq is an Enqueuer that also signals a WaitGroup per enqueue, to let
// tests wait for goroutine drains without a fixed sleep.
type chanEnq struct {
	mu   sync.Mutex
	reqs []investigate.Request
	wg   *sync.WaitGroup
}

func newChanEnq(expected int) *chanEnq {
	wg := &sync.WaitGroup{}
	wg.Add(expected)
	return &chanEnq{wg: wg}
}

func (c *chanEnq) Enqueue(r investigate.Request) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r)
	c.mu.Unlock()
	c.wg.Done()
}

func (c *chanEnq) wait(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (c *chanEnq) Reqs() []investigate.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]investigate.Request, len(c.reqs))
	copy(out, c.reqs)
	return out
}

func workload(ns, name string) providers.Workload {
	return providers.Workload{Namespace: ns, Name: name}
}

// TestRunWatchersDedupSameWorkload sends two requests for the same workload
// through a Deduper. Only the first should reach the enqueuer.
func TestRunWatchersDedupSameWorkload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req1 := investigate.Request{Title: "fail", Workload: workload("ns1", "myapp"), Fingerprint: "fp-1"}
	req2 := investigate.Request{Title: "fail again", Workload: workload("ns1", "myapp"), Fingerprint: "fp-2"}

	built := []Built{
		{
			Desc: Descriptor{Name: "watcher1", Kind: Watcher, Admission: EnableGated},
			Impl: fakeWatcher{items: []investigate.Request{req1, req2}},
		},
	}

	// Expect exactly 1 enqueue (second is deduped by same workload key).
	enq := newChanEnq(1)
	dedup := trigger.NewDeduper(time.Minute)

	RunWatchers(ctx, built, enq, dedup, nil, nil)

	if !enq.wait(2 * time.Second) {
		t.Fatal("timed out waiting for enqueue")
	}
	// Give the goroutine a tiny bit of time to process the second item.
	time.Sleep(20 * time.Millisecond)

	reqs := enq.Reqs()
	if len(reqs) != 1 {
		t.Fatalf("want 1 enqueued (dedup), got %d: %+v", len(reqs), reqs)
	}
}

// TestRunWatchersDistinctWorkloadsPass sends two requests for different workloads.
// Both should reach the enqueuer even with dedup enabled.
func TestRunWatchersDistinctWorkloadsPass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req1 := investigate.Request{Title: "fail A", Workload: workload("ns1", "app-a"), Fingerprint: "fp-a"}
	req2 := investigate.Request{Title: "fail B", Workload: workload("ns1", "app-b"), Fingerprint: "fp-b"}

	built := []Built{
		{
			Desc: Descriptor{Name: "watcher2", Kind: Watcher, Admission: EnableGated},
			Impl: fakeWatcher{items: []investigate.Request{req1, req2}},
		},
	}

	enq := newChanEnq(2)
	dedup := trigger.NewDeduper(time.Minute)

	RunWatchers(ctx, built, enq, dedup, nil, nil)

	if !enq.wait(2 * time.Second) {
		reqs := enq.Reqs()
		t.Fatalf("timed out waiting for 2 enqueues; got %d: %+v", len(reqs), reqs)
	}

	reqs := enq.Reqs()
	if len(reqs) != 2 {
		t.Fatalf("want 2 enqueued (distinct workloads), got %d: %+v", len(reqs), reqs)
	}
}

// TestRunWatchersSkipsNonWatcherKind confirms Webhook-kind Builts are ignored.
func TestRunWatchersSkipsNonWatcherKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	built := []Built{
		{
			Desc: Descriptor{Name: "webhook-only", Kind: Webhook},
			Impl: fakeDecoder{result: oneRequestResult()}, // not a WatcherSource
		},
	}

	enq := &capEnq{}
	RunWatchers(ctx, built, enq, nil, nil, nil)

	// No watcher should run; just verify no panic and nothing enqueued.
	time.Sleep(20 * time.Millisecond)
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued for non-watcher, got %d", len(enq.reqs))
	}
}

// TestRunWatchersNilDebounceEnqueuesImmediately verifies nil debouncer path.
func TestRunWatchersNilDebounceEnqueuesImmediately(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req := investigate.Request{Title: "immediate", Workload: workload("prod", "svc"), Fingerprint: "fp-imm"}

	built := []Built{
		{
			Desc: Descriptor{Name: "watcher3", Kind: Watcher},
			Impl: fakeWatcher{items: []investigate.Request{req}},
		},
	}

	enq := newChanEnq(1)
	RunWatchers(ctx, built, enq, nil, nil, nil)

	if !enq.wait(2 * time.Second) {
		t.Fatal("timed out waiting for immediate enqueue with nil debouncer")
	}
	if len(enq.Reqs()) != 1 {
		t.Fatalf("want 1 enqueued, got %d", len(enq.Reqs()))
	}
}
