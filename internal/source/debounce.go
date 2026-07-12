// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/telemetry"
)

// incidentDebouncer delays a firing alert's investigation until the alert has
// PERSISTED for a short window, then enqueues it only if it is still active —
// i.e. no matching Alertmanager `resolved` webhook cancelled it within the
// window. It filters self-resolving alerts (e.g. a KubeDaemonSetRolloutStuck
// during a Karpenter node-churn cycle) that would otherwise burn a full
// investigation on noise and are frequently already resolved by the time a
// finding is delivered.
//
// It is event-driven, mirroring the GitOps-failure debouncer's intent but using
// the resolved webhook RunLore already receives as the "still active?" signal
// (the cheapest backstop) rather than re-querying alert state.
//
// TWO things are never held:
//
//   - A CRITICAL alert. "A debounce must never delay the first look at a critical
//     page" is a design invariant (D6 in the coalescing spec), and the coalescer
//     already honours it by flushing criticals with no batching wait. The hold
//     would otherwise reintroduce exactly the latency the coalescer refuses to
//     add — and on a default install it would hit EVERY investigated alert, since
//     the shipped trigger matches `severity: [critical]` exclusively. Criticals
//     instead get their noise/cost filtering from cancel_queued_on_resolve (on by
//     default), which drops a self-resolving critical from the QUEUE at zero added
//     latency. Both sides test the same predicate: investigate.Request.IsCritical.
//   - Anything, when the window is 0. An explicit `debounce: 0s` is the full escape
//     hatch: Hold enqueues immediately, restoring investigate-on-admission.
//
// In the pipeline the hold sits AFTER match+dedup (so re-fires are already
// suppressed) and BEFORE the coalescer (so survivors are still storm-batched).
type incidentDebouncer struct {
	window  time.Duration
	log     *slog.Logger
	metrics *telemetry.Metrics // optional; nil-safe counter for dropped self-resolving alerts

	mu      sync.Mutex
	pending map[string]chan struct{} // key → cancel channel of an in-flight hold

	wg sync.WaitGroup // tracks in-flight holds so tests can wait on the enqueue decision

	// clock is injectable so tests can release the hold without real sleeps;
	// defaults to a time.After-backed clock.
	clock clock
}

// clock abstracts the debounce wait so tests can release it deterministically.
type clock interface {
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// newIncidentDebouncer builds an incidentDebouncer. A zero (or negative) window
// disables debouncing (immediate enqueue).
func newIncidentDebouncer(window time.Duration, log *slog.Logger) *incidentDebouncer {
	return &incidentDebouncer{
		window:  window,
		log:     log,
		pending: make(map[string]chan struct{}),
		clock:   realClock{},
	}
}

// withMetrics installs the (nil-safe) metric set. A dropped self-resolving alert
// increments IncidentsDebounced; a held alert lost to shutdown increments
// IncidentsDroppedOnShutdown.
func (d *incidentDebouncer) withMetrics(m *telemetry.Metrics) *incidentDebouncer {
	d.metrics = m
	return d
}

// Hold decides whether/when to enqueue r.
//
// It enqueues IMMEDIATELY, with no hold, when the window is 0 (the explicit escape
// hatch) or when r is CRITICAL — a critical page must never wait on a debounce, the
// same invariant the coalescer enforces on its batching wait (see the type comment).
//
// Otherwise it returns without blocking; a background goroutine waits the window and
// enqueues only if the hold was not cancelled by a matching Cancel (resolved webhook)
// or the context. The hold is keyed by the alert Fingerprint — the only handle a
// resolved webhook carries; a fingerprint-less alert is held uncancellably (no pending
// entry, no cancel channel) and simply waits the window out.
func (d *incidentDebouncer) Hold(ctx context.Context, r investigate.Request, enq investigate.Enqueuer) {
	if d.window <= 0 || r.IsCritical() {
		enq.Enqueue(r)
		return
	}
	fp := r.Fingerprint
	var cancel chan struct{}
	if fp != "" {
		cancel = make(chan struct{})
		d.register(fp, cancel)
	}
	d.wg.Add(1)
	go d.wait(ctx, r, enq, fp, cancel)
}

// wait blocks for the window then enqueues, unless cancelled (resolved) or the
// context is done first.
func (d *incidentDebouncer) wait(ctx context.Context, r investigate.Request, enq investigate.Enqueuer, fp string, cancel chan struct{}) {
	defer d.wg.Done()
	// A nil cancel channel (fingerprint-less hold) blocks forever in the select, so such
	// a hold can only end via the window firing or the context — never a Cancel.
	select {
	case <-ctx.Done():
		d.unregister(fp, cancel)
		// LOSS, not a filter — say so loudly. Alertmanager already got its 200 for this
		// alert (Hold returns before the window elapses, so the webhook handler answered
		// long ago), and a held incident that never reaches the queue is simply gone: no
		// investigation, no ledger entry, and no retry until Alertmanager's own
		// repeat_interval comes round (commonly hours). The hold window can also exceed
		// the drain grace period, so draining cannot rescue it. A silent drop here would
		// make a routine `helm upgrade` look like a clean restart while a page was
		// quietly discarded — hence Warn, naming the alert.
		if d.log != nil {
			d.log.Warn("held incident DROPPED: shutting down before its debounce window elapsed",
				"alert", r.Title, "fingerprint", r.Fingerprint, "severity", r.Severity, "window", d.window,
				"impact", "the alert was accepted (200 to Alertmanager) but never investigated; it will not be retried until Alertmanager's repeat_interval re-fires it",
				"remedy", "shorten triggers.incidents.debounce, or set it to 0s to investigate on admission")
		}
		if d.metrics != nil {
			// ctx is already cancelled; WithoutCancel keeps the record from being dropped
			// by an exporter that honours it.
			d.metrics.IncidentsDroppedOnShutdown.Add(context.WithoutCancel(ctx), 1)
		}
		return
	case <-cancel:
		// A matching resolved webhook arrived within the window: drop the incident.
		// unregister was already done by Cancel, which closed this channel.
		if d.log != nil {
			d.log.Debug("alert resolved within debounce window; dropping self-resolving incident",
				"alert", r.Title, "fingerprint", r.Fingerprint, "window", d.window)
		}
		if d.metrics != nil {
			d.metrics.IncidentsDebounced.Add(ctx, 1)
		}
		return
	case <-d.clock.After(d.window):
		d.unregister(fp, cancel)
		enq.Enqueue(r)
	}
}

// Cancel drops a pending hold whose alert fingerprint matches (called from the
// resolved-webhook path). It is a no-op for an empty fingerprint or an unknown
// key. Safe for concurrent use.
func (d *incidentDebouncer) Cancel(fingerprint string) {
	// Nothing is ever pending when the hold is off (explicit `debounce: 0s`), so skip
	// the lock entirely on that path. Note this is NOT the default configuration — the
	// default window is 60s — and a resolve for a critical also finds nothing pending
	// (criticals are never held); those are dropped one stage later, from the queue, by
	// cancel_queued_on_resolve.
	if d == nil || d.window <= 0 || fingerprint == "" {
		return
	}
	d.mu.Lock()
	ch, ok := d.pending[fingerprint]
	if ok {
		delete(d.pending, fingerprint)
	}
	d.mu.Unlock()
	if ok {
		close(ch) // releases the waiting goroutine, which drops the incident
	}
}

// register records an in-flight hold. A pre-existing entry for the same key
// (should not happen once dedup runs first, but be defensive) is dropped from the
// map without closing it: its goroutine will simply wait its own window out.
func (d *incidentDebouncer) register(key string, ch chan struct{}) {
	d.mu.Lock()
	d.pending[key] = ch
	d.mu.Unlock()
}

// unregister removes the hold only if the stored channel is still this one, so a
// replaced/cancelled entry is never clobbered.
func (d *incidentDebouncer) unregister(key string, ch chan struct{}) {
	d.mu.Lock()
	if cur, ok := d.pending[key]; ok && cur == ch {
		delete(d.pending, key)
	}
	d.mu.Unlock()
}

// waitIdle blocks until all in-flight holds have finished. Used by tests (after
// releasing the clock or cancelling) to observe the terminal enqueue decision.
func (d *incidentDebouncer) waitIdle() { d.wg.Wait() }
