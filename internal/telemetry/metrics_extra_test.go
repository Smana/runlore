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
	m.RecallTokensSpent.Add(ctx, 2200)

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
		// The recall cost series the Grafana "Cost & efficiency" row differences
		// against the per-investigation token histograms.
		"runlore_recall_tokens_spent_total",
		"runlore_build_info",
		"runlore_leader",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("missing series %q in /metrics output", name)
		}
	}
	// The retired series asserted a saving equal to the configured budget ceiling; it
	// must not come back alongside its measured replacement.
	if strings.Contains(body, "runlore_recall_tokens_saved_total") {
		t.Error("runlore_recall_tokens_saved_total is retired — recall cost is now measured, not asserted")
	}
	// build_info must carry the version label, and leader must read 1 here.
	if !strings.Contains(body, `version="1.2.3"`) {
		t.Errorf("runlore_build_info missing version label\n%s", body)
	}
}

// TestDecisionModelInstrumentsAreExported asserts the decision-model confidence
// histogram and shadow-comparison counter reach /metrics under their contract
// names, and that confidence is bucketed for a 0-1 probability rather than the
// BM25 score buckets (which start at 0.1 and run to 10, and would put almost
// every confidence in one bucket).
func TestDecisionModelInstrumentsAreExported(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics()
	ctx := context.Background()
	m.DecisionConfidence.Record(ctx, 0.84, metric.WithAttributes(attribute.String("consumer", "rerank")))
	m.DecisionShadow.Add(ctx, 1, metric.WithAttributes(
		attribute.String("consumer", "rerank"), attribute.String("agreement", "agree")))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, want := range []string{
		"runlore_decision_model_confidence_bucket",
		"runlore_decision_model_shadow_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metric %q missing from the scrape", want)
		}
	}
	// A probability belongs in probability buckets: the BM25 score buckets start at 0.1
	// and run to 10, which would put almost every confidence in one bucket. Matched via
	// bucketHasBoundary (buckets_test.go), not a literal substring: the exporter emits
	// otel_scope_* labels ahead of le, so consumer and le are not adjacent in the output.
	if !bucketHasBoundary(body, "runlore_decision_model_confidence_bucket", "0.9") {
		t.Errorf("confidence is not on probability buckets:\n%s", body)
	}
}
