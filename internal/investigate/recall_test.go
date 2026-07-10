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

// A recall quotes the entry's Cause + Resolution into Prior so the notifier can make
// the cache hit substantive ("⚡ instant recall") instead of a bare low-confidence
// finding. The resolve-rate is filled in downstream (needs the ledger).
func TestRecalledInvestigationStampsPrior(t *testing.T) {
	e := catalog.Entry{Title: "T", Path: "p.md", Body: "## Cause\nIAM quota hit\n\n## Resolution\ndelete a key\n"}
	inv := recalledInvestigation(Request{Title: "x"}, e, 0.7)
	if inv.Prior == nil {
		t.Fatal("recall must stamp Prior from the matched entry")
	}
	if inv.Prior.Cause != "IAM quota hit" || inv.Prior.Resolution != "delete a key" || inv.Prior.EntryPath != "p.md" {
		t.Fatalf("Prior not stamped from entry sections: %+v", inv.Prior)
	}
	// An entry with no Cause/Resolution sections leaves Prior nil (nothing to quote).
	if got := recalledInvestigation(Request{Title: "x"}, catalog.Entry{Title: "T", Path: "p.md"}, 0.7); got.Prior != nil {
		t.Fatalf("Prior must be nil when the entry has no Cause/Resolution: %+v", got.Prior)
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
		// Scopeless tier: a request with NO workload at all (PagerDuty incidents carry
		// no Kubernetes namespace/name) agrees ONLY with entries that are themselves
		// resource-less — the weakest tier, and strict mode disables it entirely.
		{"scopeless request vs scopeless entry -> scopeless", w("", ""), "", false, matchScopeless},
		{"scopeless request vs named entry -> none", w("", ""), "apps/web", false, matchNone},
		{"scopeless request vs bare-ns entry -> none", w("", ""), "apps", false, matchNone},
		{"require workload + scopeless both sides -> none (strict stays strict)", w("", ""), "", true, matchNone},
		// A name without a namespace is partial workload info, not scopeless — the
		// conservative reading refuses the weakest tier when ANY scope hint exists.
		{"nameless-namespace request with name vs scopeless entry -> none", w("", "web"), "", false, matchNone},
	}
	for _, c := range cases {
		if got := resourceAgrees(c.reqW, c.entry, c.requireWL); got != c.want {
			t.Errorf("%s: resourceAgrees(%+v, %q, %v) = %v, want %v", c.name, c.reqW, c.entry, c.requireWL, got, c.want)
		}
	}
}

// TestResourceAgreesNormalizesPodHash is the live-found recall bug: a pod-scoped
// alert (KubePodNotReady carries only a `pod` label — no deployment/workload label)
// yields a Workload whose Name is the FULL pod name INCLUDING the volatile
// ReplicaSet/pod-hash suffix, e.g. tooling/harbor-registry-59598dbd57-ltkzw. The
// structural gate must strip that suffix off the NAME segment on BOTH sides so the
// request still agrees (matchExact) with the normalized controller family stored on
// the KB entry (tooling/harbor-registry) — otherwise a perfect KB entry is skipped
// and a full paid investigation runs. It must NOT loosen anything: distinct
// workloads still disagree, strict mode is unaffected, the scopeless tier holds.
func TestResourceAgreesNormalizesPodHash(t *testing.T) {
	w := func(ns, name string) providers.Workload { return providers.Workload{Namespace: ns, Name: name} }
	cases := []struct {
		name      string
		reqW      providers.Workload
		entry     string
		requireWL bool
		want      matchStrength
	}{
		// (i) full pod-name request vs the normalized-workload entry → exact.
		{"full pod name req vs normalized entry", w("tooling", "harbor-registry-59598dbd57-ltkzw"), "tooling/harbor-registry", false, matchExact},
		// (ii) reverse — the entry itself carries a pod hash (written before the
		// curator-side CORE-681 fix, like the live duplicate), request normalized.
		{"normalized req vs hashed entry", w("tooling", "harbor-registry"), "tooling/harbor-registry-59598dbd57-ltkzw", false, matchExact},
		// both sides carry a DIFFERENT hash of the same family → still exact.
		{"both sides hashed same family", w("tooling", "harbor-registry-59598dbd57-ltkzw"), "tooling/harbor-registry-abcdef1234-qrstu", false, matchExact},
		// (iii) two DIFFERENT workloads must still NOT match after normalization.
		{"different workloads still none", w("tooling", "harbor-registry-59598dbd57-ltkzw"), "tooling/harbor-core", false, matchNone},
		// (iv) strict require_workload_match: the normalized family match still exact…
		{"strict + normalized family exact", w("tooling", "harbor-registry-59598dbd57-ltkzw"), "tooling/harbor-registry", true, matchExact},
		// …but a genuine mismatch stays none under strict.
		{"strict + different workload none", w("tooling", "harbor-registry-59598dbd57-ltkzw"), "tooling/harbor-core", true, matchNone},
		// normalization must never cross a namespace boundary.
		{"same family different namespace none", w("tooling", "harbor-registry-59598dbd57-ltkzw"), "other/harbor-registry", false, matchNone},
		// (v) the scopeless tier is untouched by name normalization.
		{"scopeless still scopeless", w("", ""), "", false, matchScopeless},
	}
	for _, c := range cases {
		if got := resourceAgrees(c.reqW, c.entry, c.requireWL); got != c.want {
			t.Errorf("%s: resourceAgrees(%+v, %q, %v) = %v, want %v", c.name, c.reqW, c.entry, c.requireWL, got, c.want)
		}
	}
}

// TestLookupRecallsPodScopedAlert is the end-to-end live-found bug: a pod-scoped
// alert whose workload name carries the volatile pod-hash suffix must now FIRE
// instant recall against a KB entry whose resource is the normalized controller
// family — where before the fix it rejected with no_resource_match and ran a full
// paid investigation.
func TestLookupRecallsPodScopedAlert(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Harbor registry pod not ready", Path: "harbor.md", Resource: "tooling/harbor-registry"}, Score: 6.0},
		{Entry: catalog.Entry{Title: "unrelated", Path: "b.md"}, Score: 2.0},
	})
	req := Request{Title: "KubePodNotReady", Workload: providers.Workload{Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"}}
	e, _ := r.lookup(context.Background(), req)
	if e == nil || e.Path != "harbor.md" {
		t.Fatalf("a pod-scoped alert must recall the normalized-workload entry harbor.md, got %+v", e)
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

// TestScopelessNeverLoosensScopedMatching states the scopeless-tier invariant: the
// new tier applies ONLY when BOTH sides carry no scope. A request with any workload
// scope never matches a resource-less entry, and a scopeless request never matches
// an entry with any stored resource — the existing Kubernetes matching semantics
// are untouched in every direction.
func TestScopelessNeverLoosensScopedMatching(t *testing.T) {
	scopedReqs := []providers.Workload{
		{Namespace: "apps", Name: "web"},
		{Namespace: "apps"}, // namespace-only alert is still scoped
	}
	for _, reqW := range scopedReqs {
		for _, strict := range []bool{false, true} {
			if got := resourceAgrees(reqW, "", strict); got != matchNone {
				t.Errorf("scoped request %+v vs resource-less entry (strict=%v) = %v, want matchNone", reqW, strict, got)
			}
		}
	}
	for _, entry := range []string{"apps/web", "apps", "other"} {
		for _, strict := range []bool{false, true} {
			if got := resourceAgrees(providers.Workload{}, entry, strict); got != matchNone {
				t.Errorf("scopeless request vs scoped entry %q (strict=%v) = %v, want matchNone", entry, strict, got)
			}
		}
	}
}

// scopelessReq is a request with no Kubernetes workload at all — the PagerDuty shape.
func scopelessReq() Request {
	return Request{Title: "checkout latency spike", Workload: providers.Workload{}}
}

// TestLookupScopeless drives the recall gate end-to-end for workload-less requests:
// a scopeless request may recall ONLY resource-less entries, and — because a
// scopeless match carries zero structural evidence — the winner must clear the solo
// floor AND min score no matter how many candidates agree (the margin gate alone is
// too weak). Strict mode (require_workload_match) disables the tier entirely.
func TestLookupScopeless(t *testing.T) {
	entry := func(path, resource string, score float64) catalog.ScoredEntry {
		return catalog.ScoredEntry{Entry: catalog.Entry{Title: "Checkout runbook", Path: path, Resource: resource}, Score: score}
	}
	cases := []struct {
		name       string
		hits       []catalog.ScoredEntry
		strict     bool
		wantRecall bool
	}{
		{
			// Lone resource-less entry clearing solo floor (4.0) + min score (1.5).
			"scopeless solo hit above solo floor recalls",
			[]catalog.ScoredEntry{entry("runbook.md", "", 6.0)},
			false, true,
		},
		{
			// Two agreeing scopeless entries with a decisive margin (4.0 >= 1.0) AND a
			// winner above the solo floor: all gates clear.
			"scopeless multi-candidate clearing solo floor recalls",
			[]catalog.ScoredEntry{entry("runbook.md", "", 6.0), entry("old.md", "", 2.0)},
			false, true,
		},
		{
			// Margin alone would pass (2.5 >= 1.0, min 1.5 met) but the winner is below
			// the solo floor (3.5 < 4.0): without structural evidence that is not enough.
			"scopeless multi-candidate below solo floor falls through",
			[]catalog.ScoredEntry{entry("runbook.md", "", 3.5), entry("old.md", "", 1.0)},
			false, false,
		},
		{
			// A scopeless request must never recall scoped entries, however strong.
			"scopeless request vs scoped entries falls through",
			[]catalog.ScoredEntry{entry("web.md", "apps/web", 9.0), entry("ns.md", "apps", 8.0)},
			false, false,
		},
		{
			// Strict mode: require_workload_match promises exact namespace+name
			// agreement, which a scopeless pair can never provide.
			"strict mode never recalls scopeless",
			[]catalog.ScoredEntry{entry("runbook.md", "", 6.0)},
			true, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := recallWith(c.hits)
			r.RequireWorkloadMatch = c.strict
			e, conf := r.lookup(context.Background(), scopelessReq())
			if got := e != nil; got != c.wantRecall {
				t.Fatalf("recall = %v (entry %+v), want %v", got, e, c.wantRecall)
			}
			if e != nil && conf > 0.70 {
				t.Fatalf("scopeless recall confidence must stay low (<= 0.70), got %v", conf)
			}
		})
	}
}

// TestDeriveRecallConfidenceScopelessWeakest pins the confidence ordering: at any
// identical (score, margin), scopeless < namespace < exact — a workload-less recall
// must never look as trustworthy as a structurally-anchored one — and the scopeless
// ceiling stays below the namespace tier's even at a maximal margin.
func TestDeriveRecallConfidenceScopelessWeakest(t *testing.T) {
	points := []struct{ score, margin float64 }{
		{8.0, 6.0}, // decisive winner
		{6.0, 6.0}, // solo hit (margin == score)
		{2.0, 0.4}, // marginal winner
	}
	for _, p := range points {
		sl := deriveRecallConfidence(p.score, p.margin, matchScopeless)
		ns := deriveRecallConfidence(p.score, p.margin, matchNamespace)
		ex := deriveRecallConfidence(p.score, p.margin, matchExact)
		if !(sl < ns && ns < ex) {
			t.Errorf("at (%v, %v): want scopeless < namespace < exact, got %v, %v, %v", p.score, p.margin, sl, ns, ex)
		}
		if sl > 0.70 {
			t.Errorf("at (%v, %v): scopeless confidence must be capped at 0.70, got %v", p.score, p.margin, sl)
		}
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
