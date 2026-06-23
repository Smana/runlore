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
telemetry:
  metrics_enabled: true
`
	var c Config
	if err := yaml.Unmarshal([]byte(y), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
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
	if got := strings.Join(c.Investigation.Coalesce.CorrelationLabels, ","); got != "alertname,namespace" {
		t.Fatalf("correlation_labels: got %q", got)
	}
	if !c.Telemetry.MetricsEnabled {
		t.Fatal("telemetry.metrics_enabled should be true")
	}
}
