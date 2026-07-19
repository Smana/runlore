// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRetireForge records every call so tests can assert exactly which entries were
// proposed for retirement (and that empty candidate sets make zero calls).
type fakeRetireForge struct {
	open, closed       []providers.CuratedIssue
	openErr, closedErr error
	retireErr          map[string]error // per-path OpenRetirePR error injection

	listOpen, listClosed int
	proposed             []string          // entryPaths passed to OpenRetirePR, in call order
	bodies               map[string]string // entryPath -> body
}

func (f *fakeRetireForge) ListPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	f.listOpen++
	return f.open, f.openErr
}

func (f *fakeRetireForge) ListClosedUnmergedPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	f.listClosed++
	return f.closed, f.closedErr
}

func (f *fakeRetireForge) OpenRetirePR(_ context.Context, entryPath, body string) (providers.Ref, error) {
	f.proposed = append(f.proposed, entryPath)
	if f.bodies == nil {
		f.bodies = map[string]string{}
	}
	f.bodies[entryPath] = body
	if err := f.retireErr[entryPath]; err != nil {
		return providers.Ref{}, err
	}
	return providers.Ref{URL: "https://github.com/o/r/pull/1"}, nil
}

type mapStats map[string]outcome.Aggregate

func (m mapStats) OpenCounts() (map[string]outcome.Aggregate, error) { return m, nil }

// *github.Client must satisfy the consumer-side RetireForge interface.
var _ RetireForge = (*github.Client)(nil)

func newRetirement(forge RetireForge, stats RetireStats) Retirement {
	return Retirement{
		Forge:           forge,
		Stats:           stats,
		MinObservations: 3,
		Floor:           0.5,
		Prior:           2.0,
		Log:             quietLogger(),
	}
}

func TestRetirement(t *testing.T) {
	t.Run("retires only sustained-decay entries, in sorted order", func(t *testing.T) {
		stats := mapStats{
			"incidents/decayed-recalls.md": {Recalls: 3},                  // factor 0.20, obs 3 -> retire
			"incidents/decayed-votes.md":   {Recalls: 2, FeedbackDown: 1}, // factor 0.20, obs 3 -> retire
			"incidents/one-bad-recall.md":  {Recalls: 1},                  // factor 0.33 but obs 1 -> keep
			"incidents/two-unresolved.md":  {Recalls: 2},                  // factor 0.25 but obs 2 -> keep
			"incidents/healthy.md":         {Recalls: 4, Resolved: 4},     // factor 0.83 -> keep
		}
		forge := &fakeRetireForge{}
		if err := newRetirement(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		want := []string{"incidents/decayed-recalls.md", "incidents/decayed-votes.md"}
		if !slices.Equal(forge.proposed, want) {
			t.Fatalf("proposed=%v, want %v", forge.proposed, want)
		}
		// Body carries the marker, the factor, and honest reviewer-facing language.
		body := forge.bodies["incidents/decayed-recalls.md"]
		marker := retireMarker("incidents/decayed-recalls.md")
		if !strings.Contains(body, marker) {
			t.Errorf("body missing marker:\n%s", body)
		}
		if !strings.Contains(body, "0.20") {
			t.Errorf("body missing factor 0.20:\n%s", body)
		}
		if !strings.Contains(body, "merging this PR retires the entry") {
			t.Errorf("body missing veto-honest phrasing:\n%s", body)
		}
	})

	t.Run("idempotent: an open retire PR with the marker is skipped", func(t *testing.T) {
		path := "incidents/decayed-recalls.md"
		stats := mapStats{path: {Recalls: 3}}
		forge := &fakeRetireForge{
			open: []providers.CuratedIssue{{Number: 7, Body: "proposal\n" + retireMarker(path)}},
		}
		if err := newRetirement(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("expected no new PR, got %v", forge.proposed)
		}
		if forge.listOpen != 1 {
			t.Fatalf("ListPRsByLabel called %d times, want exactly 1 per run", forge.listOpen)
		}
	})

	t.Run("human veto: a closed-unmerged retire PR with the marker is never re-nagged", func(t *testing.T) {
		path := "incidents/decayed-recalls.md"
		stats := mapStats{path: {Recalls: 3}}
		forge := &fakeRetireForge{
			closed: []providers.CuratedIssue{{Number: 9, Body: "rejected\n" + retireMarker(path)}},
		}
		if err := newRetirement(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(forge.proposed) != 0 {
			t.Fatalf("human veto ignored: proposed %v", forge.proposed)
		}
	})

	t.Run("ErrAlreadyRetired is a done-skip, the rest still run", func(t *testing.T) {
		stats := mapStats{
			"incidents/a.md": {Recalls: 3},
			"incidents/b.md": {Recalls: 3},
		}
		forge := &fakeRetireForge{retireErr: map[string]error{"incidents/a.md": github.ErrAlreadyRetired}}
		if err := newRetirement(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run should not surface ErrAlreadyRetired: %v", err)
		}
		if !slices.Equal(forge.proposed, []string{"incidents/a.md", "incidents/b.md"}) {
			t.Fatalf("both entries must be attempted, got %v", forge.proposed)
		}
	})

	t.Run("per-item error isolation: one flaky entry never starves the rest", func(t *testing.T) {
		stats := mapStats{
			"incidents/a.md": {Recalls: 3},
			"incidents/b.md": {Recalls: 3},
		}
		forge := &fakeRetireForge{retireErr: map[string]error{"incidents/a.md": errors.New("forge boom")}}
		if err := newRetirement(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("a per-item error must not fail the run: %v", err)
		}
		if !slices.Equal(forge.proposed, []string{"incidents/a.md", "incidents/b.md"}) {
			t.Fatalf("both entries must be attempted, got %v", forge.proposed)
		}
	})

	t.Run("empty candidate set makes zero forge calls", func(t *testing.T) {
		stats := mapStats{"incidents/healthy.md": {Recalls: 4, Resolved: 4}}
		forge := &fakeRetireForge{}
		if err := newRetirement(forge, stats).Run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if forge.listOpen != 0 || forge.listClosed != 0 || len(forge.proposed) != 0 {
			t.Fatalf("no candidates must mean zero forge calls: listOpen=%d listClosed=%d proposed=%v",
				forge.listOpen, forge.listClosed, forge.proposed)
		}
	})
}
