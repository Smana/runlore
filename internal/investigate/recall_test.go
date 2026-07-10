// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	// Two entries for the SAME workload with a near-tie (gap 0.5 < MarginGap 1.0) →
	// genuinely ambiguous → falls through. (Post-structural-filter the margin compares
	// same-workload candidates, not arbitrary lexical neighbours.)
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "BadImage", Path: "b.md", Resource: "apps/web"}, Score: 5.5},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("near-tie among same-workload entries (gap 0.5 < 1.0) must fall through")
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

func TestRecalledInvestigationStampsAlertMetadata(t *testing.T) {
	// The recall short-circuit must carry the same trigger-time facts as the full
	// loop so a recalled delivery renders an identical notification metadata block.
	start := time.Now().Add(-time.Minute)
	req := Request{
		Title:       "x",
		Severity:    "critical",
		Environment: "staging",
		At:          start,
		Labels:      map[string]string{"alertname": "HarborProbeFailure", "cluster": "c1", "tenant": "t1"},
	}
	inv := recalledInvestigation(req, catalog.Entry{Title: "T", Path: "p.md"}, 0.7)
	if inv.Severity != "critical" || inv.Environment != "staging" {
		t.Fatalf("recall path must stamp severity/environment, got %+v", inv)
	}
	if inv.Cluster != "c1" || inv.Tenant != "t1" || inv.AlertName != "HarborProbeFailure" {
		t.Fatalf("recall path must stamp cluster/tenant/alertname, got %+v", inv)
	}
	if !inv.StartedAt.Equal(start) {
		t.Fatalf("recall path must stamp StartedAt: got %v want %v", inv.StartedAt, start)
	}
}

func TestOutcomeFactor(t *testing.T) {
	const k = 2.0 // default prior strength; k=2 ⇒ documented Beta(1,1): (resolved+up+1)/(recalls+up+down+2)
	cases := []struct {
		recalls, resolved, up, down int
		want                        float64
	}{
		// resolved=0, recalls 0..3 — a never-resolving entry decays fast.
		{0, 0, 0, 0, 0.5},       // (0+1)/(0+2) — prior mean; never reaches the gate in practice
		{1, 0, 0, 0, 1.0 / 3.0}, // (0+1)/(1+2) ≈ 0.333 — already below the 0.5 floor at the 1st recall
		{2, 0, 0, 0, 0.25},      // (0+1)/(2+2)
		{3, 0, 0, 0, 0.2},       // (0+1)/(3+2)
		{6, 0, 0, 0, 0.125},     // (0+1)/(6+2)
		// mixed resolve records.
		{3, 1, 0, 0, 0.4},       // (1+1)/(3+2)
		{3, 2, 0, 0, 0.6},       // (2+1)/(3+2)
		{4, 2, 0, 0, 0.5},       // (2+1)/(4+2) — exactly at the floor
		{5, 5, 0, 0, 6.0 / 7.0}, // (5+1)/(5+2) ≈ 0.857 — always-resolving asymptotes to 1, never exceeds it
		// human feedback — extra Bernoulli observations in the same posterior. The
		// zero-recall rows are the non-resolvable-source case (GitOps): feedback is
		// the ONLY evidence such entries can ever accumulate.
		{0, 0, 2, 0, 0.75},      // (0+2+1)/(0+2+2) — two 👍, no resolves: trust builds
		{0, 0, 0, 2, 0.25},      // (0+0+1)/(0+2+2) — two 👎: below the 0.5 floor, recall rejected
		{0, 0, 0, 1, 1.0 / 3.0}, // one 👎 weighs exactly like one unresolved recall
		{3, 2, 1, 1, 4.0 / 7.0}, // (2+1+1)/(3+1+1+2) — feedback blends with resolves
		{5, 5, 0, 3, 0.6},       // (5+0+1)/(5+0+3+2) — 👎 erode even a perfect resolve record
	}
	for _, c := range cases {
		got := outcomeFactor(c.recalls, c.resolved, c.up, c.down, k)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("outcomeFactor(%d,%d,%d,%d,%v) = %v, want %v", c.recalls, c.resolved, c.up, c.down, k, got, c.want)
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

func TestLookupDecayFeedbackDownsReject(t *testing.T) {
	// A non-resolvable-source entry (recalls=0, GitOps golden path) with two human
	// 👎 and no 👍: factor = (0+0+1)/(0+0+2+2) = 0.25 < 0.5 floor → the recall is
	// rejected and the incident falls through to a full investigation. This is the
	// "click 👎 and the agent stops trusting this knowledge" contract.
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {FeedbackDown: 2}}})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("two 👎 with no successes must reject the recall")
	}
}

func TestLookupDecayFeedbackUpsBuildTrust(t *testing.T) {
	// Same entry with two 👍: factor = (0+2+1)/(0+2+2) = 0.75 → recalls, with
	// confidence biased by the human track record instead of stuck at the prior.
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {FeedbackUp: 2}}})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("👍-endorsed entry must recall")
	}
	if conf <= 0 {
		t.Fatalf("confidence must be positive, got %v", conf)
	}
}

func TestLookupDecayHealthyEntryRecalls(t *testing.T) {
	// recalls=5 resolved=5 → factor (5+1)/(5+2)=6/7≈0.857 (a Beta posterior asymptotes
	// to 1 with evidence but never reaches it), so confidence stays high but not maxed.
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 5, Resolved: 5}}})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("healthy entry (factor ~0.857) should recall")
	}
	if conf < 0.75 {
		t.Fatalf("consistently-resolving entry confidence should stay high, got %v", conf)
	}
}

func TestLookupDecayStaleEntryRejected(t *testing.T) {
	// recalls=1 resolved=0 → factor (0+1)/(1+2)=0.333 < floor 0.5 → reject at the FIRST
	// recall, fall through to a full investigation. This is the stricter Beta(1,1) gate.
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 1, Resolved: 0}}})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("an entry that recalls once and never resolves must be rejected (fall through to investigation)")
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
	// recalls=3 resolved=2 → factor (2+1)/(3+2)=0.6 (> floor 0.5) → recalls, but with
	// confidence visibly reduced from the undecayed ~0.90 (0.90*0.6 = 0.54).
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 3, Resolved: 2}}})
	e, conf := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("factor 0.6 (>= floor) should still recall")
	}
	if conf >= 0.80 || conf <= 0.40 {
		t.Fatalf("partial factor should visibly reduce confidence (~0.54), got %v", conf)
	}
}

func TestLookupDecayFactorAtFloorRecalls(t *testing.T) {
	// recalls=4 resolved=2 → factor (2+1)/(4+2)=0.5 == floor; the gate is strict (<),
	// so it recalls (boundary case).
	r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": {Recalls: 4, Resolved: 2}}})
	if e, _ := r.lookup(context.Background(), okReq()); e == nil {
		t.Fatal("factor exactly at the floor must recall (gate is strict <)")
	}
}

func TestLookupRecallsMatchingWorkload(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "payment.md", Resource: "apps/payment-api"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "X", Path: "b.md"}, Score: 2.0},
	})
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps", Name: "payment-api"}}
	if e, _ := r.lookup(context.Background(), req); e == nil {
		t.Fatal("an alert for payment-api should recall the payment-api entry (exact match)")
	}
}

func TestLookupDoesNotRecallDifferentWorkloadSameNamespace(t *testing.T) {
	// The disambiguation success metric: a payment-api alert must NOT recall a
	// different workload's (web) entry just because they share the namespace.
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "web.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "X", Path: "b.md"}, Score: 2.0},
	})
	req := Request{Title: "crashloop", Workload: providers.Workload{Namespace: "apps", Name: "payment-api"}}
	if e, _ := r.lookup(context.Background(), req); e != nil {
		t.Fatal("a payment-api alert must not recall a different workload's entry in the same namespace")
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

func TestLookupStructuralWinnerBelowTopLexical(t *testing.T) {
	// The two highest lexical hits are DIFFERENT workloads; the structurally-correct
	// entry (apps/web) is only the 3rd lexical hit. Pre-filtering must surface it —
	// the old k=2 / top-hit-only logic could not.
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OtherA", Path: "a.md", Resource: "apps/api"}, Score: 9.0},
		{Entry: catalog.Entry{Title: "OtherB", Path: "b.md", Resource: "apps/worker"}, Score: 8.0},
		{Entry: catalog.Entry{Title: "Web OOM", Path: "web.md", Resource: "apps/web"}, Score: 5.0},
	})
	e, _ := r.lookup(context.Background(), okReq())
	if e == nil || e.Path != "web.md" {
		t.Fatalf("expected the structurally-correct apps/web entry (web.md) to be recalled, got %+v", e)
	}
}

func TestLookupMarginAmongAgreeingClearWinner(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Web recent", Path: "web1.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "Web old", Path: "web2.md", Resource: "apps/web"}, Score: 2.0},
	})
	e, _ := r.lookup(context.Background(), okReq())
	if e == nil || e.Path != "web1.md" {
		t.Fatalf("clear same-workload winner should recall web1.md, got %+v", e)
	}
}

func TestLookupNoAgreeingCandidateFallsThrough(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "A", Path: "a.md", Resource: "apps/api"}, Score: 9.0},
		{Entry: catalog.Entry{Title: "B", Path: "b.md", Resource: "other/web"}, Score: 8.0},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("no structurally-agreeing candidate must fall through")
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

func TestLookupMarginAmongAgreeingWithDistractor(t *testing.T) {
	// Two agreeing same-workload entries separated by a higher-scoring wrong-workload
	// distractor. The winner must be the higher-lexical agreeing entry (web1.md @6.0),
	// and the margin its gap to the next AGREEING entry (web2.md @2.0 → gap 4.0 >= 1.0),
	// not the distractor (api.md). So it recalls web1.md.
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Web hot", Path: "web1.md", Resource: "apps/web"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "API noise", Path: "api.md", Resource: "apps/api"}, Score: 5.0},
		{Entry: catalog.Entry{Title: "Web cold", Path: "web2.md", Resource: "apps/web"}, Score: 2.0},
	})
	e, _ := r.lookup(context.Background(), okReq()) // okReq workload = apps/web
	if e == nil || e.Path != "web1.md" {
		t.Fatalf("winner must be the higher-lexical agreeing entry web1.md, got %+v", e)
	}
}
