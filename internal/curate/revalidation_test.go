// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

// confirmedAt is the resolve time every healthy fixture below carries.
var confirmedAt = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// fakeRevalidateForge records every call so tests can assert exactly which entries
// were proposed (and that empty candidate sets make zero forge calls).
type fakeRevalidateForge struct {
	open, closed       []providers.CuratedIssue
	openErr, closedErr error
	prErr              map[string]error // per-path OpenRevalidatePR error injection

	listOpen, listClosed int
	proposed             []string             // entryPaths passed to OpenRevalidatePR, in call order
	dates                map[string]time.Time // entryPath -> stamped date
	gaps                 map[string]time.Duration
	bodies               map[string]string
}

func (f *fakeRevalidateForge) ListPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	f.listOpen++
	return f.open, f.openErr
}

func (f *fakeRevalidateForge) ListClosedUnmergedPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	f.listClosed++
	return f.closed, f.closedErr
}

func (f *fakeRevalidateForge) OpenRevalidatePR(_ context.Context, entryPath string, validated time.Time, minGap time.Duration, body string) (providers.Ref, error) {
	f.proposed = append(f.proposed, entryPath)
	if f.dates == nil {
		f.dates, f.gaps, f.bodies = map[string]time.Time{}, map[string]time.Duration{}, map[string]string{}
	}
	f.dates[entryPath], f.gaps[entryPath], f.bodies[entryPath] = validated, minGap, body
	if err := f.prErr[entryPath]; err != nil {
		return providers.Ref{}, err
	}
	return providers.Ref{URL: "https://github.com/o/r/pull/1"}, nil
}

// *github.Client must satisfy the consumer-side RevalidateForge interface.
var _ RevalidateForge = (*github.Client)(nil)

func newRevalidation(forge RevalidateForge, stats EntryStats) Revalidation {
	return Revalidation{
		Forge:       forge,
		Stats:       stats,
		MinInterval: 720 * time.Hour,
		MaxOpen:     5,
		Floor:       0.5,
		Prior:       2.0,
		Log:         quietLogger(),
	}
}

func TestRevalidation(t *testing.T) {
	t.Run("proposes only confirmed, still-trusted entries, in sorted order", func(t *testing.T) {
		stats := mapStats{
			// factor 0.83, one resolved recall on record -> propose
			"incidents/works.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
			// factor 0.67 (>= floor) with a resolve -> propose
			"incidents/mixed.md": {Recalls: 3, Resolved: 2, FeedbackUp: 1, LastConfirmed: confirmedAt},
			// never resolved: 👍 votes alone are not the evidence this pass acts on
			"incidents/votes-only.md": {Recalls: 1, FeedbackUp: 3},
			// factor 0.20 -> a RETIREMENT candidate, never a revalidation one
			"incidents/decayed.md": {Recalls: 5, Resolved: 1, LastConfirmed: confirmedAt},
			// no recall history at all
			"incidents/never-recalled.md": {},
		}
		forge := &fakeRevalidateForge{}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := []string{"incidents/mixed.md", "incidents/works.md"}
		if !slices.Equal(forge.proposed, want) {
			t.Fatalf("proposed=%v, want %v", forge.proposed, want)
		}
		// The date stamped is the resolve time — the evidence's own date, not "now".
		if got := forge.dates["incidents/works.md"]; !got.Equal(confirmedAt) {
			t.Errorf("stamped date=%v, want the ledger's LastConfirmed %v", got, confirmedAt)
		}
		if got := forge.gaps["incidents/works.md"]; got != 720*time.Hour {
			t.Errorf("minGap=%v, want the configured MinInterval", got)
		}
		body := forge.bodies["incidents/works.md"]
		for _, want := range []string{
			revalidateMarker("incidents/works.md"), // idempotency + veto record
			"2026-08-03",                           // the date under review
			"0.83",                                 // the track record behind it
			"Close this PR",                        // the veto is spelled out
		} {
			if !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("an entry below the floor is left to retirement", func(t *testing.T) {
		// Disjointness is the contract: retirement fires strictly below the floor,
		// revalidation at or above it, so one sweep can never propose both.
		stats := mapStats{"incidents/decayed.md": {Recalls: 5, Resolved: 1, LastConfirmed: confirmedAt}}
		forge := &fakeRevalidateForge{}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("a below-floor entry must never be revalidated, got %v", forge.proposed)
		}
	})

	t.Run("idempotent: an open revalidate PR with the marker is skipped", func(t *testing.T) {
		path := "incidents/works.md"
		stats := mapStats{path: {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt}}
		forge := &fakeRevalidateForge{
			open: []providers.CuratedIssue{{Number: 7, Body: "proposal\n" + revalidateMarker(path)}},
		}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("expected no new PR, got %v", forge.proposed)
		}
		if forge.listOpen != 1 {
			t.Fatalf("ListPRsByLabel called %d times, want exactly 1 per run", forge.listOpen)
		}
	})

	t.Run("human veto: a closed-unmerged revalidate PR is never re-nagged", func(t *testing.T) {
		path := "incidents/works.md"
		stats := mapStats{path: {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt}}
		forge := &fakeRevalidateForge{
			closed: []providers.CuratedIssue{{Number: 9, Body: "declined\n" + revalidateMarker(path)}},
		}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("human veto ignored: proposed %v", forge.proposed)
		}
	})

	t.Run("forge done-skips do not consume the open-PR budget", func(t *testing.T) {
		stats := mapStats{
			"incidents/a.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
			"incidents/b.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
		}
		forge := &fakeRevalidateForge{prErr: map[string]error{"incidents/a.md": github.ErrRecentlyValidated}}
		p := newRevalidation(forge, stats)
		p.MaxOpen = 1 // budget for exactly one NEW PR
		if err := p.Run(context.Background()); err != nil {
			t.Fatalf("Run should not surface ErrRecentlyValidated: %v", err)
		}
		if !slices.Equal(forge.proposed, []string{"incidents/a.md", "incidents/b.md"}) {
			t.Fatalf("a skipped entry must not spend the budget, got %v", forge.proposed)
		}
	})

	t.Run("an inactive entry is a done-skip, the rest still run", func(t *testing.T) {
		stats := mapStats{
			"incidents/a.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
			"incidents/b.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
		}
		forge := &fakeRevalidateForge{prErr: map[string]error{"incidents/a.md": github.ErrEntryInactive}}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run should not surface ErrEntryInactive: %v", err)
		}
		if !slices.Equal(forge.proposed, []string{"incidents/a.md", "incidents/b.md"}) {
			t.Fatalf("both entries must be attempted, got %v", forge.proposed)
		}
	})

	t.Run("per-item error isolation: one flaky entry never starves the rest", func(t *testing.T) {
		stats := mapStats{
			"incidents/a.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
			"incidents/b.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
		}
		forge := &fakeRevalidateForge{prErr: map[string]error{"incidents/a.md": errors.New("forge boom")}}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("a per-item error must not fail the run: %v", err)
		}
		if !slices.Equal(forge.proposed, []string{"incidents/a.md", "incidents/b.md"}) {
			t.Fatalf("both entries must be attempted, got %v", forge.proposed)
		}
	})

	t.Run("empty candidate set makes zero forge calls", func(t *testing.T) {
		stats := mapStats{"incidents/never-resolved.md": {Recalls: 1}}
		forge := &fakeRevalidateForge{}
		if err := newRevalidation(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if forge.listOpen != 0 || forge.listClosed != 0 || len(forge.proposed) != 0 {
			t.Fatalf("no candidates must mean zero forge calls: listOpen=%d listClosed=%d proposed=%v",
				forge.listOpen, forge.listClosed, forge.proposed)
		}
	})

	t.Run("a listing error fails the run rather than proposing blind", func(t *testing.T) {
		stats := mapStats{"incidents/works.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt}}
		forge := &fakeRevalidateForge{closedErr: errors.New("forge 502")}
		if err := newRevalidation(forge, stats).Run(context.Background()); err == nil {
			t.Fatal("want an error when the veto listing is unavailable")
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("no PR may be opened without the veto listing, got %v", forge.proposed)
		}
	})
}

// TestRevalidationOpenPRBudget pins the reviewer-queue bound. Enabling the pass on
// a mature catalog can surface dozens of long-confirmed entries at once; without a
// cap the first sweep would open a PR for every one of them.
func TestRevalidationOpenPRBudget(t *testing.T) {
	stats := mapStats{
		"incidents/a.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
		"incidents/b.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
		"incidents/c.md": {Recalls: 4, Resolved: 4, LastConfirmed: confirmedAt},
	}

	t.Run("caps new PRs at MaxOpen", func(t *testing.T) {
		forge := &fakeRevalidateForge{}
		p := newRevalidation(forge, stats)
		p.MaxOpen = 2
		if err := p.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !slices.Equal(forge.proposed, []string{"incidents/a.md", "incidents/b.md"}) {
			t.Fatalf("proposed=%v, want the first two candidates only", forge.proposed)
		}
	})

	t.Run("an unset MaxOpen falls back to the default instead of proposing nothing", func(t *testing.T) {
		// A zero bound computes a budget of zero, which would silently make the
		// whole pass a no-op for a direct (non-config) user of the package.
		forge := &fakeRevalidateForge{}
		p := newRevalidation(forge, stats)
		p.MaxOpen = 0
		if err := p.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 3 {
			t.Fatalf("proposed=%v, want all three under the default bound of %d",
				forge.proposed, DefaultMaxOpenRevalidations)
		}
	})

	t.Run("already-outstanding PRs count against the budget", func(t *testing.T) {
		// Two unrelated revalidate PRs are already waiting on a human: the queue is
		// full, so this run adds nothing however many candidates it found.
		forge := &fakeRevalidateForge{open: []providers.CuratedIssue{{Number: 1}, {Number: 2}}}
		p := newRevalidation(forge, stats)
		p.MaxOpen = 2
		if err := p.Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("a full review queue must open nothing, got %v", forge.proposed)
		}
	})
}
