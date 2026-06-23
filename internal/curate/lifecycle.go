package curate

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// protectedLabels are never auto-closed (by the stale sweep OR dedup). "solved"
// is protected too: it marks a PR a human reviewed for content, not yet confirmed
// for merge — auto-closing it would discard that editorial work.
var protectedLabels = []string{"solved", "ready-to-merge", "accepted", "investigating", "knowledge-gap"}

// Lifecycle closes stale, unprotected KB artifacts — those with no forge activity
// within StaleAfter. A PR whose age is unknown (zero UpdatedAt) is never closed.
type Lifecycle struct {
	Forge      Forge
	StaleAfter time.Duration    // 0 disables the sweep
	Now        func() time.Time // injectable clock; nil ⇒ time.Now
	Log        *slog.Logger
}

// Run closes stale, unprotected artifacts with a comment.
func (l Lifecycle) Run(ctx context.Context) error {
	if l.StaleAfter <= 0 {
		return nil
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	prs, err := l.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if isProtected(pr.Labels) || pr.UpdatedAt.IsZero() || now().Sub(pr.UpdatedAt) <= l.StaleAfter {
			continue
		}
		// Comment first; if the back-ref comment fails, do NOT close (preserve the
		// "why" for whoever reopens it) — mirrors Dedup.
		if err := l.Forge.Comment(ctx, pr.Number, "Closed as stale by RunLore curate (no progress in the staleness window). Reopen if still relevant."); err != nil {
			l.Log.Warn("stale: comment failed; not closing", "pr", pr.Number, "err", err)
			continue
		}
		if err := l.Forge.Close(ctx, pr.Number); err != nil {
			l.Log.Warn("stale close failed", "pr", pr.Number, "err", err)
			continue
		}
		l.Log.Info("closed stale artifact", "pr", pr.Number)
	}
	return nil
}

func isProtected(labels []string) bool {
	for _, p := range protectedLabels {
		if slices.Contains(labels, p) {
			return true
		}
	}
	return false
}
