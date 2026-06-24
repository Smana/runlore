package investigate

import (
	"context"
	"log/slog"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// StillFailing reports whether a workload is STILL Ready=False right now. It
// backs the debounce re-check; the Flux/ArgoCD implementation reads the
// resource's current Ready condition via GitOpsInspector.ResourceStatus.
type StillFailing func(ctx context.Context, w providers.Workload) (bool, error)

// Debouncer delays a GitOps-failure investigation until the failure has
// PERSISTED for a short window, then re-reads the resource's current Ready
// status and enqueues only if it is still failing. This filters reconcile-churn
// transients (e.g. a brief HealthCheckCanceled or an ArtifactFailed while a new
// artifact is mid-checkout) that clear on their own — the kind of misleading
// status that previously produced a confident-but-wrong root cause.
//
// A zero window disables the wait and re-check entirely: Debounce enqueues
// immediately, preserving the original fire-on-every-failure behavior.
type Debouncer struct {
	window  time.Duration
	check   StillFailing
	log     *slog.Logger
	metrics *telemetry.Metrics // optional; nil-safe counter for dropped transients

	// clock is injectable so tests can release the wait without real sleeps;
	// defaults to a time.After-backed clock.
	clock clock
}

// clock abstracts the debounce wait so tests can release it deterministically.
type clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewDebouncer builds a Debouncer. A zero window disables debouncing (immediate
// enqueue). check is consulted only when window > 0.
func NewDebouncer(window time.Duration, check StillFailing, log *slog.Logger) *Debouncer {
	return &Debouncer{window: window, check: check, log: log, clock: realClock{}}
}

// WithMetrics installs the (nil-safe) metric set and returns the Debouncer for
// chaining. A dropped transient increments InvestigationsDropped.
func (d *Debouncer) WithMetrics(m *telemetry.Metrics) *Debouncer {
	d.metrics = m
	return d
}

// Debounce decides whether to enqueue r. With window 0 it enqueues immediately.
// Otherwise it waits window, re-checks the workload's current Ready status, and
// enqueues only if it is still failing — dropping (debug-logged) any failure
// that cleared or flipped to a different transient within the window. It blocks
// for the wait; callers run it in a goroutine per failure event.
func (d *Debouncer) Debounce(ctx context.Context, r Request, q Enqueuer) {
	if d.window <= 0 {
		q.Enqueue(r)
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-d.clock.After(d.window):
	}
	if d.check != nil {
		stillFailing, err := d.check(ctx, r.Workload)
		if err != nil {
			// Re-check failed (transient API error): fall back to enqueuing rather
			// than silently dropping a possibly-real failure.
			if d.log != nil {
				d.log.Debug("gitops-failure debounce re-check errored; enqueuing anyway",
					"workload", r.Workload.Ref(), "reason", r.Reason, "err", err)
			}
			q.Enqueue(r)
			return
		}
		if !stillFailing {
			if d.log != nil {
				d.log.Debug("gitops-failure cleared within debounce window; dropping transient",
					"workload", r.Workload.Ref(), "reason", r.Reason, "window", d.window)
			}
			if d.metrics != nil {
				d.metrics.GitOpsFailuresDebounced.Add(ctx, 1)
			}
			return
		}
	}
	q.Enqueue(r)
}
