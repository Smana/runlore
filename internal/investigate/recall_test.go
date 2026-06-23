package investigate

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
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

type fakeOutcome struct {
	counts map[string]outcome.Aggregate
	err    error
}

func (f fakeOutcome) OpenCounts() (map[string]outcome.Aggregate, error) { return f.counts, f.err }

// soloRecall builds a Recall over a single strong hit that clears the margin +
// solo gates for an apps/web workload, with decay configured (k=2, floor=0.5).
func soloRecall(oc OutcomeStats) *Recall {
	return &Recall{
		Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Path: "x.md", Resource: "apps/web"}, Score: 6.0},
		}},
		MinScore: 1.5, SoloFloor: 4.0, MarginGap: 1.0,
		Outcome: oc, OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
}

func TestLookupDecayHealthyEntryRecalls(t *testing.T) {
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 5, Resolved: 5}}})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("healthy entry (factor 1.0) should recall")
	}
	if conf < 0.80 {
		t.Fatalf("healthy entry confidence should be ~undecayed, got %v", conf)
	}
}

func TestLookupDecayStaleEntryRejected(t *testing.T) {
	// recalls=4 resolved=0 → factor (0+2)/(4+2)=0.333 < floor 0.5 → reject, fall through.
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 4, Resolved: 0}}})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("stale never-resolving entry must be rejected (fall through to investigation)")
	}
}

func TestLookupDecayNoHistoryRecalls(t *testing.T) {
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{}}) // entry absent from counts
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("an entry with no outcome history must not be penalized")
	}
}

func TestLookupDecayNilOutcomeRecalls(t *testing.T) {
	r := soloRecall(nil) // no outcome stats wired
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("nil Outcome must behave as before (no decay)")
	}
}

func TestLookupDecayStatsErrorRecalls(t *testing.T) {
	r := soloRecall(fakeOutcome{err: errors.New("ledger unavailable")})
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("an outcome-stats error must degrade to a normal recall (skip decay)")
	}
}

func TestLookupDecayPartialFactorReducesConfidence(t *testing.T) {
	// recalls=3 resolved=1 → factor (1+2)/(3+2)=0.6 (> floor 0.5) → recalls, but with
	// confidence visibly reduced from the undecayed ~0.90 (0.90*0.6 = 0.54).
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 3, Resolved: 1}}})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("factor 0.6 (>= floor) should still recall")
	}
	if conf >= 0.80 || conf <= 0.40 {
		t.Fatalf("partial factor should visibly reduce confidence (~0.54), got %v", conf)
	}
}

func TestLookupDecayFactorAtFloorRecalls(t *testing.T) {
	// recalls=2 resolved=0 → factor (0+2)/(2+2)=0.5 == floor; the gate is strict (<),
	// so it recalls (boundary case).
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 2, Resolved: 0}}})
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("factor exactly at the floor must recall (gate is strict <)")
	}
}

func TestResourceAgrees(t *testing.T) {
	w := func(ns, name string) providers.Workload { return providers.Workload{Namespace: ns, Name: name} }
	cases := []struct {
		name      string
		reqW      providers.Workload
		entry     string
		requireWL bool
		want      matchStrength
	}{
		{"exact ns/name", w("apps", "payment-api"), "apps/payment-api", false, matchExact},
		{"different names same ns -> none", w("apps", "payment-api"), "apps/web", false, matchNone},
		{"named alert vs bare-ns entry -> namespace", w("apps", "payment-api"), "apps", false, matchNamespace},
		{"bare-ns alert vs named entry -> namespace", w("apps", ""), "apps/web", false, matchNamespace},
		{"both bare ns -> exact", w("apps", ""), "apps", false, matchExact},
		{"different ns -> none", w("apps", "payment-api"), "other/web", false, matchNone},
		{"empty entry -> none", w("apps", "payment-api"), "", false, matchNone},
		{"require workload + exact -> exact", w("apps", "web"), "apps/web", true, matchExact},
		{"require workload + ns-only -> none", w("apps", ""), "apps/web", true, matchNone},
		{"require workload + bare-ns entry -> none", w("apps", "web"), "apps", true, matchNone},
		{"bare-ns alert vs different-ns bare-ns entry -> none", w("apps", ""), "other", false, matchNone},
	}
	for _, c := range cases {
		if got := resourceAgrees(c.reqW, c.entry, c.requireWL); got != c.want {
			t.Errorf("%s: resourceAgrees(%+v, %q, %v) = %v, want %v", c.name, c.reqW, c.entry, c.requireWL, got, c.want)
		}
	}
}

func TestLookupDecayRejectionMetric(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 4, Resolved: 0}}})
	r.Metrics = telemetry.NewMetrics()
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("stale entry should be rejected")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `reason="low_outcome"`) {
		t.Fatalf("expected recall_rejections_total{reason=\"low_outcome\"} in metrics:\n%s", rec.Body.String())
	}
}
