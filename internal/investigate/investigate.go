// SPDX-License-Identifier: Apache-2.0

// Package investigate routes triggers (incident alerts, GitOps failures) into a
// single async investigation queue and runs them. The investigation itself is
// pluggable via Investigator: LoopInvestigator (loop.go) is the real
// implementation — a ReAct loop that drives a ModelProvider with tools, feeds
// tool results back, and delivers a providers.Investigation when the model calls
// submit_findings — while LogInvestigator remains the read-only fallback used
// when no model is configured.
package investigate

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/telemetry"
)

// Source identifies what triggered an investigation.
type Source string

const (
	// SourceAlert means the investigation was triggered by an incident alert.
	SourceAlert Source = "alert"
	// SourceGitOpsFailure means the investigation was triggered by a GitOps failure.
	SourceGitOpsFailure Source = "gitops-failure"
	// SourcePagerDuty means the investigation was triggered by a PagerDuty incident.
	SourcePagerDuty Source = "pagerduty"
)

// Request is a normalized investigation trigger.
type Request struct {
	Source       Source
	Title        string
	Workload     providers.Workload // optional; zero for alerts without a workload
	Reason       string
	Severity     string // alert severity (alert-like sources); shapes prompt + notification
	Environment  string // deployment environment (prod/staging/…)
	Message      string
	Labels       map[string]string
	Annotations  map[string]string // alert annotations (runbook_url, dashboards, …); surfaced in the seed prompt
	GroupKey     string            // Alertmanager group identity (shared by all alerts in one webhook POST)
	At           time.Time
	Fingerprint  string   // Alertmanager fingerprint (stable firing↔resolved); for outcome attribution
	Fingerprints []string // coalesced batch fingerprints; one open is recorded per entry so every constituent alert's resolve matches
	TriggerKey   string   // deterministic incident identity (alert fingerprint, or failing resource+condition) set at trigger time; threaded to Investigation.TriggerKey for stable dedup across reworded re-investigations (#137)
}

// FromFailureEvent builds a Request from a GitOps failure.
func FromFailureEvent(fe providers.FailureEvent) Request {
	r := Request{
		Source:   SourceGitOpsFailure,
		Title:    fe.Workload.Kind + "/" + fe.Workload.Name + " " + fe.Reason,
		Workload: fe.Workload,
		Reason:   fe.Reason,
		Message:  fe.Message,
		At:       fe.When,
	}
	// TriggerKey is the failing resource ref + its condition reason — both
	// deterministic K8s fields, not LLM prose. A persistently-failing resource (e.g.
	// an ArgoCD Application that retries every ~30m) re-fires with the same ref+reason,
	// so its re-investigations dedupe to one curated PR however the model rewords the
	// cause (#137). Guard the degenerate empty-ref case → fall back to prose dedup.
	if ref := fe.Workload.Ref(); ref != "" {
		r.TriggerKey = ref + ":" + fe.Reason
	}
	// A GitOps failure carries no external alert fingerprint. Without one the outcome
	// ledger's open-emission guard skips it entirely — no open event, no Occurrences
	// recurrence facts, and the Phase-2 Recurrence pass (which reads Episodes) never
	// sees pure-GitOps patterns. Derive a stable, deterministic fingerprint from the
	// incident identity (the trigger key, else the title) so these incidents are
	// captured like any other; the gitops: prefix keeps it from ever colliding with an
	// Alertmanager fingerprint (and marks the open non-resolvable — no resolve webhook
	// can ever match it).
	key := r.TriggerKey
	if key == "" {
		key = r.Title
	}
	r.Fingerprint = outcome.DeriveFingerprint(outcome.GitOpsFingerprintPrefix, key)
	r.Fingerprints = []string{r.Fingerprint}
	return r
}

// Investigator runs an investigation for a Request.
type Investigator interface {
	Investigate(ctx context.Context, r Request) error
}

// LogInvestigator is the read-only fallback used when no model is configured:
// it logs the request it would investigate and does nothing else. The real
// investigation is LoopInvestigator.
type LogInvestigator struct {
	Log *slog.Logger
}

// Investigate logs the request.
func (l LogInvestigator) Investigate(_ context.Context, r Request) error {
	l.Log.Info("investigate",
		"source", string(r.Source), "title", r.Title,
		"workload", r.Workload.Namespace+"/"+r.Workload.Name, "reason", r.Reason)
	return nil
}

// Enqueuer accepts investigation requests.
type Enqueuer interface {
	Enqueue(r Request)
}

// key is the comparable workqueue item; duplicate triggers with the same key
// coalesce. The full Request payload is held in Queue.reqs.
type key struct {
	Source    Source
	Namespace string
	Name      string
	Title     string
}

func keyOf(r Request) key {
	return key{Source: r.Source, Namespace: r.Workload.Namespace, Name: r.Workload.Name, Title: r.Title}
}

// pending holds a coalesced request plus the sequence number of the latest
// Enqueue that produced it (used for compare-and-delete after processing).
type pending struct {
	req Request
	seq uint64
}

// Queue is a rate-limiting investigation queue: duplicate triggers coalesce, and
// failed investigations are retried with exponential backoff. A fresh workqueue
// is built per Run (leadership term), so the queue recovers after a lost-then-
// re-acquired leadership instead of staying permanently shut down.
type Queue struct {
	mu   sync.Mutex
	wq   workqueue.TypedRateLimitingInterface[key] // current term's queue; nil between terms
	reqs map[key]pending
	seq  uint64
	inv  Investigator
	log  *slog.Logger

	// Rate limiting: nil starts = unlimited; 0 maxRequeues = drop immediately on throttle.
	starts      *ratelimit.Window  // sliding-window start budget; nil = unlimited
	maxRequeues int                // drop a key after this many backoff requeues
	metrics     *telemetry.Metrics // nil-safe; counters for started/throttled/dropped
	throttled   *ratelimit.Window  // 1-per-window guard: log.Warn at most once per window
}

// NewQueue builds an investigation queue. The workqueue itself is created per Run.
func NewQueue(inv Investigator, log *slog.Logger) *Queue {
	return &Queue{reqs: map[key]pending{}, inv: inv, log: log}
}

// Enqueue submits a request. Re-enqueuing the same key before it is processed
// coalesces (latest payload wins). Requests that arrive between terms are held
// and replayed when the next Run starts.
func (q *Queue) Enqueue(r Request) {
	k := keyOf(r)
	q.mu.Lock()
	q.seq++
	q.reqs[k] = pending{req: r, seq: q.seq}
	wq := q.wq
	q.mu.Unlock()
	if wq != nil {
		wq.Add(k)
	}
}

// Run builds a fresh workqueue for this leadership term, replays any pending
// requests, and consumes until ctx is done — then shuts that queue down. Safe to
// call again on re-acquired leadership.
func (q *Queue) Run(ctx context.Context) {
	wq := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]())
	q.mu.Lock()
	q.wq = wq
	for k := range q.reqs {
		wq.Add(k) // replay requests that arrived between terms / were mid-flight
	}
	q.mu.Unlock()
	go func() {
		<-ctx.Done()
		wq.ShutDown()
	}()
	for {
		k, shutdown := wq.Get()
		if shutdown {
			q.mu.Lock()
			if q.wq == wq {
				q.wq = nil
			}
			q.mu.Unlock()
			return
		}
		q.process(ctx, wq, k)
	}
}

// Drain stops the current term's queue from starting new work and waits for an
// in-flight investigation to finish, up to ctx's deadline. It is the graceful
// counterpart to a workCtx cancel (which aborts immediately — used on LOST
// leadership): on SIGTERM the leader keeps its work context alive and calls Drain so
// the in-flight investigation can COMPLETE (record its outcome + deliver) before the
// process exits, instead of being killed mid-flight. A no-op when not running (no
// leadership / between terms). If the deadline fires first it returns, and the
// caller's subsequent workCtx cancel aborts the straggler.
func (q *Queue) Drain(ctx context.Context) {
	q.mu.Lock()
	wq := q.wq
	q.mu.Unlock()
	if wq == nil {
		return
	}
	done := make(chan struct{})
	// ShutDownWithDrain stops new Get()s but waits for the in-flight item (Get-but-
	// not-Done) to finish — unlike ShutDown(), which drops it. The worker's per-item
	// ctx is workCtx (still alive during the drain), so the investigation completes.
	go func() { wq.ShutDownWithDrain(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (q *Queue) process(ctx context.Context, wq workqueue.TypedRateLimitingInterface[key], k key) {
	defer wq.Done(k)
	q.mu.Lock()
	p, ok := q.reqs[k]
	q.mu.Unlock()
	if !ok {
		wq.Forget(k)
		return
	}
	// Rate-limit gate: if the sliding-window budget is exhausted, either back off
	// (requeue with exponential delay) or drop after max_requeues retries.
	if q.starts != nil && !q.starts.Allow() {
		if wq.NumRequeues(k) >= q.maxRequeues {
			if q.metrics != nil {
				q.metrics.InvestigationsDropped.Add(ctx, 1)
			}
			q.log.Warn("investigation budget exhausted; dropping (Alertmanager will re-fire)", "title", p.req.Title)
			wq.Forget(k)
			q.mu.Lock()
			delete(q.reqs, k)
			q.mu.Unlock()
			q.maybeNotifyThrottle()
			return
		}
		if q.metrics != nil {
			q.metrics.InvestigationsThrottled.Add(ctx, 1)
		}
		wq.AddRateLimited(k)
		q.maybeNotifyThrottle()
		return
	}
	if q.metrics != nil {
		q.metrics.InvestigationsStarted.Add(ctx, 1)
	}
	if err := q.inv.Investigate(ctx, p.req); err != nil {
		// A permanent error (e.g. a 4xx model bad-request) won't succeed on retry, so
		// drop it instead of requeuing with backoff — otherwise a doomed request is
		// retried forever, burning model calls and amplifying a rate-limit storm.
		// Alertmanager re-fires the alert if it persists, re-enqueuing a fresh attempt.
		if providers.IsPermanent(err) {
			if q.metrics != nil {
				q.metrics.InvestigationsDropped.Add(ctx, 1)
			}
			q.log.Error("investigation failed permanently; dropping", "title", p.req.Title, "err", err)
			wq.Forget(k)
			q.mu.Lock()
			delete(q.reqs, k)
			q.mu.Unlock()
			return
		}
		q.log.Error("investigation failed; retrying", "title", p.req.Title, "err", err)
		wq.AddRateLimited(k)
		return
	}
	wq.Forget(k)
	// Compare-and-delete: only drop the payload if it hasn't been superseded by a
	// re-fired trigger while we were investigating (else the fresh trigger is lost).
	q.mu.Lock()
	if cur, ok := q.reqs[k]; ok && cur.seq == p.seq {
		delete(q.reqs, k)
	}
	q.mu.Unlock()
}

// maybeNotifyThrottle emits a warning log at most once per throttle window.
func (q *Queue) maybeNotifyThrottle() {
	if q.throttled != nil && q.throttled.Allow() {
		q.log.Warn("investigation rate limit engaged; throttling new investigations this window")
	}
}

// ConfigureRateLimit installs a sliding-window start budget and wires metric
// counters into the Queue. Call before Run; nil starts = unlimited. The
// once-per-window throttle-log guard is derived from starts internally.
func (q *Queue) ConfigureRateLimit(starts *ratelimit.Window, maxRequeues int, m *telemetry.Metrics) {
	q.starts = starts
	q.maxRequeues = maxRequeues
	q.metrics = m
	if starts != nil {
		q.throttled = ratelimit.New(1, starts.Window())
	}
}
