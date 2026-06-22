package curate

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/providers"
)

// RecurrenceStore persists per-pattern unresolved counts across curate runs.
type RecurrenceStore interface {
	Load(ctx context.Context) (map[string]int, error)
	Save(ctx context.Context, counts map[string]int) error
}

// Recurrence tracks unresolved-pattern recurrence and opens ONE knowledge-gap
// issue when a pattern crosses the threshold — the only path that creates issues.
type Recurrence struct {
	Forge     Forge
	Store     RecurrenceStore
	Threshold int // default 3 when 0
	Log       *slog.Logger
}

// Observe records one unresolved occurrence of a pattern (a Fingerprint-style key)
// and opens a knowledge-gap issue when it first reaches the threshold.
func (r Recurrence) Observe(ctx context.Context, pattern string) error {
	thr := r.Threshold
	if thr == 0 {
		thr = 3
	}
	counts, err := r.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load recurrence: %w", err)
	}
	if counts == nil {
		counts = map[string]int{}
	}
	counts[pattern]++
	shouldOpen := counts[pattern] == thr // exactly at threshold → open once
	// Persist the count BEFORE opening the issue. If we opened first and Save then
	// failed, the next run would re-load the pre-increment count, re-hit the
	// threshold, and double-open. Saving first means a later OpenIssue failure
	// leaves the count at thr (next run increments past it → the == thr guard
	// never re-fires).
	if err := r.Store.Save(ctx, counts); err != nil {
		return fmt.Errorf("save recurrence: %w", err)
	}
	if shouldOpen {
		inv := providers.Investigation{
			Title: "knowledge-gap: " + pattern,
			RootCauses: []providers.Hypothesis{{
				Summary: fmt.Sprintf("RunLore could not resolve %q across %d incidents — needs seeded knowledge or a RunLore fix.", pattern, thr),
			}},
		}
		if _, err := r.Forge.OpenIssue(ctx, inv); err != nil {
			return fmt.Errorf("open knowledge-gap issue: %w", err)
		}
		r.Log.Info("opened knowledge-gap issue", "pattern", pattern, "count", thr)
	}
	return nil
}
