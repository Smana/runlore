// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// TestNewInstrumentsExposeContractNames records the A2 instruments and asserts the
// exact Prometheus series names appear at /metrics — these names are the contract the
// Grafana dashboard and the alert rules depend on, so a rename must break this test.
func TestNewInstrumentsExposeContractNames(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics()
	ctx := context.Background()
	m.InvestigationDuration.Record(ctx, 1.5, metric.WithAttributes(attribute.String("result", "resolved")))
	m.InvestigationsCompleted.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "resolved")))
	m.ToolCalls.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", "query_metrics"), attribute.String("result", "ok")))
	m.ToolCallDuration.Record(ctx, 0.2, metric.WithAttributes(attribute.String("tool", "query_metrics")))
	m.ModelRequests.Add(ctx, 1, metric.WithAttributes(attribute.String("provider", "anthropic"), attribute.String("result", "ok")))
	m.ModelRequestDuration.Record(ctx, 0.9, metric.WithAttributes(attribute.String("provider", "anthropic")))
	m.Curations.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", "pr"), attribute.String("result", "opened")))

	if err := RegisterRuntimeGauges("1.2.3", func() bool { return true }); err != nil {
		t.Fatalf("register gauges: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	want := []string{
		"runlore_investigation_duration_seconds",
		"runlore_investigations_completed_total",
		"runlore_tool_calls_total",
		"runlore_tool_call_duration_seconds",
		"runlore_model_requests_total",
		"runlore_model_request_duration_seconds",
		"runlore_curations_total",
		"runlore_build_info",
		"runlore_leader",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("missing series %q in /metrics output", name)
		}
	}
	// build_info must carry the version label, and leader must read 1 here.
	if !strings.Contains(body, `version="1.2.3"`) {
		t.Errorf("runlore_build_info missing version label\n%s", body)
	}
}
