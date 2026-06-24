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
// outcome ledger holds a matching resolved episode. The join is keyed on the
// deterministic dedup fingerprint (resource+cause) carried in the PR body and stamped
// on every ledger open — stable across the LLM's prose, so a reworded re-investigation
// of one incident still resolves the matching PR. A PR filed before the fingerprint
// was wired carries no marker; it falls back to a whitespace-normalized title join.
// Source-agnostic: it reads the ledger's episodes, never a trigger-specific API.
type LedgerResolutionChecker struct {
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
}

// IsResolved implements ResolutionChecker.
func (c LedgerResolutionChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
	wantFP := providers.ParseFingerprintMarker(pr.Body) // "" when the PR carries no marker
	title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
	if wantFP == "" && title == "" {
		return false, nil
	}
	eps, err := c.Ledger.Episodes()
	if err != nil {
		return false, err
	}
	for _, e := range eps {
		if !e.Resolved {
			continue
		}
		// Primary: the stable dedup-fingerprint join. An episode with an empty
		// fingerprint never equals a non-empty wantFP, so empty never false-matches.
		if wantFP != "" {
			if e.DupFingerprint == wantFP {
				return true, nil
			}
			// A marker is present: stay fingerprint-only. Falling through to the title
			// here would resurrect the very free-text fragility this join removes.
			continue
		}
		// Legacy fallback (no marker): whitespace-robust title equality on both sides.
		if title != "" && strings.TrimSpace(e.Title) == title {
			return true, nil
		}
	}
	return false, nil
}
