package curate

import (
	"context"
	"log/slog"
	"slices"

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
