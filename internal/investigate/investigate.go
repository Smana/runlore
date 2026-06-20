// Package investigate routes triggers (incident alerts, GitOps failures) into a
// single async investigation queue. The investigation itself is pluggable via
// Investigator; LogInvestigator is the read-only placeholder until the ReAct loop
// lands.
package investigate

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// Source identifies what triggered an investigation.
type Source string

const (
	// SourceAlert means the investigation was triggered by an incident alert.
	SourceAlert Source = "alert"
	// SourceGitOpsFailure means the investigation was triggered by a GitOps failure.
	SourceGitOpsFailure Source = "gitops-failure"
)

// Request is a normalized investigation trigger.
type Request struct {
	Source   Source
	Title    string
	Workload providers.Workload // optional; zero for alerts without a workload
	Reason   string
	Message  string
	Labels   map[string]string
	At       time.Time
}

// FromIncident builds a Request from a matched incident alert.
func FromIncident(inc config.Incident) Request {
	return Request{
		Source:   SourceAlert,
		Title:    inc.AlertName,
		Workload: providers.Workload{Namespace: inc.Namespace},
		Reason:   inc.Severity,
		Labels:   inc.Labels,
		At:       inc.StartsAt,
	}
}

// FromFailureEvent builds a Request from a GitOps failure.
func FromFailureEvent(fe providers.FailureEvent) Request {
	return Request{
		Source:   SourceGitOpsFailure,
		Title:    fe.Workload.Kind + "/" + fe.Workload.Name + " " + fe.Reason,
		Workload: fe.Workload,
		Reason:   fe.Reason,
		Message:  fe.Message,
		At:       fe.When,
	}
}

// Investigator runs an investigation for a Request.
type Investigator interface {
	Investigate(ctx context.Context, r Request) error
}

// LogInvestigator is the read-only placeholder: it logs the request it would
// investigate. Replaced by the ReAct loop in a later phase.
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

// Queue is a rate-limiting investigation queue: duplicate triggers coalesce, and
// failed investigations are retried with exponential backoff.
type Queue struct {
	wq   workqueue.TypedRateLimitingInterface[key]
	mu   sync.Mutex
	reqs map[key]Request
	inv  Investigator
	log  *slog.Logger
}

// NewQueue builds an investigation queue.
func NewQueue(inv Investigator, log *slog.Logger) *Queue {
	return &Queue{
		wq:   workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[key]()),
		reqs: map[key]Request{},
		inv:  inv,
		log:  log,
	}
}

// Enqueue submits a request. Re-enqueuing the same key before it is processed
// coalesces (latest payload wins).
func (q *Queue) Enqueue(r Request) {
	k := keyOf(r)
	q.mu.Lock()
	q.reqs[k] = r
	q.mu.Unlock()
	q.wq.Add(k)
}

// Run consumes the queue until ctx is done.
func (q *Queue) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		q.wq.ShutDown()
	}()
	for {
		k, shutdown := q.wq.Get()
		if shutdown {
			return
		}
		q.process(ctx, k)
	}
}

func (q *Queue) process(ctx context.Context, k key) {
	defer q.wq.Done(k)
	q.mu.Lock()
	r, ok := q.reqs[k]
	q.mu.Unlock()
	if !ok {
		q.wq.Forget(k)
		return
	}
	if err := q.inv.Investigate(ctx, r); err != nil {
		q.log.Error("investigation failed; retrying", "title", r.Title, "err", err)
		q.wq.AddRateLimited(k)
		return
	}
	q.wq.Forget(k)
	q.mu.Lock()
	delete(q.reqs, k)
	q.mu.Unlock()
}
