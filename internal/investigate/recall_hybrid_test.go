package investigate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

type fakeHybrid struct {
	hits   []catalog.ScoredEntry
	hasVec bool
}

func (f fakeHybrid) SearchHybrid(context.Context, string, int) ([]catalog.ScoredEntry, error) {
	return f.hits, nil
}
func (f fakeHybrid) HasVectors() bool { return f.hasVec }

// TestRecallHybridGatesOnCosine: with a live hybrid searcher, recall uses the COSINE
// thresholds, not the BM25 ones (set impossibly high here to prove the path).
func TestRecallHybridGatesOnCosine(t *testing.T) {
	hyb := fakeHybrid{hasVec: true, hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Harbor", Path: "harbor.md", Resource: "apps"}, Score: 0.86},
		{Entry: catalog.Entry{Title: "Other", Path: "other.md", Resource: "apps"}, Score: 0.50},
	}}
	r := &Recall{
		Catalog: fakeScored{}, Hybrid: hyb,
		HybridMinScore: 0.80, HybridMarginGap: 0.10,
		MinScore: 999, SoloFloor: 999, MarginGap: 999, // BM25 gates impossible on purpose
	}
	e, conf := r.lookup(context.Background(), Request{Title: "harbor down", Workload: providers.Workload{Namespace: "apps"}})
	if e == nil || e.Path != "harbor.md" {
		t.Fatalf("hybrid recall should return harbor.md via the cosine gate, got %v", e)
	}
	if conf <= 0 || conf > 0.9 {
		t.Fatalf("confidence out of range: %v", conf)
	}
}

func TestRecallHybridRejectsBelowCosineFloor(t *testing.T) {
	hyb := fakeHybrid{hasVec: true, hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Weak", Path: "weak.md", Resource: "apps"}, Score: 0.40}, // < 0.80 floor
	}}
	r := &Recall{Catalog: fakeScored{}, Hybrid: hyb, HybridMinScore: 0.80, HybridMarginGap: 0.10}
	if e, _ := r.lookup(context.Background(), Request{Title: "x", Workload: providers.Workload{Namespace: "apps"}}); e != nil {
		t.Fatalf("a below-floor cosine hit must be rejected, got %v", e)
	}
}

// TestRecallHybridFallsBackToBM25WithoutVectors: a Hybrid that has no vectors yet
// (embedder configured but corpus not embedded / embed failed) must use the BM25
// path + BM25 thresholds — zero regression.
func TestRecallHybridFallsBackToBM25WithoutVectors(t *testing.T) {
	r := &Recall{
		Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "BM25", Path: "bm.md", Resource: "apps"}, Score: 8.0}}},
		Hybrid:   fakeHybrid{hasVec: false},
		MinScore: 2.0, SoloFloor: 2.0,
		HybridMinScore: 0.80,
	}
	e, _ := r.lookup(context.Background(), Request{Title: "x", Workload: providers.Workload{Namespace: "apps"}})
	if e == nil || e.Path != "bm.md" {
		t.Fatalf("no vectors → BM25 fallback should return bm.md, got %v", e)
	}
}
