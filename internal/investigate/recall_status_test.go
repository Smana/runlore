// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
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

// TestRecallSkipsRetiredEntry: a retired entry that would otherwise be the
// structurally-agreeing winner is filtered before the gate, so — when it is the
// only candidate — the recall falls through with no_resource_match, exactly as if
// the entry were absent. This is what makes the retirement pass effective.
func TestRecallSkipsRetiredEntry(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web", Status: "retired"}, Score: 6.0},
	})
	r.Metrics = telemetry.NewMetrics()
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("a retired entry must never fire")
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `reason="no_resource_match"`) {
		t.Fatalf("a retired sole candidate must reject as no_resource_match:\n%s", rec.Body.String())
	}
}

// TestRecallSkipsDraftEntry: a draft entry is inactive too — it never fires.
func TestRecallSkipsDraftEntry(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web", Status: "draft"}, Score: 6.0},
	})
	if e, _ := r.lookup(context.Background(), okReq()); e != nil {
		t.Fatal("a draft entry must never fire")
	}
}

// TestRecallSkipsRetiredWinnerFiresActiveRunnerUp: with a retired entry filtered
// out, an active same-workload candidate below it becomes the lone agreeing hit and
// fires (clearing the solo bar). The retired entry is simply invisible to recall.
func TestRecallSkipsRetiredWinnerFiresActiveRunnerUp(t *testing.T) {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "retired.md", Resource: "apps/web", Status: "retired"}, Score: 9.0},
		{Entry: catalog.Entry{Title: "OOM fixed", Path: "active.md", Resource: "apps/web"}, Score: 6.0},
	})
	e, _ := r.lookup(context.Background(), okReq())
	if e == nil {
		t.Fatal("the active runner-up must fire once the retired winner is filtered")
	}
	if e.Path != "active.md" {
		t.Fatalf("fired %q, want the active entry active.md", e.Path)
	}
}

// TestRecallForeignAndEmptyStatusFire pins the OKF §9 tolerance invariant: an
// absent status and any foreign status value fire exactly as before the field
// existed (only retired/draft are inactive).
func TestRecallForeignAndEmptyStatusFire(t *testing.T) {
	for _, status := range []string{"", "active", "SomeForeignState"} {
		r := recallWith([]catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web", Status: status}, Score: 6.0},
		})
		if e, _ := r.lookup(context.Background(), okReq()); e == nil {
			t.Fatalf("status=%q must fire exactly as today", status)
		}
	}
}

// TestRecallRetiredRunnerUpNeverFiresInFallback: even on the outcome-decay fallback
// path, a retired candidate must never be fired. The winner (stale) is outcome-
// rejected; the only other agreeing candidate is retired, so the fallback filters it
// out and the recall falls through — the retired entry never resurfaces.
func TestRecallRetiredRunnerUpNeverFiresInFallback(t *testing.T) {
	r := &Recall{
		Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Path: "stale.md", Resource: "apps/web"}, Score: 9.0},
			{Entry: catalog.Entry{Path: "retired.md", Resource: "apps/web", Status: "retired"}, Score: 6.0},
		}},
		MinScore: 1.5, SoloFloor: 4.0, MarginGap: 1.0,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			"stale.md": {Recalls: 4, Resolved: 0}, // factor 1/6 < 0.5 → rejected winner
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), okReq(), nil)
	if e != nil {
		t.Fatalf("a retired runner-up must never fire in the fallback, got %q", e.Path)
	}
	if len(rejected) != 1 || rejected[0] != "stale.md" {
		t.Fatalf("rejected = %v, want exactly the decayed winner stale.md", rejected)
	}
}

// TestNearMissNeverReturnsRetired: a retired entry must not resurface as an
// unverified near-miss lead either — an entry retired for being wrong is not a hint.
func TestNearMissNeverReturnsRetired(t *testing.T) {
	r := &Recall{
		MinScore: 4.0, MarginGap: 2.0, SoloFloor: 4.0, // gate unreachable: exercise the near-miss path
		Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "old", Path: "old.md", Resource: "apps/web", Status: "retired"}, Score: 6.0},
		}},
	}
	if nm := r.nearMiss(context.Background(), Request{Title: "x", Workload: providers.Workload{Namespace: "apps", Name: "web"}}); nm != nil {
		t.Fatalf("a retired entry must not be offered as a near-miss lead, got %q", nm.Path)
	}
}
