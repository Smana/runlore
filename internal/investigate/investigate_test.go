package investigate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
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

func TestFromIncident(t *testing.T) {
	inc := config.Incident{AlertName: "HighLatency", Severity: "critical", Namespace: "apps", Labels: map[string]string{"team": "x"}}
	r := FromIncident(inc)
	if r.Source != SourceAlert || r.Title != "HighLatency" || r.Reason != "critical" || r.Workload.Namespace != "apps" {
		t.Fatalf("unexpected request: %+v", r)
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

func TestFromIncidentCarriesFingerprint(t *testing.T) {
	r := FromIncident(config.Incident{AlertName: "A", Namespace: "ns", Fingerprint: "fp-9"})
	if r.Fingerprint != "fp-9" {
		t.Fatalf("Request.Fingerprint = %q, want fp-9", r.Fingerprint)
	}
}
