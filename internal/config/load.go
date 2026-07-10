// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads, strictly parses, and validates a RunLore config file. Unknown keys
// are rejected (KnownFields) so a typo in a safety-critical field — e.g. an
// autonomy gate — fails loudly instead of being silently ignored.
func Load(path string) (*Config, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the operator-supplied config file
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer func() { _ = f.Close() }()
	var c Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyDefaults(&c)
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &c, nil
}

// applyDefaults fills in safe zero-value defaults for optional fields so that a
// minimal config (e.g. coalesce.enabled: true with no sub-fields) is valid and
// predictable without requiring every field to be spelled out.
func applyDefaults(c *Config) {
	// Coalesce defaults: when enabled without explicit tuning, choose conservative
	// values that reduce storm noise without introducing too much investigation lag.
	if c.Investigation.Coalesce.Enabled {
		co := &c.Investigation.Coalesce
		if co.Debounce == 0 {
			co.Debounce = Duration(30 * time.Second)
		}
		if co.MaxWait == 0 {
			co.MaxWait = Duration(2 * time.Minute)
		}
		if co.MaxBatch == 0 {
			co.MaxBatch = 50
		}
		if co.Cooldown == 0 {
			co.Cooldown = Duration(10 * time.Minute)
		}
	}
	// GitOps-failure debounce default: when the window is unset (nil), wait 60s and
	// re-check before investigating — long enough to let reconcile-churn transients
	// clear, short enough to catch real failures promptly. An explicit `debounce: 0`
	// (non-nil) is left untouched, so it fires on every Ready=False as documented.
	// Applied unconditionally now that enablement lives under sources.gitops; it is
	// harmless when the gitops source is disabled (the debouncer is never built).
	if c.Triggers.GitOpsFailures.Debounce == nil {
		d := Duration(60 * time.Second)
		c.Triggers.GitOpsFailures.Debounce = &d
	}
	// Rate-limit window default: 1h when a per-window budget is set but no window
	// is given (a zero window would silently allow unlimited investigations).
	if c.Investigation.RateLimit.MaxPerWindow > 0 && c.Investigation.RateLimit.Window == 0 {
		c.Investigation.RateLimit.Window = Duration(time.Hour)
	}
	// Per-investigation deadline: bound a whole investigation (recall + every model/
	// tool call, incl. a hung git clone) so one stuck investigation can't starve the
	// single-worker queue. Default 10m when unset.
	if c.Investigation.Timeout == 0 {
		c.Investigation.Timeout = Duration(10 * time.Minute)
	}
	// Per-tool-call timeout: default to 60s when unset so one hung tool can't eat
	// the whole per-investigation budget while still allowing legitimately slow queries
	// (log scans, range PromQL) to finish. BuildInvestigator's 0→60s guard becomes a
	// no-op when already defaulted here; it remains as a safety net for callers that
	// bypass Load (e.g. tests that build a LoopInvestigator directly).
	if c.Investigation.ToolTimeout == 0 {
		c.Investigation.ToolTimeout = Duration(60 * time.Second)
	}
	// Interim progress-update default: when enabled without an explicit cadence,
	// ping every 5 steps — frequent enough to reassure on a long investigation,
	// sparse enough to avoid chat noise. Left at 0 when disabled (unused).
	if c.Investigation.ProgressUpdates.Enabled && c.Investigation.ProgressUpdates.EverySteps == 0 {
		c.Investigation.ProgressUpdates.EverySteps = 5
	}
	// Instant-recall trust defaults: when enabled without explicit tuning, keep the
	// margin and solo gates active. A zero margin_gap/solo_floor would degrade recall
	// to a bare similarity threshold — the exact brittleness this feature removes.
	if c.Catalog.InstantRecall.Enabled {
		ir := &c.Catalog.InstantRecall
		if ir.MinScore == 0 {
			ir.MinScore = 1.0
		}
		if ir.MarginGap == 0 {
			ir.MarginGap = 1.0
		}
		if ir.SoloFloor == 0 {
			ir.SoloFloor = 4.0
		}
		if ir.OutcomePrior == 0 {
			ir.OutcomePrior = 2.0
		}
		if ir.OutcomeFloor == 0 {
			ir.OutcomeFloor = 0.5
		}
	}
}
