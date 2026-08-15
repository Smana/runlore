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

// isAutoCloseExempt reports whether a PR must never be auto-closed by Dedup.
// It does NOT govern the stale sweep — see Lifecycle.Run, which exempts only
// isEntryEditProposal, not isOperatorNote. The two exemptions look identical
// (both cover entryEditLabels and operatorNoteLabel) but rest on different
// rationale for each label, worth stating precisely rather than conflating,
// since conflating them is exactly what led an earlier version of this
// comment astray:
//
//   - entryEditLabels (retire/revalidate proposals): the veto hazard is real
//     here. Revalidation and Retirement keep no store — they reconstruct an
//     entry's history from the forge on every run, with no memory of WHO
//     closed a prior proposal, so a close by RunLore's own housekeeping is
//     byte-for-byte the same signal as a human's "no" and is read back as a
//     permanent veto for a decision nobody made. This is why entry-edit
//     proposals are exempt from BOTH Dedup and the stale sweep.
//
//   - operatorNoteLabel (standalone notes filed via thread capture): the veto
//     hazard does NOT apply here, and the mechanism that would make it apply
//     does not read a note's close back as anything. ClosedPRSuppression
//     skips every markerless PR (suppression.go, the `fp == "" → continue`
//     branch), and thread.ConceptEntry deliberately leaves Fingerprint unset
//     precisely so a note never collides with a curated finding — so an
//     auto-closed note is never suppressed, never escalated, never read back
//     as a veto by anything in this codebase. Operator notes stay exempt from
//     DEDUP for a narrower, genuine reason instead: ConceptEntry always
//     titles a note "KB: Operator note: <finding title>", so two notes on the
//     same recurring incident score ~1.0 on the title-Jaccard fallback (see
//     TestDedupNeverClosesAnOperatorNote) — closing one as a "duplicate" of
//     the other would discard a human's contribution outright. They are NOT
//     exempt from the stale sweep: closing an untouched note is ordinary
//     housekeeping, not discarding one, provided the close comment says so
//     and invites reopening — see staleOperatorNoteComment.
func isAutoCloseExempt(labels []string) bool {
	return isEntryEditProposal(labels) || isOperatorNote(labels)
}

// staleComment is posted on an ordinary stale KB draft before the sweep
// closes it.
const staleComment = "Closed as stale by RunLore curate (no progress in the staleness window). Reopen if still relevant."

// staleOperatorNoteComment is posted on a stale operator note before the
// sweep closes it — deliberately distinct wording from staleComment (see
// isAutoCloseExempt's doc comment): this close is routine housekeeping, not
// a rejection of the human's contribution, and the knowledge is not
// discarded — reopening restores it for review.
const staleOperatorNoteComment = "Closed as stale by RunLore curate — this operator note saw no activity within " +
	"the staleness window. This is routine housekeeping, not a rejection of your note: nothing about it is discarded, " +
	"and reopening this pull request restores it for review."

// Lifecycle closes stale, unprotected KB artifacts — those with no forge activity
// within StaleAfter. A PR whose age is unknown (zero UpdatedAt) is never closed.
type Lifecycle struct {
	Forge      Forge
	StaleAfter time.Duration    // 0 disables the sweep
	Now        func() time.Time // injectable clock; nil ⇒ time.Now
	Log        *slog.Logger
}

// Run closes stale, unprotected artifacts with a comment. Entry-edit
// proposals (isEntryEditProposal) are exempt — see isAutoCloseExempt's doc
// comment for why. Operator notes are NOT exempt here, unlike from Dedup:
// an untouched note past StaleAfter is closed too, with its own comment
// (staleOperatorNoteComment) that says so plainly and invites reopening.
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
		if isEntryEditProposal(pr.Labels) {
			continue // only a human may close one of these — see entryEditLabels
		}
		if isProtected(pr.Labels) || pr.UpdatedAt.IsZero() || now().Sub(pr.UpdatedAt) <= l.StaleAfter {
			continue
		}
		comment := staleComment
		if isOperatorNote(pr.Labels) {
			comment = staleOperatorNoteComment
		}
		// Comment first; if the back-ref comment fails, do NOT close (preserve the
		// "why" for whoever reopens it) — mirrors Dedup.
		if err := l.Forge.Comment(ctx, pr.Number, comment); err != nil {
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
