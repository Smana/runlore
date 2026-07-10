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
// (the cheapest backstop) rather than re-querying alert state. A zero window
// disables the hold entirely: Hold enqueues immediately, preserving the original
// investigate-on-admission behavior.
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
// increments IncidentsDebounced.
func (d *incidentDebouncer) withMetrics(m *telemetry.Metrics) *incidentDebouncer {
	d.metrics = m
	return d
}

// Hold decides whether/when to enqueue r. With window 0 it enqueues immediately
// (today's behavior). Otherwise it returns without blocking; a background goroutine
// waits the window and enqueues only if the hold was not cancelled by a matching
// Cancel (resolved webhook) or the context. The hold is keyed by the alert
// Fingerprint — the only handle a resolved webhook carries; a fingerprint-less alert
// is held uncancellably (no pending entry, no cancel channel) and simply waits the
// window out.
func (d *incidentDebouncer) Hold(ctx context.Context, r investigate.Request, enq investigate.Enqueuer) {
	if d.window <= 0 {
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
	// Nothing is ever pending when debounce is off (window 0), so skip the lock on the
	// default-configuration resolved-webhook path.
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
