// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
)

// staleRecall builds a lone-strong-hit recall (fires for okReq) with a fixed clock
// and controllable freshness dates, so an age down-weight can be observed against a
// StaleAfter:0 control.
func staleRecall(lastValidated, timestamp string, staleAfter time.Duration, now time.Time) *Recall {
	r := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web", LastValidated: lastValidated, Timestamp: timestamp}, Score: 6.0},
	})
	r.StaleAfter = staleAfter
	r.Now = func() time.Time { return now }
	return r
}

var staleNow = time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

const thirtyDays = 30 * 24 * time.Hour

// TestRecallStaleDownWeights: an entry whose last_validated predates StaleAfter is
// still fired (age never rejects), but its confidence is a single 0.75 step below
// the same lookup with StaleAfter disabled.
func TestRecallStaleDownWeights(t *testing.T) {
	stale := staleRecall("2024-07-20", "", thirtyDays, staleNow) // ~2y old > 30d
	ctrl := staleRecall("2024-07-20", "", 0, staleNow)           // age off
	eS, confS := stale.lookup(context.Background(), okReq())
	eC, confC := ctrl.lookup(context.Background(), okReq())
	if eS == nil || eC == nil {
		t.Fatal("a stale entry must still FIRE — age never rejects on its own")
	}
	if math.Abs(confS-confC*staleFactor) > 1e-9 {
		t.Fatalf("stale confidence = %v, want control*%v = %v", confS, staleFactor, confC*staleFactor)
	}
}

// TestRecallStaleFallsBackToTimestamp: with no last_validated, freshness comes from
// the entry timestamp.
func TestRecallStaleFallsBackToTimestamp(t *testing.T) {
	stale := staleRecall("", "2024-07-20T00:00:00Z", thirtyDays, staleNow)
	ctrl := staleRecall("", "2024-07-20T00:00:00Z", 0, staleNow)
	_, confS := stale.lookup(context.Background(), okReq())
	_, confC := ctrl.lookup(context.Background(), okReq())
	if math.Abs(confS-confC*staleFactor) > 1e-9 {
		t.Fatalf("timestamp-fallback stale confidence = %v, want %v", confS, confC*staleFactor)
	}
}

// TestRecallFreshWithinHorizonUntouched: a recently-validated entry is not
// down-weighted.
func TestRecallFreshWithinHorizonUntouched(t *testing.T) {
	fresh := staleRecall("2026-07-10", "", thirtyDays, staleNow) // 10d < 30d
	ctrl := staleRecall("2026-07-10", "", 0, staleNow)
	_, confS := fresh.lookup(context.Background(), okReq())
	_, confC := ctrl.lookup(context.Background(), okReq())
	if confS != confC {
		t.Fatalf("a fresh entry must not be down-weighted: %v != %v", confS, confC)
	}
}

// TestRecallStaleNoDatesUntouched: a dateless entry is exempt (fail-safe = absent
// fields reproduce pre-staleness behavior).
func TestRecallStaleNoDatesUntouched(t *testing.T) {
	none := staleRecall("", "", thirtyDays, staleNow)
	ctrl := staleRecall("", "", 0, staleNow)
	_, confS := none.lookup(context.Background(), okReq())
	_, confC := ctrl.lookup(context.Background(), okReq())
	if confS != confC {
		t.Fatalf("a dateless entry must not be down-weighted: %v != %v", confS, confC)
	}
}

// TestRecallStaleUnparseableDateExempt: an unparseable freshness date is treated as
// no age signal (never an error), so the entry is not down-weighted.
func TestRecallStaleUnparseableDateExempt(t *testing.T) {
	bad := staleRecall("not-a-date", "", thirtyDays, staleNow)
	ctrl := staleRecall("not-a-date", "", 0, staleNow)
	_, confS := bad.lookup(context.Background(), okReq())
	_, confC := ctrl.lookup(context.Background(), okReq())
	if confS != confC {
		t.Fatalf("an unparseable date must be exempt: %v != %v", confS, confC)
	}
}

// TestRecallStaleNilClockUsesNow: an unset Now falls back to the real clock. A
// last_validated far in the past with a tiny horizon is down-weighted relative to
// the StaleAfter:0 control.
func TestRecallStaleNilClockUsesNow(t *testing.T) {
	stale := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web", LastValidated: "2000-01-01"}, Score: 6.0},
	})
	stale.StaleAfter = time.Hour // nil Now ⇒ time.Now
	ctrl := recallWith([]catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "OOM", Path: "a.md", Resource: "apps/web", LastValidated: "2000-01-01"}, Score: 6.0},
	})
	eS, confS := stale.lookup(context.Background(), okReq())
	_, confC := ctrl.lookup(context.Background(), okReq())
	if eS == nil {
		t.Fatal("a stale entry must still fire under the real clock")
	}
	if confS >= confC {
		t.Fatalf("real-clock stale confidence %v must be below the un-aged %v", confS, confC)
	}
}
