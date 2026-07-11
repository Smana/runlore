// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestApplyDefaultsCoalesceEnabled(t *testing.T) {
	// Only coalesce.enabled set — all numeric fields should get safe defaults.
	var c Config
	c.Investigation.Coalesce.Enabled = true
	applyDefaults(&c)
	co := c.Investigation.Coalesce
	if co.Debounce.Std() != 30*time.Second {
		t.Fatalf("default Debounce: got %v, want 30s", co.Debounce.Std())
	}
	if co.MaxWait.Std() != 2*time.Minute {
		t.Fatalf("default MaxWait: got %v, want 2m", co.MaxWait.Std())
	}
	if co.MaxBatch != 50 {
		t.Fatalf("default MaxBatch: got %d, want 50", co.MaxBatch)
	}
	if co.Cooldown.Std() != 10*time.Minute {
		t.Fatalf("default Cooldown: got %v, want 10m", co.Cooldown.Std())
	}
}

func TestApplyDefaultsRateLimitWindow(t *testing.T) {
	var c Config
	c.Investigation.RateLimit.MaxPerWindow = 10
	applyDefaults(&c)
	if c.Investigation.RateLimit.Window.Std() != time.Hour {
		t.Fatalf("default Window: got %v, want 1h", c.Investigation.RateLimit.Window.Std())
	}
}

func TestApplyDefaultsInvestigationTimeout(t *testing.T) {
	// Unset ⇒ a 10m per-investigation deadline is applied (active out of the box).
	var c Config
	applyDefaults(&c)
	if c.Investigation.Timeout.Std() != 10*time.Minute {
		t.Fatalf("default investigation Timeout: got %v, want 10m", c.Investigation.Timeout.Std())
	}
	// Explicit value is respected, not overwritten.
	var c2 Config
	c2.Investigation.Timeout = Duration(2 * time.Minute)
	applyDefaults(&c2)
	if c2.Investigation.Timeout.Std() != 2*time.Minute {
		t.Fatalf("explicit Timeout overwritten: got %v, want 2m", c2.Investigation.Timeout.Std())
	}
}

func TestApplyDefaultsToolTimeout(t *testing.T) {
	// Unset ⇒ 60s default is applied so cfg.Investigation.ToolTimeout is non-zero
	// after Load(); BuildInvestigator's 0→60s guard then becomes a no-op safety net.
	var c Config
	applyDefaults(&c)
	if c.Investigation.ToolTimeout.Std() != 60*time.Second {
		t.Fatalf("default ToolTimeout: got %v, want 60s", c.Investigation.ToolTimeout.Std())
	}
	// Explicit value is respected, not overwritten.
	var c2 Config
	c2.Investigation.ToolTimeout = Duration(30 * time.Second)
	applyDefaults(&c2)
	if c2.Investigation.ToolTimeout.Std() != 30*time.Second {
		t.Fatalf("explicit ToolTimeout overwritten: got %v, want 30s", c2.Investigation.ToolTimeout.Std())
	}
}

func TestValidateRejectsNegativeToolTimeout(t *testing.T) {
	// time.ParseDuration accepts negative durations; a negative tool_timeout
	// silently disables the feature (fails the > 0 guard) instead of setting a
	// timeout, so it must be rejected at validation time.
	c := &Config{Investigation: Investigation{ToolTimeout: Duration(-1 * time.Second)}}
	if err := c.Validate(); err == nil {
		t.Fatal("negative investigation.tool_timeout must be rejected by Validate")
	}
	// Zero is the "use the default" sentinel and must be accepted.
	ok := &Config{}
	if err := ok.Validate(); err != nil {
		t.Fatalf("zero tool_timeout (use default) must validate clean: %v", err)
	}
	// Positive value is fine.
	pos := &Config{Investigation: Investigation{ToolTimeout: Duration(45 * time.Second)}}
	if err := pos.Validate(); err != nil {
		t.Fatalf("positive tool_timeout must validate clean: %v", err)
	}
}

func TestApplyDefaultsInstantRecall(t *testing.T) {
	// enabled with no tuning → margin/solo gates and decay knobs default to active values.
	var c Config
	c.Catalog.InstantRecall.Enabled = true
	applyDefaults(&c)
	ir := c.Catalog.InstantRecall
	if ir.MinScore != 1.0 || ir.MarginGap != 1.0 || ir.SoloFloor != 4.0 {
		t.Fatalf("instant-recall defaults not applied: %+v", ir)
	}
	if ir.OutcomePrior != 2.0 || ir.OutcomeFloor != 0.5 {
		t.Fatalf("recall-decay defaults not applied: %+v", ir)
	}
}

func TestApplyDefaultsRecallDecayExplicit(t *testing.T) {
	// Explicit decay knobs must not be overwritten.
	var c Config
	c.Catalog.InstantRecall.Enabled = true
	c.Catalog.InstantRecall.OutcomePrior = 5.0
	c.Catalog.InstantRecall.OutcomeFloor = 0.3
	applyDefaults(&c)
	ir := c.Catalog.InstantRecall
	if ir.OutcomePrior != 5.0 || ir.OutcomeFloor != 0.3 {
		t.Fatalf("explicit recall-decay values overwritten: %+v", ir)
	}
}

// rerankPtr is a *bool literal helper for the three-state rerank knob.
func rerankPtr(b bool) *bool { return &b }

// TestRerankEnabledDefault pins the three-state default: instant_recall enabled
// with rerank UNSET ⇒ the reranker is ON (the calibrated gate is what makes recall
// fire out of the box); explicit false disables; disabled recall is always off.
func TestRerankEnabledDefault(t *testing.T) {
	on := InstantRecall{Enabled: true} // Rerank nil ⇒ default ON
	if !on.RerankEnabled() {
		t.Fatal("instant_recall enabled with unset rerank must default the reranker ON")
	}
	if (InstantRecall{Enabled: true, Rerank: rerankPtr(false)}).RerankEnabled() {
		t.Fatal("explicit rerank:false must disable the reranker")
	}
	if !(InstantRecall{Enabled: true, Rerank: rerankPtr(true)}).RerankEnabled() {
		t.Fatal("explicit rerank:true must enable the reranker")
	}
	if (InstantRecall{Enabled: false, Rerank: rerankPtr(true)}).RerankEnabled() {
		t.Fatal("recall disabled ⇒ reranker off regardless of the rerank knob")
	}
}

func TestApplyDefaultsRecallRerank(t *testing.T) {
	// Enabled with no tuning → the reranker is ON by default and its knobs default to
	// the stable, corpus-independent values (nothing to set for it to work).
	var c Config
	c.Catalog.InstantRecall.Enabled = true // Rerank unset ⇒ ON
	applyDefaults(&c)
	ir := c.Catalog.InstantRecall
	if ir.RerankThreshold != 0.7 || ir.RerankK != 5 || ir.RerankMinScore != 0.1 {
		t.Fatalf("rerank defaults not applied under the default-on path: %+v", ir)
	}
	// Explicit rerank:false (legacy BM25-magnitude gate) ⇒ knobs stay zero (unused).
	var off Config
	off.Catalog.InstantRecall.Enabled = true
	off.Catalog.InstantRecall.Rerank = rerankPtr(false)
	applyDefaults(&off)
	if off.Catalog.InstantRecall.RerankThreshold != 0 || off.Catalog.InstantRecall.RerankK != 0 {
		t.Fatalf("rerank knobs must stay zero when rerank is explicitly off: %+v", off.Catalog.InstantRecall)
	}
	// Explicit rerank knobs are respected, not overwritten.
	var ex Config
	ex.Catalog.InstantRecall.Enabled = true
	ex.Catalog.InstantRecall.RerankThreshold = 0.85
	ex.Catalog.InstantRecall.RerankK = 3
	applyDefaults(&ex)
	if ex.Catalog.InstantRecall.RerankThreshold != 0.85 || ex.Catalog.InstantRecall.RerankK != 3 {
		t.Fatalf("explicit rerank values overwritten: %+v", ex.Catalog.InstantRecall)
	}
}

func TestValidateRecallRerank(t *testing.T) {
	base := func() Config {
		var c Config
		c.Catalog.InstantRecall.Enabled = true // Rerank unset ⇒ ON
		return c
	}
	// Defaulted config validates.
	ok := base()
	applyDefaults(&ok)
	if err := ok.Validate(); err != nil {
		t.Fatalf("defaulted rerank config must validate, got %v", err)
	}
	// Threshold out of (0,1] is rejected (it is a calibrated probability).
	for _, bad := range []float64{-0.1, 0, 1.5} {
		c := base()
		c.Catalog.InstantRecall.RerankThreshold = bad
		c.Catalog.InstantRecall.RerankK = 5
		c.Catalog.InstantRecall.RerankMinScore = 0.1
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rerank_threshold") {
			t.Fatalf("threshold %g must be rejected, got %v", bad, err)
		}
	}
	// K < 1 is rejected.
	c := base()
	c.Catalog.InstantRecall.RerankThreshold = 0.7
	c.Catalog.InstantRecall.RerankK = 0 // explicit-but-would-default; force via a post-default check
	c.Catalog.InstantRecall.RerankMinScore = 0.1
	// applyDefaults would fill K=5, so exercise Validate directly with an out-of-range K.
	c.Catalog.InstantRecall.RerankK = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "rerank_k") {
		t.Fatalf("rerank_k -1 must be rejected, got %v", err)
	}
	// Negative min-score is rejected.
	c2 := base()
	c2.Catalog.InstantRecall.RerankThreshold = 0.7
	c2.Catalog.InstantRecall.RerankK = 5
	c2.Catalog.InstantRecall.RerankMinScore = -1
	if err := c2.Validate(); err == nil || !strings.Contains(err.Error(), "rerank_min_score") {
		t.Fatalf("negative rerank_min_score must be rejected, got %v", err)
	}
	// Explicit rerank:false ⇒ the knobs are not validated (they are unused).
	off := Config{}
	off.Catalog.InstantRecall.Enabled = true
	off.Catalog.InstantRecall.Rerank = rerankPtr(false)
	off.Catalog.InstantRecall.RerankThreshold = 99 // nonsense, but ignored while rerank is off
	if err := off.Validate(); err != nil {
		t.Fatalf("rerank OFF must ignore rerank knobs, got %v", err)
	}
}

func TestApplyDefaultsDoesNotOverride(t *testing.T) {
	// Explicit values must not be overwritten.
	var c Config
	c.Investigation.Coalesce.Enabled = true
	c.Investigation.Coalesce.Debounce = Duration(5 * time.Second)
	c.Investigation.Coalesce.MaxBatch = 3
	applyDefaults(&c)
	if c.Investigation.Coalesce.Debounce.Std() != 5*time.Second {
		t.Fatalf("explicit Debounce overwritten: got %v", c.Investigation.Coalesce.Debounce.Std())
	}
	if c.Investigation.Coalesce.MaxBatch != 3 {
		t.Fatalf("explicit MaxBatch overwritten: got %d", c.Investigation.Coalesce.MaxBatch)
	}
}

func TestInvestigationConfigParse(t *testing.T) {
	const y = `
investigation:
  coalesce:
    enabled: true
    debounce: 30s
    max_wait: 2m
    max_batch: 50
    cooldown: 10m
    correlation_labels: [alertname, namespace]
  rate_limit:
    max_per_window: 20
    window: 1h
    max_requeues: 10
  max_steps: 15
  max_tool_output_bytes: 16384
  max_tokens_per_investigation: 120000
  tool_timeout: 45s
  pod_log_namespaces: [flux-system, kube-system]
telemetry:
  metrics_enabled: true
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := strings.Join(c.Investigation.PodLogNamespaces, ","); got != "flux-system,kube-system" {
		t.Fatalf("pod_log_namespaces: got %q", got)
	}
	if !c.Investigation.Coalesce.Enabled {
		t.Fatal("coalesce.enabled should be true")
	}
	if c.Investigation.Coalesce.Debounce.Std() != 30*time.Second {
		t.Fatalf("debounce: got %v", c.Investigation.Coalesce.Debounce.Std())
	}
	if c.Investigation.RateLimit.MaxPerWindow != 20 {
		t.Fatalf("max_per_window: got %d", c.Investigation.RateLimit.MaxPerWindow)
	}
	if c.Investigation.RateLimit.MaxRequeues != 10 {
		t.Fatalf("max_requeues: got %d", c.Investigation.RateLimit.MaxRequeues)
	}
	if c.Investigation.MaxSteps != 15 || c.Investigation.MaxToolOutputBytes != 16384 {
		t.Fatalf("scalar fields: %+v", c.Investigation)
	}
	if c.Investigation.ToolTimeout.Std() != 45*time.Second {
		t.Fatalf("tool_timeout: got %v, want 45s", c.Investigation.ToolTimeout.Std())
	}
	if got := strings.Join(c.Investigation.Coalesce.CorrelationLabels, ","); got != "alertname,namespace" {
		t.Fatalf("correlation_labels: got %q", got)
	}
	if !c.Telemetry.MetricsEnabled {
		t.Fatal("telemetry.metrics_enabled should be true")
	}
}

func TestApplyDefaultsProgressUpdates(t *testing.T) {
	// Enabled without an explicit cadence ⇒ default 5.
	var c Config
	c.Investigation.ProgressUpdates.Enabled = true
	applyDefaults(&c)
	if c.Investigation.ProgressUpdates.EverySteps != 5 {
		t.Fatalf("default every_steps: got %d, want 5", c.Investigation.ProgressUpdates.EverySteps)
	}
	// Explicit cadence respected.
	var c2 Config
	c2.Investigation.ProgressUpdates.Enabled = true
	c2.Investigation.ProgressUpdates.EverySteps = 3
	applyDefaults(&c2)
	if c2.Investigation.ProgressUpdates.EverySteps != 3 {
		t.Fatalf("explicit every_steps overwritten: got %d, want 3", c2.Investigation.ProgressUpdates.EverySteps)
	}
	// Disabled ⇒ left at 0 (unused).
	var c3 Config
	applyDefaults(&c3)
	if c3.Investigation.ProgressUpdates.EverySteps != 0 {
		t.Fatalf("disabled every_steps must stay 0, got %d", c3.Investigation.ProgressUpdates.EverySteps)
	}
}

func TestValidateProgressUpdatesEverySteps(t *testing.T) {
	// Enabled with a negative cadence is rejected (applyDefaults fills unset 0 with 5,
	// so only an explicit negative reaches Validate).
	var c Config
	c.Investigation.ProgressUpdates.Enabled = true
	c.Investigation.ProgressUpdates.EverySteps = -1
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "every_steps") {
		t.Fatalf("expected every_steps validation error, got %v", err)
	}
	// Enabled + positive cadence passes.
	var ok Config
	ok.Investigation.ProgressUpdates.Enabled = true
	ok.Investigation.ProgressUpdates.EverySteps = 5
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid progress config rejected: %v", err)
	}
}

func TestValidatePricingNonNegative(t *testing.T) {
	// Negative rate on the main model is rejected.
	var c Config
	c.Model.Pricing = &Pricing{InputUSDPerMTok: -1}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "model.pricing") {
		t.Fatalf("expected model.pricing validation error, got %v", err)
	}
	// Negative rate on the verify override is rejected.
	var c2 Config
	c2.Model.Verify = &ModelOverride{Pricing: &Pricing{OutputUSDPerMTok: -5}}
	if err := c2.Validate(); err == nil || !strings.Contains(err.Error(), "model.verify.pricing") {
		t.Fatalf("expected model.verify.pricing validation error, got %v", err)
	}
	// Non-negative rates pass.
	var ok Config
	ok.Model.Pricing = &Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15, CachedInputUSDPerMTok: 0.3}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid pricing rejected: %v", err)
	}
}

// TestApplyDefaultsResourceCaps locks in the safe-by-default resource caps
// (C3/B3-budget): a zero-value config must get the bounded Helm-chart defaults
// (32768 bytes / 100000 tokens) so that `lore serve --config` and `lore
// investigate` are never silently unbounded.
func TestApplyDefaultsResourceCaps(t *testing.T) {
	// Unset (zero) → bounded defaults are applied (match the Helm chart values.yaml).
	var c Config
	applyDefaults(&c)
	if c.Investigation.MaxToolOutputBytes != 32768 {
		t.Fatalf("default MaxToolOutputBytes: got %d, want 32768", c.Investigation.MaxToolOutputBytes)
	}
	if c.Investigation.MaxTokensPerInvestigation != 100000 {
		t.Fatalf("default MaxTokensPerInvestigation: got %d, want 100000", c.Investigation.MaxTokensPerInvestigation)
	}
}

// TestApplyDefaultsResourceCapsUnlimited confirms that -1 (the user-visible
// opt-out) maps to 0 (the consumer's unlimited sentinel used by truncateOutput,
// overBudget, and compactionTarget) so internal/investigate never has to know
// about -1.
func TestApplyDefaultsResourceCapsUnlimited(t *testing.T) {
	var c Config
	c.Investigation.MaxToolOutputBytes = -1
	c.Investigation.MaxTokensPerInvestigation = -1
	applyDefaults(&c)
	if c.Investigation.MaxToolOutputBytes != 0 {
		t.Fatalf("-1 MaxToolOutputBytes must map to 0 (unlimited sentinel), got %d", c.Investigation.MaxToolOutputBytes)
	}
	if c.Investigation.MaxTokensPerInvestigation != 0 {
		t.Fatalf("-1 MaxTokensPerInvestigation must map to 0 (unlimited sentinel), got %d", c.Investigation.MaxTokensPerInvestigation)
	}
}

// TestApplyDefaultsResourceCapsExplicit asserts that an explicitly-set positive
// value is preserved and not clobbered by the defaulting logic.
func TestApplyDefaultsResourceCapsExplicit(t *testing.T) {
	var c Config
	c.Investigation.MaxToolOutputBytes = 16384
	c.Investigation.MaxTokensPerInvestigation = 50000
	applyDefaults(&c)
	if c.Investigation.MaxToolOutputBytes != 16384 {
		t.Fatalf("explicit MaxToolOutputBytes overwritten: got %d, want 16384", c.Investigation.MaxToolOutputBytes)
	}
	if c.Investigation.MaxTokensPerInvestigation != 50000 {
		t.Fatalf("explicit MaxTokensPerInvestigation overwritten: got %d, want 50000", c.Investigation.MaxTokensPerInvestigation)
	}
}
