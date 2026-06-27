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
	c.Triggers.Incidents.Enabled = true // empty Match ⇒ matches anything
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
	c.Triggers.Incidents.Enabled = true
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
