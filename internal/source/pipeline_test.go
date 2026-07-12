// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

type capEnq struct{ reqs []investigate.Request }

func (c *capEnq) Enqueue(r investigate.Request) { c.reqs = append(c.reqs, r) }

// ptr is the pointer-taking helper for the *bool / *Duration config knobs whose
// nil-vs-set distinction carries the "unset ⇒ default" semantics.
func ptr[T any](v T) *T { return &v }

func matchAllCfg() *config.Config {
	c := &config.Config{}
	// empty Match ⇒ matches anything; enablement is now the source's job (sources.*).
	c.Triggers.Incidents.Dedup.Window = config.Duration(30 * time.Minute)
	return c
}

func TestPipelineMatchGatedAdmitsMatching(t *testing.T) {
	enq := &capEnq{}
	p := NewPipeline(matchAllCfg(), enq, nil, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Severity: "critical", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued, got %d", len(enq.reqs))
	}
}

func TestPipelineMatchGatedDropsUnmatched(t *testing.T) {
	enq := &capEnq{}
	c := &config.Config{}
	c.Triggers.Incidents.Match.Severity = []string{"critical"}
	p := NewPipeline(c, enq, nil, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Severity: "warning", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued, got %d", len(enq.reqs))
	}
}

func TestPipelineDedupsStillFiring(t *testing.T) {
	enq := &capEnq{}
	p := NewPipeline(matchAllCfg(), enq, nil, nil)
	r := DecodeResult{Requests: []investigate.Request{{Title: "A", Fingerprint: "f1"}}}
	p.Ingest(context.Background(), MatchGated, r)
	p.Ingest(context.Background(), MatchGated, r)
	if len(enq.reqs) != 1 {
		t.Fatalf("want dedup to 1, got %d", len(enq.reqs))
	}
}

func debounceCfg(window time.Duration) *config.Config {
	c := matchAllCfg()
	d := config.Duration(window)
	c.Triggers.Incidents.Debounce = &d
	return c
}

func TestPipelineDebounceHoldsThenEnqueues(t *testing.T) {
	enq := &capEnq{}
	clk := newFakeClock()
	p := NewPipeline(debounceCfg(60*time.Second), enq, nil, nil)
	p.debounce.clock = clk

	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Fingerprint: "f1"}},
	})
	// Nothing enqueued yet — the alert is held for the window.
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued while held, got %d", len(enq.reqs))
	}
	clk.release()
	p.debounce.waitIdle()
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued after window (still active), got %d", len(enq.reqs))
	}
}

func TestPipelineDebounceDropsSelfResolvingAlert(t *testing.T) {
	enq := &capEnq{}
	clk := newFakeClock()
	p := NewPipeline(debounceCfg(60*time.Second), enq, nil, nil)
	p.debounce.clock = clk

	// Firing alert is held; a matching resolved webhook arrives within the window.
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Fingerprint: "f1"}},
	})
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Resolved: []Resolution{{Fingerprint: "f1", At: time.Now()}},
	})
	p.debounce.waitIdle()
	if len(enq.reqs) != 0 {
		t.Fatalf("want 0 enqueued (self-resolved within debounce window), got %d", len(enq.reqs))
	}
}

// TestPipelineDebounceNeverHoldsCritical is the load-bearing one. The shipped chart
// trigger matches `severity: [critical]` EXCLUSIVELY, so if the hold applied to
// criticals it would delay 100% of investigated alerts on a default install by the
// full window — violating the design invariant the coalescer already honours ("a
// debounce must never delay the first look at a critical page"). A critical must reach
// the enqueuer with the clock never released.
func TestPipelineDebounceNeverHoldsCritical(t *testing.T) {
	enq := &capEnq{}
	clk := newFakeClock()
	p := NewPipeline(debounceCfg(60*time.Second), enq, nil, nil)
	p.debounce.clock = clk // deliberately never released: a hold would hang here forever

	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "PagerNow", Severity: "critical", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 1 {
		t.Fatalf("critical must be enqueued IMMEDIATELY despite a 60s debounce, got %d enqueued", len(enq.reqs))
	}
	// Casing is Alertmanager's, not ours: CRITICAL/Critical must not sneak into the hold.
	for _, sev := range []string{"CRITICAL", "Critical"} {
		enq2 := &capEnq{}
		p2 := NewPipeline(debounceCfg(60*time.Second), enq2, nil, nil)
		p2.debounce.clock = newFakeClock()
		p2.Ingest(context.Background(), MatchGated, DecodeResult{
			Requests: []investigate.Request{{Title: "PagerNow", Severity: sev, Fingerprint: "f-" + sev}},
		})
		if len(enq2.reqs) != 1 {
			t.Fatalf("severity %q must be treated as critical (never held), got %d enqueued", sev, len(enq2.reqs))
		}
	}
}

// TestPipelineDebounceHoldsNonCritical is the flip side: a warning-grade alert IS held,
// and is dropped when it self-resolves inside the window. This is the noise/cost filter
// the debounce exists for; it just must not reach criticals.
func TestPipelineDebounceHoldsNonCritical(t *testing.T) {
	enq := &capEnq{}
	clk := newFakeClock()
	p := NewPipeline(debounceCfg(60*time.Second), enq, nil, nil)
	p.debounce.clock = clk

	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "Flappy", Severity: "warning", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 0 {
		t.Fatalf("non-critical must be held for the window, got %d enqueued", len(enq.reqs))
	}
	// It self-resolves inside the window → dropped, never investigated.
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Resolved: []Resolution{{Fingerprint: "f1", At: time.Now()}},
	})
	p.debounce.waitIdle()
	if len(enq.reqs) != 0 {
		t.Fatalf("non-critical resolved within the window must be dropped, got %d enqueued", len(enq.reqs))
	}
}

func TestPipelineDebounceZeroWindowUnchanged(t *testing.T) {
	enq := &capEnq{}
	p := NewPipeline(debounceCfg(0), enq, nil, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Requests: []investigate.Request{{Title: "A", Fingerprint: "f1"}},
	})
	if len(enq.reqs) != 1 {
		t.Fatalf("want 1 enqueued immediately with debounce=0, got %d", len(enq.reqs))
	}
}

func TestPipelineDebounceCoexistsWithDedup(t *testing.T) {
	enq := &capEnq{}
	clk := newFakeClock()
	p := NewPipeline(debounceCfg(60*time.Second), enq, nil, nil)
	p.debounce.clock = clk

	r := DecodeResult{Requests: []investigate.Request{{Title: "A", Fingerprint: "f1"}}}
	// A re-fire of the same alert is suppressed by dedup BEFORE the hold begins,
	// so only one hold is created.
	p.Ingest(context.Background(), MatchGated, r)
	p.Ingest(context.Background(), MatchGated, r)
	clk.release()
	p.debounce.waitIdle()
	if len(enq.reqs) != 1 {
		t.Fatalf("want dedup+debounce to yield 1, got %d", len(enq.reqs))
	}
}

// capCancel records CancelByFingerprint calls and returns a canned result.
type capCancel struct {
	fps []string
	ret bool
}

func (c *capCancel) CancelByFingerprint(fp string) bool {
	c.fps = append(c.fps, fp)
	return c.ret
}

// TestPipelineCancelQueuedOnResolve pins the wiring: with
// triggers.incidents.cancel_queued_on_resolve enabled (the default), a resolved alert
// also cancels its queued investigation — and the resolve itself still runs (the
// outcome ledger must close regardless of cancellation).
func TestPipelineCancelQueuedOnResolve(t *testing.T) {
	c := matchAllCfg()
	c.Triggers.Incidents.CancelQueuedOnResolve = ptr(true)
	can := &capCancel{ret: true}
	var resolved []string
	resolve := func(fp string, _ time.Time) { resolved = append(resolved, fp) }
	p := NewPipeline(c, &capEnq{}, resolve, nil).WithCanceller(can)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Resolved: []Resolution{{Fingerprint: "f9", At: time.Now()}},
	})
	if len(can.fps) != 1 || can.fps[0] != "f9" {
		t.Fatalf("want queue cancel called with f9, got %+v", can.fps)
	}
	if len(resolved) != 1 || resolved[0] != "f9" {
		t.Fatalf("resolve must still run after cancellation, got %+v", resolved)
	}
}

// TestPipelineCancelQueuedExplicitFalse pins the escape hatch: an explicit
// `cancel_queued_on_resolve: false` (the flag now defaults to TRUE) means the resolved
// path never touches the queue, even with a canceller wired — for teams who want the
// post-hoc "why did it fire?" investigation of a self-resolved alert. The resolve
// itself still runs.
func TestPipelineCancelQueuedExplicitFalse(t *testing.T) {
	c := matchAllCfg()
	c.Triggers.Incidents.CancelQueuedOnResolve = ptr(false)
	can := &capCancel{ret: true}
	var resolved []string
	resolve := func(fp string, _ time.Time) { resolved = append(resolved, fp) }
	p := NewPipeline(c, &capEnq{}, resolve, nil).WithCanceller(can)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Resolved: []Resolution{{Fingerprint: "f9", At: time.Now()}},
	})
	if len(can.fps) != 0 {
		t.Fatalf("explicit false: the queue must never be touched, got %+v", can.fps)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolve must still run with the flag off, got %+v", resolved)
	}
}

// TestPipelineCancelQueuedNilCancellerSafe pins nil-safety: the flag on with no
// canceller wired must not panic, and the resolve still runs.
func TestPipelineCancelQueuedNilCancellerSafe(t *testing.T) {
	c := matchAllCfg()
	c.Triggers.Incidents.CancelQueuedOnResolve = ptr(true)
	var resolved []string
	resolve := func(fp string, _ time.Time) { resolved = append(resolved, fp) }
	p := NewPipeline(c, &capEnq{}, resolve, nil) // no canceller
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Resolved: []Resolution{{Fingerprint: "f9", At: time.Now()}},
	})
	if len(resolved) != 1 {
		t.Fatalf("resolve must run with no canceller wired, got %+v", resolved)
	}
}

func TestPipelineRoutesResolvedToLedger(t *testing.T) {
	enq := &capEnq{}
	var resolved []string
	resolve := func(fp string, _ time.Time) { resolved = append(resolved, fp) }
	p := NewPipeline(matchAllCfg(), enq, resolve, nil)
	p.Ingest(context.Background(), MatchGated, DecodeResult{Resolved: []Resolution{{Fingerprint: "f9", At: time.Now()}}})
	if len(resolved) != 1 || resolved[0] != "f9" {
		t.Fatalf("want resolve f9, got %+v", resolved)
	}
}
