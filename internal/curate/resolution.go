package curate

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// ResolutionChecker reports whether the incident behind a curated PR has cleared
// (alert resolved / resource healthy). Implemented read-only over the cluster.
type ResolutionChecker interface {
	IsResolved(ctx context.Context, pr providers.CuratedIssue) (bool, error)
}

// Queue applies the merge condition: a quality-passing (solved) PR becomes
// ready-to-merge when the incident is resolved, OR when a human has labelled it
// accepted. Unresolved + unaccepted PRs wait (surfaced, never auto-queued).
type Queue struct {
	Forge   Forge
	Checker ResolutionChecker
	Log     *slog.Logger
}

// Run gates each solved PR onto the decision-ready queue.
func (q Queue) Run(ctx context.Context) error {
	prs, err := q.Forge.ListPRsByLabel(ctx, "solved")
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if slices.Contains(pr.Labels, "ready-to-merge") {
			continue // already queued
		}
		accepted := slices.Contains(pr.Labels, "accepted")
		resolved := false
		if !accepted {
			resolved, err = q.Checker.IsResolved(ctx, pr)
			if err != nil {
				q.Log.Warn("resolution check failed", "pr", pr.Number, "err", err)
				continue
			}
		}
		if accepted || resolved {
			if err := q.Forge.ReplaceLabel(ctx, pr.Number, "", "ready-to-merge"); err != nil {
				q.Log.Warn("queue: relabel failed", "pr", pr.Number, "err", err)
				continue
			}
			q.Log.Info("queued ready-to-merge", "pr", pr.Number, "reason", queueReason(accepted))
		}
	}
	return nil
}

func queueReason(accepted bool) string {
	if accepted {
		return "human-accepted"
	}
	return "resolved"
}

// LedgerResolutionChecker reports a curated PR's incident has resolved when the
// outcome ledger holds a resolved episode whose title matches the PR's. A curated
// PR's title is "KB: " + the incident title, and the ledger records each open with
// that same incident title — so the join is an exact title match. Source-agnostic:
// it reads the ledger's resolve events, never a trigger-specific API.
type LedgerResolutionChecker struct {
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
}

// IsResolved implements ResolutionChecker.
func (c LedgerResolutionChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
	title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
	if title == "" {
		return false, nil
	}
	eps, err := c.Ledger.Episodes()
	if err != nil {
		return false, err
	}
	for _, e := range eps {
		if e.Resolved && e.Title == title {
			return true, nil
		}
	}
	return false, nil
}
