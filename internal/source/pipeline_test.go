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
	c.Triggers.Incidents.Debounce = config.Duration(window)
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

// TestPipelineCancelQueuedOnResolve pins the opt-in wiring: with
// triggers.incidents.cancel_queued_on_resolve enabled, a resolved alert also
// cancels its queued investigation — and the resolve itself still runs (the
// outcome ledger must close regardless of cancellation).
func TestPipelineCancelQueuedOnResolve(t *testing.T) {
	c := matchAllCfg()
	c.Triggers.Incidents.CancelQueuedOnResolve = true
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

// TestPipelineCancelQueuedDisabledByDefault pins behavior preservation: with the
// flag off (the default) the resolved path never touches the queue, even when a
// canceller is wired — bit-for-bit today's behavior.
func TestPipelineCancelQueuedDisabledByDefault(t *testing.T) {
	can := &capCancel{ret: true}
	var resolved []string
	resolve := func(fp string, _ time.Time) { resolved = append(resolved, fp) }
	p := NewPipeline(matchAllCfg(), &capEnq{}, resolve, nil).WithCanceller(can)
	p.Ingest(context.Background(), MatchGated, DecodeResult{
		Resolved: []Resolution{{Fingerprint: "f9", At: time.Now()}},
	})
	if len(can.fps) != 0 {
		t.Fatalf("opt-in off: the queue must never be touched, got %+v", can.fps)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolve must still run with the flag off, got %+v", resolved)
	}
}

// TestPipelineCancelQueuedNilCancellerSafe pins nil-safety: the flag on with no
// canceller wired must not panic, and the resolve still runs.
func TestPipelineCancelQueuedNilCancellerSafe(t *testing.T) {
	c := matchAllCfg()
	c.Triggers.Incidents.CancelQueuedOnResolve = true
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
