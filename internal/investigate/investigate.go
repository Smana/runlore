// Package investigate routes triggers (incident alerts, GitOps failures) into a
// single async investigation queue. The investigation itself is pluggable via
// Investigator; LogInvestigator is the read-only placeholder until the ReAct loop
// lands.
package investigate

import (
	"context"
	"log/slog"
	"time"

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

// Queue is a buffered, single-worker investigation queue.
type Queue struct {
	ch  chan Request
	inv Investigator
	log *slog.Logger
}

// NewQueue builds a Queue with the given buffer size.
func NewQueue(inv Investigator, log *slog.Logger, buffer int) *Queue {
	return &Queue{ch: make(chan Request, buffer), inv: inv, log: log}
}

// Enqueue submits a request. If the buffer is full it logs and drops (backpressure)
// rather than blocking the caller (e.g. the webhook handler).
func (q *Queue) Enqueue(r Request) {
	select {
	case q.ch <- r:
	default:
		q.log.Warn("investigation queue full; dropping", "title", r.Title, "source", string(r.Source))
	}
}

// Run consumes the queue until ctx is done.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-q.ch:
			if err := q.inv.Investigate(ctx, r); err != nil {
				q.log.Error("investigation failed", "title", r.Title, "err", err)
			}
		}
	}
}
