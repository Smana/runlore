// SPDX-License-Identifier: Apache-2.0

package gcplog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	logging "google.golang.org/api/logging/v2"

	"github.com/Smana/runlore/internal/providers"
)

// fakeEntries serves canned pages and records every filter it was asked for.
type fakeEntries struct {
	// pages[i] is the entry count for page i; every page but the last reports a token.
	pages   []int
	calls   int
	filters []string
	err     error
}

func (f *fakeEntries) List(_ context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.filters = append(f.filters, req.Filter)
	i := f.calls
	f.calls++
	if i >= len(f.pages) {
		return &logging.ListLogEntriesResponse{}, nil
	}
	entries := make([]*logging.LogEntry, f.pages[i])
	for j := range entries {
		entries[j] = &logging.LogEntry{InsertId: "e", Timestamp: "2026-08-24T10:00:00Z"}
	}
	resp := &logging.ListLogEntriesResponse{Entries: entries}
	if i < len(f.pages)-1 {
		resp.NextPageToken = "tok"
	}
	return resp, nil
}

// TestWalkStopsWhenEveryPageIsRejected is the bug this package exists to make
// impossible.
//
// A visit function that keeps NOTHING is not hypothetical: the GCP audit lens re-checks
// each entry's own status under failed_only, because the server-side "!=" clause rests on
// a promise about how Cloud Logging treats an ABSENT field. If that promise ever bends,
// the server returns successes and the client drops every one of them — so Kept never
// grows, the cap can never bind, and a loop whose only other exit is an empty page token
// follows that token across the entire window. Hundreds of sequential round-trips inside
// one tool call, until the investigation's context deadline kills it and the lens returns
// nothing at all: being slow becomes being absent.
func TestWalkStopsWhenEveryPageIsRejected(t *testing.T) {
	// 50 full pages, each reporting another token — an effectively endless stream.
	pages := make([]int, 50)
	for i := range pages {
		pages[i] = 26
	}
	f := &fakeEntries{pages: pages}

	res, err := Walk(context.Background(), f, Query{Project: "p", Filter: "x", Cap: 25},
		func(*logging.LogEntry) bool { return false }) // keeps nothing, ever
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if f.calls > DefaultMaxPages {
		t.Errorf("Walk made %d round-trips against a stream that never satisfies the cap; "+
			"the page budget is %d", f.calls, DefaultMaxPages)
	}
	if !res.PageBudgetSpent {
		t.Error("a budget-bound read must SAY it stopped reading: reported as an ordinary " +
			"result, a page-budgeted answer reads as a complete one")
	}
	if res.CapBound {
		t.Error("nothing was kept, so the cap cannot have bound; that would tell the model to " +
			"narrow a window that is not the problem")
	}
}

// TestWalkTellsAnExactlyFullResultFromATruncatedOne pins the distinction the extra
// PageSize entry buys. A spurious "narrow your search" note teaches the model to distrust
// a complete answer.
func TestWalkTellsAnExactlyFullResultFromATruncatedOne(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pages      []int
		wantBound  bool
		wantCalls  int
		wantReason string
	}{
		{
			name: "a single short page is complete", pages: []int{3},
			wantBound: false, wantCalls: 1,
			wantReason: "fewer entries than the cap means the stream is exhausted",
		},
		{
			name: "exactly the cap with no token is complete, not truncated", pages: []int{25},
			wantBound: false, wantCalls: 1,
			wantReason: "the server said there is no more; a truncation note here would be a lie",
		},
		{
			name: "one over the cap in a single page is truncated", pages: []int{26},
			wantBound: true, wantCalls: 1,
			wantReason: "the over-collected entry settles it without a second round-trip",
		},
		{
			name: "a full page plus a token is truncated, and costs ONE call", pages: []int{25, 25},
			wantBound: true, wantCalls: 1,
			wantReason: "the token already says there is more; fetching again to confirm it is waste",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEntries{pages: tc.pages}
			res, err := Walk(context.Background(), f, Query{Project: "p", Filter: "x", Cap: 25},
				func(*logging.LogEntry) bool { return true })
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if res.CapBound != tc.wantBound {
				t.Errorf("CapBound = %v, want %v — %s", res.CapBound, tc.wantBound, tc.wantReason)
			}
			if f.calls != tc.wantCalls {
				t.Errorf("made %d round-trips, want %d — %s", f.calls, tc.wantCalls, tc.wantReason)
			}
		})
	}
}

// TestWalkDoesNotCountRejectedEntriesAgainstTheCap: an entry the caller filters out
// client-side must not consume the budget, or a selective query reports itself truncated
// on its first page while having shown the model almost nothing.
func TestWalkDoesNotCountRejectedEntriesAgainstTheCap(t *testing.T) {
	f := &fakeEntries{pages: []int{26, 4}}
	kept := 0
	res, err := Walk(context.Background(), f, Query{Project: "p", Filter: "x", Cap: 25},
		func(*logging.LogEntry) bool {
			// Keep one in ten, the shape of a sparse failed_only match.
			kept++
			return kept%10 == 0
		})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.CapBound {
		t.Errorf("a sparse filter reported itself as cap-bound after keeping %d of %d entries",
			res.Kept, kept)
	}
	if res.Kept != 3 {
		t.Errorf("Kept = %d, want 3 (one in ten of 30 visited)", res.Kept)
	}
}

// TestWalkPropagatesTheAPIError: a failed page is a failed query. Swallowing it would
// render an auth or quota failure as a quiet window, which is the one answer a health
// lens must never invent.
func TestWalkPropagatesTheAPIError(t *testing.T) {
	sentinel := errors.New("boom")
	_, err := Walk(context.Background(), &fakeEntries{err: sentinel}, Query{Project: "p", Cap: 25},
		func(*logging.LogEntry) bool { return true })
	if !errors.Is(err, sentinel) {
		t.Errorf("Walk swallowed the API error: %v", err)
	}
}

// TestWindowFilterKeepsSubSecondPrecision pins the trap in the one line of this contract
// that has one. A window's End is typically a time.Now() carrying sub-second precision,
// and formatting it without the fractional part truncates DOWNWARD — moving the boundary
// backwards over exactly the newest entries the cap exists to keep.
func TestWindowFilterKeepsSubSecondPrecision(t *testing.T) {
	end := time.Date(2026, 8, 24, 10, 0, 0, 123456789, time.UTC)
	got := WindowFilter("base", providers.TimeWindow{End: end})
	if !strings.Contains(got, ".123456789") {
		t.Errorf("the window boundary lost its sub-second precision, so the newest entries fall "+
			"outside it:\n%s", got)
	}
	if !strings.Contains(got, `timestamp<="`) {
		t.Errorf("no upper-bound clause was emitted:\n%s", got)
	}
}

// TestWindowFilterLeavesAZeroBoundUnbounded: a zero Start or End means "no bound on that
// side", not "the epoch".
func TestWindowFilterLeavesAZeroBoundUnbounded(t *testing.T) {
	got := WindowFilter("base", providers.TimeWindow{})
	if got != "base" {
		t.Errorf("an empty window added clauses: %s", got)
	}
}

// TestEntryTimeDegradesRatherThanFailing: callers print the raw timestamp beside the
// parsed one, so an unreadable format must cost the ordering of one row, not the row.
func TestEntryTimeDegradesRatherThanFailing(t *testing.T) {
	if got := EntryTime("not-a-time"); !got.IsZero() {
		t.Errorf("EntryTime(%q) = %v, want the zero time", "not-a-time", got)
	}
	if got := EntryTime("2026-08-24T10:00:00Z"); got.IsZero() {
		t.Error("EntryTime rejected a valid RFC3339 timestamp")
	}
}
