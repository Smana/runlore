package curate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// gapTitlePrefix titles every knowledge-gap issue; it is also the existence-check
// key (the pass opens at most one issue per pattern).
const gapTitlePrefix = "knowledge-gap: "

// Recurrence opens ONE knowledge-gap issue when an unresolved incident pattern recurs
// at least Threshold times. It is ledger-driven and idempotent: it recomputes counts
// from the outcome ledger every run and opens an issue only when no knowledge-gap
// issue for that pattern already exists — so re-running never double-opens (no mutable
// store or watermark).
type Recurrence struct {
	Forge  Forge
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
	Threshold int // default 3 when 0
	Log       *slog.Logger
}

// Run opens a knowledge-gap issue for each pattern that crosses the threshold and has
// none open yet.
func (r Recurrence) Run(ctx context.Context) error {
	thr := r.Threshold
	if thr == 0 {
		thr = 3
	}
	eps, err := r.Ledger.Episodes()
	if err != nil {
		return fmt.Errorf("recurrence: load episodes: %w", err)
	}
	// Count UNRESOLVED occurrences per pattern (affected resource; title fallback).
	counts := map[string]int{}
	for _, e := range eps {
		if e.Resolved {
			continue
		}
		counts[recurrencePattern(e)]++
	}
	// Existing knowledge-gap issues are the idempotency guard. OpenIssue labels issues
	// "runlore"/"triggered" and titles them gapTitlePrefix+pattern, so match by title.
	existing, err := r.Forge.ListIssuesByLabel(ctx, "runlore")
	if err != nil {
		return fmt.Errorf("recurrence: list issues: %w", err)
	}
	open := map[string]bool{}
	for _, iss := range existing {
		if p, ok := strings.CutPrefix(iss.Title, gapTitlePrefix); ok {
			open[p] = true
		}
	}
	for pattern, n := range counts {
		if n < thr || open[pattern] {
			continue
		}
		inv := providers.Investigation{
			Title: gapTitlePrefix + pattern,
			RootCauses: []providers.Hypothesis{{
				Summary: fmt.Sprintf("RunLore could not resolve incidents on %q across %d occurrences — needs seeded knowledge or a RunLore fix.", pattern, n),
			}},
		}
		if _, err := r.Forge.OpenIssue(ctx, inv); err != nil {
			r.Log.Warn("recurrence: open knowledge-gap issue failed", "pattern", pattern, "err", err)
			continue // best-effort; other patterns still processed
		}
		r.Log.Info("opened knowledge-gap issue", "pattern", pattern, "count", n)
	}
	return nil
}

// recurrencePattern groups unresolved incidents by the affected resource, falling
// back to the incident title when no resource was identified.
func recurrencePattern(e outcome.Episode) string {
	if e.Resource != "" {
		return e.Resource
	}
	return e.Title
}
