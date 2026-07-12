// SPDX-License-Identifier: Apache-2.0

package source

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
)

// fakeClock is a manually advanced clock whose After channel is released on
// demand, so the incident debouncer's hold can be fired deterministically
// without real sleeps.
type fakeClock struct{ after chan time.Time }

func newFakeClock() *fakeClock { return &fakeClock{after: make(chan time.Time, 1)} }

func (c *fakeClock) After(time.Duration) <-chan time.Time { return c.after }

// release fires the pending timer so the debouncer proceeds to enqueue.
func (c *fakeClock) release() { c.after <- time.Now() }

func TestIncidentDebouncerZeroWindowEnqueuesImmediately(t *testing.T) {
	enq := &capEnq{}
	d := newIncidentDebouncer(0, nil)
	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued immediately with debounce=0, got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerEnqueuesWhenStillActive(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	// No resolved webhook arrives; releasing the timer must enqueue the survivor.
	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	clk.release()
	d.waitIdle()

	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued (still active at window end), got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerDropsResolvedWithinWindow(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	// Hold the alert, then a matching resolved arrives inside the window → drop.
	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	d.Cancel("f1")
	d.waitIdle()

	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (resolved within window), got %d", len(enq.reqs))
	}
}

func TestIncidentDebouncerCancelIgnoresUnrelatedFingerprint(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	d.Hold(context.Background(), investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	d.Cancel("other") // a resolve for a different alert must not drop this hold
	clk.release()
	d.waitIdle()

	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued (unrelated resolve ignored), got %d", len(enq.reqs))
	}
}

// TestIncidentDebouncerNeverHoldsCritical pins the design invariant the coalescer
// already enforces on its own wait: "a debounce must never delay the first look at a
// critical page" (D6, coalescing spec). The clock is NEVER released here — if a
// critical were held, nothing would ever be enqueued. Both this and
// coalesce.Coalescer.Add go through investigate.Request.IsCritical, so the two waits
// cannot drift apart.
func TestIncidentDebouncerNeverHoldsCritical(t *testing.T) {
	for _, sev := range []string{"critical", "CRITICAL", "Critical"} {
		enq := &capEnq{}
		d := newIncidentDebouncer(60*time.Second, nil)
		d.clock = newFakeClock() // never released

		d.Hold(context.Background(), investigate.Request{Title: "A", Severity: sev, Fingerprint: "f1"}, enq)

		if len(enq.reqs) != 1 {
			t.Fatalf("severity %q: critical must be enqueued immediately with no hold, got %d", sev, len(enq.reqs))
		}
	}
}

// TestIncidentDebouncerHoldsNonCritical is the counterpart: everything below critical
// still gets the hold the debounce exists for.
func TestIncidentDebouncerHoldsNonCritical(t *testing.T) {
	for _, sev := range []string{"warning", "info", ""} {
		clk := newFakeClock()
		enq := &capEnq{}
		d := newIncidentDebouncer(60*time.Second, nil)
		d.clock = clk

		d.Hold(context.Background(), investigate.Request{Title: "A", Severity: sev, Fingerprint: "f1"}, enq)
		if len(enq.reqs) != 0 {
			t.Fatalf("severity %q: must be held, got %d enqueued before the window elapsed", sev, len(enq.reqs))
		}
		clk.release()
		d.waitIdle()
		if len(enq.reqs) != 1 {
			t.Fatalf("severity %q: must enqueue at window end (still active), got %d", sev, len(enq.reqs))
		}
	}
}

func TestIncidentDebouncerContextCancelDrops(t *testing.T) {
	clk := newFakeClock()
	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, nil)
	d.clock = clk

	ctx, cancel := context.WithCancel(context.Background())
	d.Hold(ctx, investigate.Request{Title: "A", Fingerprint: "f1"}, enq)
	cancel()
	d.waitIdle()

	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (context cancelled during hold), got %d", len(enq.reqs))
	}
}

// TestIncidentDebouncerShutdownDropIsLoud pins the shutdown-loss warning. A held alert
// killed by shutdown is real LOSS, not a filter: Alertmanager already got its 200, so
// the alert is never investigated and is not retried until its repeat_interval (often
// hours). The hold window (60s default) can also exceed the drain grace period (25s),
// so draining cannot rescue it. The drop must therefore be a WARN naming the alert —
// never silent, which would make a routine `helm upgrade` look like a clean restart
// while a page was quietly discarded.
func TestIncidentDebouncerShutdownDropIsLoud(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	enq := &capEnq{}
	d := newIncidentDebouncer(60*time.Second, log)
	d.clock = newFakeClock() // never released: the hold is still in-flight at shutdown

	ctx, cancel := context.WithCancel(context.Background())
	d.Hold(ctx, investigate.Request{Title: "KubePodCrashLooping", Severity: "warning", Fingerprint: "fp-abc123"}, enq)
	cancel() // SIGTERM mid-hold
	d.waitIdle()

	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (shutdown during hold), got %d", len(enq.reqs))
	}
	out := buf.String()
	if out == "" {
		t.Fatal("shutdown drop was SILENT: a held incident lost to shutdown must be logged at WARN")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("log record: %v (raw: %q)", err, out)
	}
	if rec["level"] != "WARN" {
		t.Fatalf("shutdown drop must be WARN (it is data loss), got level %v", rec["level"])
	}
	// The alert must be identifiable from the log line alone — an operator reading it
	// after a rollout has no other record that this alert ever arrived.
	if rec["alert"] != "KubePodCrashLooping" {
		t.Fatalf("shutdown drop must name the alert, got %v", rec["alert"])
	}
	if rec["fingerprint"] != "fp-abc123" {
		t.Fatalf("shutdown drop must name the fingerprint, got %v", rec["fingerprint"])
	}
}
