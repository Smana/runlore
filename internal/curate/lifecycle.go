// SPDX-License-Identifier: Apache-2.0

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

// entryEditLabels mark the lifecycle proposals the decay-driven passes open —
// Retirement's retire PRs and Revalidation's revalidate PRs. RunLore must never
// close one itself, by ANY pass.
//
// Those passes keep no store: an entry's history is reconstructed from the forge
// on every run, where a still-open proposal means "already asked" and a
// CLOSED-UNMERGED one means "a human declined — never ask again". Nothing in that
// signal distinguishes a reviewer's close from RunLore's own housekeeping, so a
// stale-sweep or dedup close is read straight back as a permanent veto, and the
// entry is never proposed again for a decision no human ever made. Excluding them
// keeps the veto meaning exactly what it claims. The cost is that an unreviewed
// proposal stays open indefinitely; Revalidation's MaxOpen is what bounds that
// queue instead, and closing one by hand is then a real veto rather than an
// accident.
var entryEditLabels = []string{retireLabel, revalidateLabel}

// isEntryEditProposal reports whether a PR is a retire/revalidate proposal about
// an EXISTING merged entry rather than a draft of a new one.
func isEntryEditProposal(labels []string) bool {
	_, ok := firstLabelIn(labels, entryEditLabels)
	return ok
}

// operatorNoteLabel marks a standalone KB PR opened from a human's thread
// reply (thread.ConceptEntry, which appends it via providers.KBEntry.
// ExtraLabels). RunLore must never auto-close one itself, by ANY pass — an
// operator note IS a human's contribution, so closing it here is the same
// class of hazard entryEditLabels guards against: RunLore's own housekeeping
// producing a close that is indistinguishable from a human veto.
//
// internal/thread depends only on providers and internal/catalog by design,
// so this literal is duplicated — not imported — from thread.noteForgeLabel.
// Kept in sync by hand.
const operatorNoteLabel = "runlore-operator-note"

// isOperatorNote reports whether a PR is a standalone note filed from a
// Slack/Matrix thread reply (see operatorNoteLabel) — never a candidate for
// auto-closing by dedup or the stale sweep.
func isOperatorNote(labels []string) bool {
	return slices.Contains(labels, operatorNoteLabel)
}

// isAutoCloseExempt reports whether a PR must never be auto-closed by ANY
// curate pass (dedup, the stale sweep): entry-edit proposals and standalone
// operator notes share the same hazard — see entryEditLabels and
// operatorNoteLabel for why each, on its own, is a permanent, unintended veto.
func isAutoCloseExempt(labels []string) bool {
	return isEntryEditProposal(labels) || isOperatorNote(labels)
}

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
		if isAutoCloseExempt(pr.Labels) {
			continue // only a human may close one of these — see entryEditLabels / operatorNoteLabel
		}
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
