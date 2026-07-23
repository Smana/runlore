// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"log/slog"
	"time"
)

// DefaultSweepInterval is the fallback cadence when no interval is configured.
// config.applyDefaults ships the same value; this guard only protects direct users.
const DefaultSweepInterval = 6 * time.Hour

// Sweeper runs the grooming Agent on a fixed interval until ctx is done — the
// in-server counterpart of the `lore curate` CronJob. The first sweep happens one
// FULL interval after start, never immediately: it is launched on every leadership
// (re-)acquisition, and a flapping leader must not stampede the forge with
// back-to-back listing storms. All passes are idempotent, so an occasional
// double-run across a flap is safe, just wasteful.
type Sweeper struct {
	Agent    Agent
	Interval time.Duration // <= 0 ⇒ DefaultSweepInterval
	Log      *slog.Logger
}

// Run sweeps every Interval until ctx is cancelled.
func (s Sweeper) Run(ctx context.Context) {
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultSweepInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.Log.Debug("curate sweep starting")
		s.Agent.Run(ctx)
	}
}
