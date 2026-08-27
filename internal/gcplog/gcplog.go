// SPDX-License-Identifier: Apache-2.0

// Package gcplog is the Cloud Logging entries.list paging contract, shared by every
// provider in this repo that reads a Google log stream.
//
// It exists because there were two independent copies of this loop — the GCP firewall
// network provider and the GCP cloud audit lens — agreeing on the parts that are easy
// and disagreeing on the parts that are not. Both built the same window clauses, the
// same request literal and the same page-until-done skeleton; they differed on how a
// binding cap is detected, and only one of them recorded WHY the timestamp format has
// to be RFC3339Nano. One copy also had no page budget at all, which is the failure this
// package's Walk exists to make impossible.
package gcplog

import (
	"context"
	"fmt"
	"time"

	logging "google.golang.org/api/logging/v2"

	"github.com/Smana/runlore/internal/providers"
)

// EntriesAPI is the one Cloud Logging method these providers use.
//
// An interface rather than *logging.Service because the generated call is a builder
// chain (Entries.List(req).Context(ctx).Do()) that no fake can satisfy, and because the
// filter string — the part of a log query that is easy to get wrong and impossible to
// see from the outside — is only assertable if a test can capture the request.
type EntriesAPI interface {
	List(ctx context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error)
}

// Entries adapts a generated *logging.Service to EntriesAPI. The generated call is a
// builder chain that no fake can satisfy; this collapses it to one ordinary method.
func Entries(svc *logging.Service) EntriesAPI { return serviceEntries{svc: svc} }

type serviceEntries struct{ svc *logging.Service }

func (l serviceEntries) List(ctx context.Context, req *logging.ListLogEntriesRequest) (*logging.ListLogEntriesResponse, error) {
	return l.svc.Entries.List(req).Context(ctx).Do()
}

// DefaultMaxPages bounds how many pages one Walk may read.
//
// A budget is required, not a nicety. Both a server-side filter that over-matches and a
// caller whose visit function rejects everything produce the same shape: pages that
// arrive full and contribute nothing, with a NextPageToken every time. Without a
// ceiling the loop then follows that token across the entire window — hundreds of
// sequential round-trips inside one tool call, until the investigation's context
// deadline kills it and the lens returns nothing at all. Ten pages is enough to fill
// any cap this repo sets from a normally-selective filter, and small enough that the
// pathological case costs a bounded delay instead of the whole call.
const DefaultMaxPages = 10

// WindowFilter appends the timestamp clauses for w to a base filter.
//
// RFC3339Nano rather than RFC3339, and this is the one line in the paging contract with
// a real trap in it: a window's End is typically a time.Now() carrying sub-second
// precision, and formatting it without the fractional part truncates DOWNWARD, moving
// the boundary backwards over exactly the newest entries the cap exists to keep. A
// zero Start or End is left unbounded on that side.
func WindowFilter(base string, w providers.TimeWindow) string {
	if !w.Start.IsZero() {
		base += fmt.Sprintf(` AND timestamp>="%s"`, w.Start.Format(time.RFC3339Nano))
	}
	if !w.End.IsZero() {
		base += fmt.Sprintf(` AND timestamp<="%s"`, w.End.Format(time.RFC3339Nano))
	}
	return base
}

// EntryTime parses a log entry's timestamp, returning the zero time when it is absent
// or unparseable. Callers render the raw string alongside, so a format this cannot read
// degrades the ordering of one row rather than dropping it.
func EntryTime(ts string) time.Time {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Query describes one paged read.
type Query struct {
	// Project is the project id; the request is scoped to "projects/<id>".
	Project string
	// Filter is the complete Cloud Logging filter, window clauses included.
	Filter string
	// Cap is how many KEPT entries the caller wants. Walk asks the server for one more
	// than this, so a single page can settle whether the cap is BINDING or the result is
	// merely exactly full even on a stream that returns no page token.
	Cap int64
	// MaxPages bounds the read; 0 means DefaultMaxPages.
	MaxPages int
}

// Result reports how a Walk ended. The three stop reasons are distinguishable because
// they mean different things to a model: a bound cap says "narrow the search", an
// exhausted stream says "this is everything", and a spent page budget says "the filter
// matched far more than it kept" — which is a statement about the QUERY, not the window.
type Result struct {
	// Kept counts entries visit accepted.
	Kept int64
	// Pages counts round-trips made.
	Pages int
	// CapBound is true when collection stopped because more than Cap entries were kept.
	CapBound bool
	// PageBudgetSpent is true when MaxPages bound the read with the stream still going.
	PageBudgetSpent bool
}

// Walk pages entries.list, calling visit for each non-nil entry. visit reports whether
// it KEPT the entry, which is what counts against Cap — an entry the caller filters out
// client-side must not consume the budget, or a selective filter would report itself as
// truncated on the first page.
//
// It stops at the first of: more than Cap kept, the stream exhausted, or the page
// budget spent. The last of those is why this function exists rather than each caller
// writing the loop; see DefaultMaxPages.
func Walk(ctx context.Context, api EntriesAPI, q Query, visit func(*logging.LogEntry) bool) (Result, error) {
	maxPages := q.MaxPages
	if maxPages <= 0 {
		maxPages = DefaultMaxPages
	}
	var res Result
	token := ""
	for {
		resp, err := api.List(ctx, &logging.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + q.Project},
			Filter:        q.Filter,
			OrderBy:       "timestamp desc",
			PageSize:      q.Cap + 1,
			PageToken:     token,
		})
		if err != nil {
			return res, err
		}
		res.Pages++
		for _, e := range resp.Entries {
			if e == nil {
				continue
			}
			if visit(e) {
				res.Kept++
			}
		}
		if res.Kept > q.Cap {
			res.CapBound = true
			return res, nil
		}
		if resp.NextPageToken == "" {
			// The stream ended. Kept == Cap here means the result is EXACTLY full, not
			// truncated — a distinction worth keeping, since a spurious "narrow your
			// search" note teaches the model to distrust a complete answer.
			return res, nil
		}
		if res.Kept >= q.Cap {
			// Cap filled AND the server says there is more: that settles it without
			// another round-trip. Fetching one more page to confirm what the token
			// already stated is a wasted call on the busiest windows, which are exactly
			// the ones that reach this branch.
			res.CapBound = true
			return res, nil
		}
		if res.Pages >= maxPages {
			res.PageBudgetSpent = true
			return res, nil
		}
		token = resp.NextPageToken
	}
}
