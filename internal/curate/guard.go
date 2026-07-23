// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// GuardedForge is the union of every read and write the grooming passes perform —
// the surface Guard wraps. *github.Client satisfies it (pinned in internal/app).
type GuardedForge interface {
	Forge
	ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ListIssueCommentBodies(ctx context.Context, number int) ([]string, error)
	IsPROpen(ctx context.Context, number int) (bool, error)
	OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error)
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

// Reads: pass-through (a dry-run sweep must still SEE the queue to report on it).

func (g Guard) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListPRsByLabel(ctx, label)
}

func (g Guard) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListIssuesByLabel(ctx, label)
}

func (g Guard) ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListClosedUnmergedPRsByLabel(ctx, label)
}

func (g Guard) ListIssueCommentBodies(ctx context.Context, number int) ([]string, error) {
	return g.Inner.ListIssueCommentBodies(ctx, number)
}

func (g Guard) IsPROpen(ctx context.Context, number int) (bool, error) {
	return g.Inner.IsPROpen(ctx, number)
}

// Writes: audited, dry-run-able.

func (g Guard) Comment(ctx context.Context, number int, body string) error {
	return g.write("kb.comment", fmt.Sprintf("pr/%d", number), firstLine(body),
		func() error { return g.Inner.Comment(ctx, number, body) })
}

func (g Guard) ReplaceLabel(ctx context.Context, number int, remove, add string) error {
	return g.write("kb.relabel", fmt.Sprintf("pr/%d", number), fmt.Sprintf("%s -> %s", remove, add),
		func() error { return g.Inner.ReplaceLabel(ctx, number, remove, add) })
}

func (g Guard) Close(ctx context.Context, number int) error {
	return g.write("kb.close", fmt.Sprintf("pr/%d", number), "",
		func() error { return g.Inner.Close(ctx, number) })
}

func (g Guard) OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.open-issue", firstLine(inv.Title), "", func() error {
		var ierr error
		ref, ierr = g.Inner.OpenIssue(ctx, inv)
		return ierr
	})
	return ref, err
}

func (g Guard) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.retire-pr", entryPath, "", func() error {
		var ierr error
		ref, ierr = g.Inner.OpenRetirePR(ctx, entryPath, body)
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
		g.record(op, target, audit.DecisionFailed, err.Error())
		return err
	}
	g.record(op, target, audit.DecisionExecuted, reason)
	return nil
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
	const max = 120
	if len(s) > max {
		s = s[:max]
	}
	return s
}
