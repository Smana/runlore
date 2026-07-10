// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"
)

// singleFPRequest mirrors what the Alertmanager decoder (and a coalesced batch of
// one) produces: Fingerprint set and Fingerprints holding exactly that entry.
func singleFPRequest(title, fp string) Request {
	return Request{Source: SourceAlert, Title: title, Fingerprint: fp, Fingerprints: []string{fp}}
}

// TestQueueCancelPendingByFingerprint pins the core semantics: a QUEUED (not yet
// started) single-fingerprint request is cancelled on its resolve — the payload is
// gone, the stale workqueue item no-ops in process, and a later firing of the same
// alert (same key) still investigates normally.
func TestQueueCancelPendingByFingerprint(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	r := singleFPRequest("A", "f1")
	q.Enqueue(r)
	wq.Add(keyOf(r)) // the workqueue item exists before the resolve arrives

	if !q.CancelByFingerprint("f1") {
		t.Fatal("want pending single-fingerprint request cancelled")
	}
	// The stale workqueue item must no-op: process tolerates the missing payload.
	k, _ := wq.Get()
	q.process(context.Background(), wq, k)
	if inv.n != 0 {
		t.Fatalf("cancelled investigation must never run; got %d Investigate call(s)", inv.n)
	}
	// The fingerprint index entry is gone with the payload: a second resolve is a no-op.
	if q.CancelByFingerprint("f1") {
		t.Fatal("second cancel of the same fingerprint must report false")
	}
	// A later firing of the same alert (same key) still investigates.
	q.Enqueue(r)
	drainWith(t, q, wq)
	if inv.n != 1 {
		t.Fatalf("re-fired alert after a cancel must investigate; got %d", inv.n)
	}
}

// blockingInv parks inside Investigate until released, so a test can observe the
// queue with an investigation genuinely IN FLIGHT.
type blockingInv struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingInv) Investigate(context.Context, Request) error {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	b.started <- struct{}{}
	<-b.release
	return nil
}

// TestQueueCancelInFlightRefused pins the in-flight boundary: once Investigate has
// started, a resolve must NOT cancel it — the investigation runs to completion,
// and afterwards there is nothing left to cancel (index cleaned on completion).
func TestQueueCancelInFlightRefused(t *testing.T) {
	inv := &blockingInv{started: make(chan struct{}, 1), release: make(chan struct{})}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	q.Enqueue(singleFPRequest("A", "f1"))
	wq.Add(keyOf(singleFPRequest("A", "f1")))
	done := make(chan struct{})
	go func() {
		k, _ := wq.Get()
		q.process(context.Background(), wq, k)
		close(done)
	}()

	<-inv.started // Investigate is running now
	if q.CancelByFingerprint("f1") {
		t.Fatal("an in-flight investigation must never be cancelled")
	}
	close(inv.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the in-flight investigation to finish")
	}
	inv.mu.Lock()
	calls := inv.calls
	inv.mu.Unlock()
	if calls != 1 {
		t.Fatalf("in-flight investigation must complete exactly once; got %d", calls)
	}
	// Completion cleaned both the payload and the fingerprint index.
	if q.CancelByFingerprint("f1") {
		t.Fatal("nothing must be cancellable after completion")
	}
}

// TestQueueCancelMultiFingerprintBatchRefused pins the coalesced-batch boundary:
// a batch carrying >1 fingerprints is never cancelled on one member's resolve —
// partial resolution is ambiguous (the rest may still be firing), so the batch
// investigation proceeds.
func TestQueueCancelMultiFingerprintBatchRefused(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	q.Enqueue(Request{Source: SourceAlert, Title: "batch", Fingerprint: "f1", Fingerprints: []string{"f1", "f2"}})
	if q.CancelByFingerprint("f1") {
		t.Fatal("multi-fingerprint batch must not be cancelled via its representative fingerprint")
	}
	if q.CancelByFingerprint("f2") {
		t.Fatal("multi-fingerprint batch must not be cancelled via a constituent fingerprint")
	}
	drainWith(t, q, wq)
	if inv.n != 1 {
		t.Fatalf("batch must investigate despite one member resolving; got %d", inv.n)
	}
}

// TestQueueCancelIndexCleanedAfterCompletion pins index hygiene: after a normal
// (uncancelled) completion the fingerprint index entry is removed alongside the
// payload, and a later same-key alert enqueues and investigates as before.
func TestQueueCancelIndexCleanedAfterCompletion(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	r := singleFPRequest("A", "f1")
	q.Enqueue(r)
	drainWith(t, q, wq)
	if inv.n != 1 {
		t.Fatalf("first firing must investigate; got %d", inv.n)
	}
	if q.CancelByFingerprint("f1") {
		t.Fatal("a completed investigation must not be cancellable (index must be cleaned)")
	}
	// The same alert fires again later: same key, must investigate again.
	q.Enqueue(r)
	drainWith(t, q, wq)
	if inv.n != 2 {
		t.Fatalf("same-key alert after completion must investigate again; got %d", inv.n)
	}
}

// TestQueueCancelEmptyOrUnknownFingerprint pins the no-op edges: an empty or
// unknown fingerprint cancels nothing, and a fingerprint-less request is never
// indexed (so it can never be cancelled).
func TestQueueCancelEmptyOrUnknownFingerprint(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	q.Enqueue(Request{Source: SourceAlert, Title: "no-fp"})
	if q.CancelByFingerprint("") {
		t.Fatal("empty fingerprint must never cancel")
	}
	if q.CancelByFingerprint("unknown") {
		t.Fatal("unknown fingerprint must never cancel")
	}
	drainWith(t, q, wq)
	if inv.n != 1 {
		t.Fatalf("fingerprint-less request must investigate; got %d", inv.n)
	}
}

// TestQueueCancelCoalescedPayloadReindexed pins Enqueue's index maintenance: when
// a re-fired trigger coalesces over a pending payload (same key, latest wins), the
// index follows the LATEST payload — cancelling the new fingerprint works, and the
// replaced payload's fingerprint no longer cancels anything.
func TestQueueCancelCoalescedPayloadReindexed(t *testing.T) {
	inv := &countingInv{}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	t.Cleanup(func() { wq.ShutDown() })

	// Same key (same Source/Title, no workload), different fingerprints.
	q.Enqueue(singleFPRequest("A", "f-old"))
	q.Enqueue(singleFPRequest("A", "f-new"))
	if q.CancelByFingerprint("f-old") {
		t.Fatal("replaced payload's fingerprint must no longer cancel")
	}
	if !q.CancelByFingerprint("f-new") {
		t.Fatal("latest payload's fingerprint must cancel the pending request")
	}
	wq.Add(keyOf(singleFPRequest("A", "f-new")))
	k, _ := wq.Get()
	q.process(context.Background(), wq, k)
	if inv.n != 0 {
		t.Fatalf("cancelled coalesced request must never run; got %d", inv.n)
	}
}
