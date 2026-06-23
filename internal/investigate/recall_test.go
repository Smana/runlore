package investigate

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// recallWith builds a Recall over fixed hits with sensible trust thresholds.
func recallWith(hits []catalog.ScoredEntry) *Recall {
	return &Recall{Catalog: fakeScored{hits: hits}, MinScore: 1.5, MarginGap: 1.0, SoloFloor: 4.0}
}

// okReq is a request whose workload matches the "apps/web" test entries.
func okReq() Request {
	return Request{Title: "pod crashloop", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
}

// TestRecallScoreRecordedNilSafe verifies a Recall with nil Metrics does not panic
// when a hit is found and scored.
func TestRecallScoreRecordedNilSafe(t *testing.T) {
	r := &Recall{
		Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "x", Path: "x.md", Resource: "ns"}, Score: 5.0}}},
		MinScore: 2.0, SoloFloor: 2.0,
		Metrics: nil, // no-op: must not panic
	}
	entry, conf := r.lookup(context.Background(), Request{Title: "incident", Workload: providers.Workload{Namespace: "ns"}})
	if entry == nil {
		t.Fatal("expected a confident hit")
	}
	if conf <= 0 || conf > 0.9 {
		t.Fatalf("derived confidence out of range: %v", conf)
	}
}

// TestRecallScoreRecordedRealProvider verifies a hit records the BM25 score in the
// OTel histogram, scraped via the Prometheus exposition.
func TestRecallScoreRecordedRealProvider(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := telemetry.NewMetrics() // instruments bound to the real provider
	r := &Recall{
		Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "Known", Path: "known.md", Resource: "ns"}, Score: 7.5}}},
		MinScore: 2.0, SoloFloor: 2.0,
		Metrics: m,
	}
	entry, _ := r.lookup(context.Background(), Request{Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "ns"}})
	if entry == nil {
		t.Fatal("expected a confident hit")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "runlore_recall_score") {
		t.Fatalf("runlore_recall_score not found in metrics output:\n%s", rec.Body.String())
	}
}

func TestLookupMarginClearWinner(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "BadImage", Path: "b.md"}, Score: 2.0},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("clear winner (gap 4.0 >= 1.0) should recall")
	}
}

func TestLookupMarginNearTieFallsThrough(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "BadImage", Path: "b.md"}, Score: 5.5},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("near-tie (gap 0.5 < 1.0) must fall through")
	}
}

func TestLookupLoneWeakHitFallsThrough(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web"}, Score: 3.0}, // below SoloFloor 4.0
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("a lone hit below solo_floor must fall through")
	}
}

func structuralRecall(entryResource string, requireWorkload bool) *Recall {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: entryResource}, Score: 6.0},
		{Entry: catalog.Entry{Title: "X", Path: "b.md"}, Score: 2.0},
	})
	r.RequireWorkloadMatch = requireWorkload
	return r
}

func TestLookupStructuralMatch(t *testing.T) {
	r := structuralRecall("apps/web", false)
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if e, _ := r.lookup(context.Background(), req); e == nil {
		t.Fatal("exact resource match should recall")
	}
}

func TestLookupStructuralNamespaceOnly(t *testing.T) {
	r := structuralRecall("apps/web", false)
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps"}} // name unknown from the alert
	if e, _ := r.lookup(context.Background(), req); e == nil {
		t.Fatal("namespace agreement should recall when require_workload_match=false")
	}
}

func TestLookupStructuralMismatchFallsThrough(t *testing.T) {
	r := structuralRecall("apps/web", false)
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "kube-system"}}
	if e, _ := r.lookup(context.Background(), req); e != nil {
		t.Fatal("different namespace must fall through")
	}
}

func TestLookupNoStoredResourceFallsThrough(t *testing.T) {
	r := structuralRecall("", false) // entry predates the write-side change
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps"}}
	if e, _ := r.lookup(context.Background(), req); e != nil {
		t.Fatal("entry with no stored resource must fall through (fail-safe)")
	}
}

func TestDeriveRecallConfidence(t *testing.T) {
	hi := deriveRecallConfidence(8.0, 6.0, matchExact)
	if hi <= 0.8 || hi > 0.9 {
		t.Fatalf("decisive+exact should be (0.8, 0.9], got %v", hi)
	}
	lo := deriveRecallConfidence(2.0, 0.4, matchNamespace)
	if lo >= hi || lo < 0.5 {
		t.Fatalf("marginal+namespace should be lower and >= 0.5, got %v (hi=%v)", lo, hi)
	}
}

func TestRecalledInvestigationUsesDerivedConfidence(t *testing.T) {
	inv := recalledInvestigation(Request{Title: "x"}, catalog.Entry{Title: "T", Description: "D", Path: "p.md"}, 0.72)
	if inv.Confidence != 0.72 || inv.RootCauses[0].Confidence != 0.72 {
		t.Fatalf("recalledInvestigation must use the derived confidence, got %+v", inv)
	}
}

func TestRecalledInvestigationCarriesEntryPath(t *testing.T) {
	inv := recalledInvestigation(Request{Title: "x"}, catalog.Entry{Title: "T", Path: "p.md"}, 0.7)
	if inv.RecalledEntry != "p.md" {
		t.Fatalf("RecalledEntry = %q, want p.md", inv.RecalledEntry)
	}
}

func TestOutcomeFactor(t *testing.T) {
	const k = 2.0
	cases := []struct {
		recalls, resolved int
		want              float64
	}{
		{0, 0, 1.0},  // no history → no penalty
		{5, 5, 1.0},  // always resolves → no penalty
		{3, 0, 0.4},  // (0+2)/(3+2)
		{6, 0, 0.25}, // (0+2)/(6+2)
		{3, 1, 0.6},  // (1+2)/(3+2) — partial resolve rate
	}
	for _, c := range cases {
		got := outcomeFactor(c.recalls, c.resolved, k)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("outcomeFactor(%d,%d,%v) = %v, want %v", c.recalls, c.resolved, k, got, c.want)
		}
		if got > 1.0 {
			t.Errorf("factor must be <= 1.0, got %v", got)
		}
	}
}
