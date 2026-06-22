package curate

import (
	"context"
	"log/slog"
	"slices"
)

// protectedLabels are never auto-closed (by the stale sweep OR dedup). "solved"
// is protected too: it marks a PR a human reviewed for content, not yet confirmed
// for merge — auto-closing it would discard that editorial work.
var protectedLabels = []string{"solved", "ready-to-merge", "accepted", "investigating", "knowledge-gap"}

// Lifecycle closes stale, unprotected KB artifacts (no progress within the window).
type Lifecycle struct {
	Forge Forge
	Stale func(number int) bool // true ⇒ older than the staleness window (wired with real ages in the CLI)
	Log   *slog.Logger
}

// Run closes stale, unprotected artifacts with a comment.
func (l Lifecycle) Run(ctx context.Context) error {
	prs, err := l.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if isProtected(pr.Labels) || !l.Stale(pr.Number) {
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
