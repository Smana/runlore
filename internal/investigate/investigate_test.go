package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// spyInvestigator records the requests it receives.
type spyInvestigator struct {
	mu   sync.Mutex
	got  []Request
	done chan struct{}
}

func (s *spyInvestigator) Investigate(_ context.Context, r Request) error {
	s.mu.Lock()
	s.got = append(s.got, r)
	s.mu.Unlock()
	if s.done != nil {
		s.done <- struct{}{}
	}
	return nil
}

func TestQueueDispatches(t *testing.T) {
	spy := &spyInvestigator{done: make(chan struct{}, 4)}
	q := NewQueue(spy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(Request{Source: SourceAlert, Title: "A"})
	q.Enqueue(Request{Source: SourceGitOpsFailure, Title: "B", Workload: providers.Workload{Namespace: "ns", Name: "x"}})

	for i := 0; i < 2; i++ {
		select {
		case <-spy.done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for dispatch")
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.got) != 2 {
		t.Fatalf("want 2 dispatched, got %d", len(spy.got))
	}
}

// failingInvestigator fails n times, then succeeds.
type failingInvestigator struct {
	mu        sync.Mutex
	failsLeft int
	calls     int
	done      chan struct{}
}

func (f *failingInvestigator) Investigate(context.Context, Request) error {
	f.mu.Lock()
	f.calls++
	fail := f.failsLeft > 0
	if fail {
		f.failsLeft--
	}
	f.mu.Unlock()
	if !fail && f.done != nil {
		f.done <- struct{}{}
	}
	if fail {
		return errTransient
	}
	return nil
}

var errTransient = fmt.Errorf("transient")

func TestQueueRetriesOnError(t *testing.T) {
	inv := &failingInvestigator{failsLeft: 2, done: make(chan struct{}, 1)}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(Request{Source: SourceAlert, Title: "flaky"})
	select {
	case <-inv.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out: retried request never succeeded")
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.calls < 3 {
		t.Fatalf("want >=3 calls (2 failures + success), got %d", inv.calls)
	}
}

// permanentFailingInvestigator always fails with a permanent error and records calls.
type permanentFailingInvestigator struct {
	mu     sync.Mutex
	calls  int
	called chan struct{}
}

func (f *permanentFailingInvestigator) Investigate(context.Context, Request) error {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.called != nil {
		f.called <- struct{}{}
	}
	return providers.Permanent(errors.New("bad request"))
}

// TestQueueDropsPermanent asserts a permanent failure is dropped, not requeued —
// so a doomed request isn't retried forever.
func TestQueueDropsPermanent(t *testing.T) {
	inv := &permanentFailingInvestigator{called: make(chan struct{}, 4)}
	q := NewQueue(inv, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Enqueue(Request{Source: SourceAlert, Title: "doomed"})
	select {
	case <-inv.called:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out: permanent request never investigated")
	}
	// It must NOT be requeued — no second call should arrive.
	select {
	case <-inv.called:
		t.Fatal("permanent failure was retried; want dropped")
	case <-time.After(500 * time.Millisecond):
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	if inv.calls != 1 {
		t.Fatalf("want exactly 1 call (dropped, not retried), got %d", inv.calls)
	}
}

func TestFromFailureEvent(t *testing.T) {
	fe := providers.FailureEvent{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Engine:   providers.EngineFlux, Reason: "BuildFailed", Message: "boom",
	}
	r := FromFailureEvent(fe)
	if r.Source != SourceGitOpsFailure || r.Workload != fe.Workload || r.Reason != "BuildFailed" || r.Message != "boom" {
		t.Fatalf("unexpected request: %+v", r)
	}
}

// TestFromFailureEventDerivesFingerprint pins the fix for GitOps incidents being
// invisible to the outcome ledger: a GitOps failure now carries a stable, deterministic
// synthetic fingerprint (gitops:<hash> of its trigger key) so the open-emission guard
// records it, and re-firings of the same failure derive the SAME id (recurrence).
func TestFromFailureEventDerivesFingerprint(t *testing.T) {
	fe := providers.FailureEvent{
		Workload: providers.Workload{Kind: "Application", Namespace: "argocd", Name: "airflow"},
		Reason:   "Degraded",
	}
	r := FromFailureEvent(fe)
	if r.Fingerprint == "" {
		t.Fatal("GitOps failure must derive a fingerprint (else the outcome ledger skips it)")
	}
	if !outcome.Derived(r.Fingerprint) {
		t.Fatalf("derived fingerprint %q must be recognised as synthetic/non-resolvable", r.Fingerprint)
	}
	if len(r.Fingerprints) != 1 || r.Fingerprints[0] != r.Fingerprint {
		t.Fatalf("Fingerprints must mirror the derived id: %+v", r.Fingerprints)
	}
	// Deterministic: a re-firing of the same failure derives the same id (recurrence).
	if again := FromFailureEvent(fe); again.Fingerprint != r.Fingerprint {
		t.Fatalf("re-firing must derive the same fingerprint: %q != %q", again.Fingerprint, r.Fingerprint)
	}
}

func TestFromFailureEventSetsTriggerKey(t *testing.T) {
	// The trigger key is the failing resource ref + the condition reason — both
	// deterministic K8s fields, set before the LLM runs, so re-investigations of one
	// persistent failure share it regardless of how the model rewords the cause (#137).
	fe := providers.FailureEvent{
		Workload: providers.Workload{Kind: "Application", Namespace: "argocd", Name: "airflow"},
		Reason:   "Degraded",
	}
	if r := FromFailureEvent(fe); r.TriggerKey != "argocd/airflow:Degraded" {
		t.Fatalf("Request.TriggerKey = %q, want argocd/airflow:Degraded", r.TriggerKey)
	}
}
