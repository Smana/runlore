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
	// Persistent what_changed mirror cap: an unset (0) max keeps at most 10 bare
	// mirrors on disk, evicting oldest-mtime first. NewMirrorCache applies the same
	// default, but filling it here keeps the effective config observable.
	if c.GitOps.Mirror.Max == 0 {
		c.GitOps.Mirror.Max = 10
	}
	// Incident debounce default: same 60s hold, for the same reason — let a transient,
	// self-resolving alert clear before burning a paid investigation on it. It also
	// keeps that alert's `resolved` webhook out of the outcome ledger, where it would
	// otherwise credit the recalled entry's resolve rate for a resolution the diagnosis
	// had nothing to do with. An explicit `debounce: 0` (non-nil) is left untouched and
	// investigates on every fire.
	if c.Triggers.Incidents.Debounce == nil {
		d := Duration(60 * time.Second)
		c.Triggers.Incidents.Debounce = &d
	}
	// cancel_queued_on_resolve defaults ON — and it, not the hold, is what filters
	// self-resolving noise on a default install. The debounce hold deliberately skips
	// CRITICAL alerts (a debounce must never delay the first look at a critical page),
	// and the shipped trigger matches `severity: [critical]` exclusively, so the hold
	// would otherwise be dead code there. Cancelling a QUEUED-but-not-started
	// investigation when the resolve lands gets the same saving — no paid investigation,
	// no `resolved` webhook crediting a recalled entry's resolve rate in the outcome
	// ledger — while adding ZERO latency to the page. An explicit `false` (non-nil) is
	// left untouched, for teams who want the post-hoc "why did it fire?" regardless.
	if c.Triggers.Incidents.CancelQueuedOnResolve == nil {
		b := true
		c.Triggers.Incidents.CancelQueuedOnResolve = &b
	}
	// Investigation rate limit: UNSET defaults to 30/h (cost-DoS guard — the count
	// of investigations was unbounded out of the box; the Helm chart already ships
	// an explicit 20). An explicit 0 keeps the documented unlimited meaning.
	if c.Investigation.RateLimit.MaxPerWindow == nil {
		n := 30
		c.Investigation.RateLimit.MaxPerWindow = &n
	}
	// Rate-limit window default: 1h when a per-window budget is in effect but no
	// window is given (a zero window would silently allow unlimited investigations).
	if *c.Investigation.RateLimit.MaxPerWindow > 0 && c.Investigation.RateLimit.Window == 0 {
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
	// Safe-by-default resource caps (C3/B3-budget): anyone running `lore serve
	// --config` or `lore investigate` directly gets the same bounded defaults as
	// the Helm chart (values.yaml: max_tool_output_bytes=32768,
	// max_tokens_per_investigation=100000). Without these defaults only the chart
	// values.yaml applied the caps; a bare YAML config or a direct CLI run got
	// unlimited tool output and token budget, enabling unbounded LLM cost and
	// memory pressure.
	//
	// Consumer convention (unchanged — do NOT flip these, they are relied on across
	// internal/investigate): 0 is the "unlimited" sentinel for truncateOutput,
	// overBudget, and compactionTarget. We map the YAML opt-out value -1 back to 0
	// so consumers see the same unlimited sentinel they always have.
	//   -1 in YAML → 0 in struct (unlimited, opt-in)
	//    0 in YAML → bounded default (safe-by-default, applied here)
	//   >0 in YAML → preserved as-is (explicit user setting)
	switch c.Investigation.MaxToolOutputBytes {
	case -1:
		c.Investigation.MaxToolOutputBytes = 0 // -1 is the user-visible opt-out; map to consumer sentinel
	case 0:
		c.Investigation.MaxToolOutputBytes = 32768 // match the Helm chart default
	}
	switch c.Investigation.MaxTokensPerInvestigation {
	case -1:
		c.Investigation.MaxTokensPerInvestigation = 0 // -1 is the user-visible opt-out; map to consumer sentinel
	case 0:
		c.Investigation.MaxTokensPerInvestigation = 100000 // match the Helm chart default
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
		// Reranker (opt-in) defaults: a STABLE, corpus-independent threshold (0.7) is the
		// whole point — unlike solo_floor it does not need per-cluster tuning. K is bounded
		// small (one cheap call over a few candidates); the trivial min-score floor keeps
		// the paid call from ever running when retrieval surfaced nothing plausible.
		if ir.RerankEnabled() {
			if ir.RerankThreshold == 0 {
				ir.RerankThreshold = 0.7
			}
			if ir.RerankK == 0 {
				ir.RerankK = 5
			}
			if ir.RerankMinScore == 0 {
				ir.RerankMinScore = 0.1
			}
		}
	}
}
