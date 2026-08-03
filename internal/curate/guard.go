// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

// GuardedForge is the union of every read and write the grooming passes perform —
// the surface Guard wraps. Composed from the pass role interfaces so it stays in
// sync with them by construction (RetireForge adds ListClosedUnmergedPRsByLabel +
// OpenRetirePR; RevalidateForge adds OpenRevalidatePR; ContestedForge adds
// ListIssueCommentBodies + IsPROpen; overlapping methods across the embedded sets
// are legal since Go 1.14). *github.Client satisfies it (pinned in internal/app).
type GuardedForge interface {
	Forge
	RetireForge
	RevalidateForge
	ContestedForge
}

// Guard is the sweep-safety seam around the forge: reads pass through untouched;
// every write is recorded in the audit chain and, in dry-run, logged instead of
// executed. One wrapper gives every pass dry-run + audit without touching the
// passes themselves — the KB mirror of action.NewAuditedExecutor.
type Guard struct {
	Inner  GuardedForge
	DryRun bool
	Audit  audit.Auditor // nil-safe: nil drops records (actions are still slog-logged)
	Log    *slog.Logger
}

// Reads pass through untouched — a dry-run sweep must still SEE the queue to report on it.

// ListPRsByLabel passes through to the wrapped forge.
func (g Guard) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListPRsByLabel(ctx, label)
}

// ListIssuesByLabel passes through to the wrapped forge.
func (g Guard) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListIssuesByLabel(ctx, label)
}

// ListClosedUnmergedPRsByLabel passes through to the wrapped forge.
func (g Guard) ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListClosedUnmergedPRsByLabel(ctx, label)
}

// ListIssueCommentBodies passes through to the wrapped forge.
func (g Guard) ListIssueCommentBodies(ctx context.Context, number int) ([]string, error) {
	return g.Inner.ListIssueCommentBodies(ctx, number)
}

// IsPROpen passes through to the wrapped forge.
func (g Guard) IsPROpen(ctx context.Context, number int) (bool, error) {
	return g.Inner.IsPROpen(ctx, number)
}

// Writes are audited and dry-run-able through the single write() choke point below.

// Comment posts a PR comment (audited; skipped in dry-run).
func (g Guard) Comment(ctx context.Context, number int, body string) error {
	return g.write("kb.comment", fmt.Sprintf("pr/%d", number), firstLine(body),
		func() error { return g.Inner.Comment(ctx, number, body) })
}

// ReplaceLabel swaps a label on a PR (audited; skipped in dry-run).
func (g Guard) ReplaceLabel(ctx context.Context, number int, remove, add string) error {
	return g.write("kb.relabel", fmt.Sprintf("pr/%d", number), fmt.Sprintf("%s -> %s", remove, add),
		func() error { return g.Inner.ReplaceLabel(ctx, number, remove, add) })
}

// Close closes a PR (audited; skipped in dry-run).
func (g Guard) Close(ctx context.Context, number int) error {
	return g.write("kb.close", fmt.Sprintf("pr/%d", number), "",
		func() error { return g.Inner.Close(ctx, number) })
}

// OpenIssue opens a knowledge-gap issue (audited; skipped in dry-run).
func (g Guard) OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.open-issue", firstLine(inv.Title), "", func() error {
		var ierr error
		ref, ierr = g.Inner.OpenIssue(ctx, inv)
		return ierr
	})
	return ref, err
}

// OpenRetirePR opens a retire PR for a decayed entry (audited; skipped in dry-run).
func (g Guard) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.retire-pr", entryPath, "", func() error {
		var ierr error
		ref, ierr = g.Inner.OpenRetirePR(ctx, entryPath, body)
		return ierr
	})
	return ref, err
}

// OpenRevalidatePR opens a revalidate PR for a still-working entry (audited;
// skipped in dry-run). The audited reason is the date under proposal — the one
// fact a reviewer of the audit chain needs.
//
// What a dry-run reports is NOT what an apply run would open, and it errs in BOTH
// directions, so do not size review load from it. The "already fresh enough" check
// lives behind the forge call (ErrRecentlyValidated), which a dry-run never makes,
// so a reported candidate may already be fresh — an over-report. But a dry-run
// returns nil, which SPENDS the pass's open-PR budget, so the report also stops at
// max_open, while an apply run skips fresh entries for free and walks further down
// the candidate list — an under-report. Read it as "these entries are in scope",
// not "these PRs will open".
func (g Guard) OpenRevalidatePR(ctx context.Context, entryPath string, validated time.Time, minGap time.Duration, body string) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.revalidate-pr", entryPath, validated.UTC().Format(time.DateOnly), func() error {
		var ierr error
		ref, ierr = g.Inner.OpenRevalidatePR(ctx, entryPath, validated, minGap, body)
		return ierr
	})
	return ref, err
}

// write is the single choke point for every forge mutation a grooming pass performs.
// Dry-run returns nil so a pass's comment-then-close sequencing (Lifecycle, Dedup,
// Suppress) walks both steps and both are visible in the dry-run report.
func (g Guard) write(op, target, reason string, do func() error) error {
	if g.DryRun {
		g.Log.Info("curate dry-run: skipped forge write", "op", op, "target", target, "detail", reason)
		g.record(op, target, audit.DecisionDryRun, reason)
		return nil
	}
	if err := do(); err != nil {
		g.record(op, target, doneSkipOrFailed(err), err.Error())
		return err
	}
	g.record(op, target, audit.DecisionExecuted, reason)
	return nil
}

// doneSkipOrFailed classifies a forge error for the audit chain. The entry-edit
// stamps return sentinels meaning "the forge looked and no edit was warranted" —
// nothing was attempted and nothing failed — so recording them as failures writes
// a claim into an append-only chain that is simply untrue. Nor is it a rare
// mislabel: for revalidation the done-skip IS the steady state, because every
// healthy entry is a candidate on every sweep and nearly all of them are already
// fresh. The chain would fill with "failed" records describing a pass working
// exactly as designed, and drown the records that mean something.
func doneSkipOrFailed(err error) audit.Decision {
	switch {
	case errors.Is(err, github.ErrRecentlyValidated),
		errors.Is(err, github.ErrEntryInactive),
		errors.Is(err, github.ErrAlreadyRetired):
		return audit.DecisionSkipped
	default:
		return audit.DecisionFailed
	}
}

// record appends to the audit chain; a failed audit write must never abort the
// sweep (the action itself already happened or was skipped) — warn and continue.
func (g Guard) record(op, target string, d audit.Decision, reason string) {
	if g.Audit == nil {
		return
	}
	if err := g.Audit.Log(audit.Record{Actor: "curate", Op: op, Target: target, Decision: d, Reason: reason}); err != nil {
		g.Log.Warn("curate audit write failed", "op", op, "target", target, "err", err)
	}
}

// firstLine caps free text to a one-line hint for the audit Reason field (the full
// body lives on the forge artifact itself).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxLen = 120
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}
