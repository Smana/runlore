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

func TestFromIncidentSetsTriggerKey(t *testing.T) {
	// For alerts the deterministic identity is the Alertmanager fingerprint.
	if r := FromIncident(config.Incident{AlertName: "A", Namespace: "ns", Fingerprint: "fp-9"}); r.TriggerKey != "fp-9" {
		t.Fatalf("Request.TriggerKey = %q, want fp-9", r.TriggerKey)
	}
	// No alert fingerprint ⇒ no trigger key (DupFingerprint falls back to the prose cause).
	if r := FromIncident(config.Incident{AlertName: "A", Namespace: "ns"}); r.TriggerKey != "" {
		t.Fatalf("Request.TriggerKey = %q, want empty when no fingerprint", r.TriggerKey)
	}
}

func TestFromIncidentCarriesFingerprint(t *testing.T) {
	r := FromIncident(config.Incident{AlertName: "A", Namespace: "ns", Fingerprint: "fp-9"})
	if r.Fingerprint != "fp-9" {
		t.Fatalf("Request.Fingerprint = %q, want fp-9", r.Fingerprint)
	}
}

func TestFromIncidentSetsFingerprints(t *testing.T) {
	r := FromIncident(config.Incident{AlertName: "A", Namespace: "ns", Fingerprint: "fp-9"})
	if len(r.Fingerprints) != 1 || r.Fingerprints[0] != "fp-9" {
		t.Fatalf("Request.Fingerprints = %v, want [fp-9]", r.Fingerprints)
	}
	// No fingerprint ⇒ nil (so coalescer can append non-empty ones without a "" entry).
	if r2 := FromIncident(config.Incident{AlertName: "A", Namespace: "ns"}); r2.Fingerprints != nil {
		t.Fatalf("Request.Fingerprints = %v, want nil when fingerprint empty", r2.Fingerprints)
	}
}

func TestWorkloadFromLabels(t *testing.T) {
	cases := []struct {
		name             string
		labels           map[string]string
		wantKind, wantNm string
	}{
		{"deployment", map[string]string{"deployment": "payment-api"}, "Deployment", "payment-api"},
		{"pod only", map[string]string{"pod": "x-abc123"}, "Pod", "x-abc123"},
		{"controller beats pod", map[string]string{"deployment": "payment-api", "pod": "payment-api-abc"}, "Deployment", "payment-api"},
		{"workload with type", map[string]string{"workload": "w", "workload_type": "Rollout"}, "Rollout", "w"},
		{"workload no type -> empty kind", map[string]string{"workload": "w"}, "", "w"},
		{"none", map[string]string{"severity": "critical"}, "", ""},
	}
	for _, c := range cases {
		k, n := workloadFromLabels(c.labels)
		if k != c.wantKind || n != c.wantNm {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, k, n, c.wantKind, c.wantNm)
		}
	}
}

func TestFromIncidentDerivesWorkload(t *testing.T) {
	inc := config.Incident{AlertName: "Crash", Namespace: "apps", Labels: map[string]string{"namespace": "apps", "deployment": "payment-api"}}
	r := FromIncident(inc)
	if r.Workload.Namespace != "apps" || r.Workload.Name != "payment-api" || r.Workload.Kind != "Deployment" {
		t.Fatalf("FromIncident workload = %+v, want apps/payment-api Deployment", r.Workload)
	}
}
