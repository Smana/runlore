// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"log/slog"
)

// Pass is one grooming pass (dedup, queue, lifecycle…). Each is independent and
// resilient: a pass error is logged, not fatal — the agent runs the rest.
type Pass interface {
	Run(ctx context.Context) error
}

// Agent runs the grooming passes in order. Forge-only writes; never merges.
type Agent struct {
	Passes []Pass
	Log    *slog.Logger
}

// Run executes every pass, logging (not propagating) per-pass errors.
func (a Agent) Run(ctx context.Context) {
	for _, p := range a.Passes {
		if err := p.Run(ctx); err != nil {
			a.Log.Error("curate pass failed", "err", err)
		}
	}
}
