package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

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
