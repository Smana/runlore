// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
)

// TestLatencyHistogramsUseSLOBuckets asserts the seconds-scale latency histograms
// carry the explicit SLO-aligned bucket boundaries, not the OTel SDK defaults.
// The defaults are {5,10,25,50,75,100,250,...}; a le="2.5" bucket can only exist
// when the explicit view is installed, so this test fails before the buckets change.
func TestLatencyHistogramsUseSLOBuckets(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics()
	ctx := context.Background()
	// Record one sample into each latency histogram so its bucket series materialize.
	m.ToolCallDuration.Record(ctx, 0.2)
	m.ModelRequestDuration.Record(ctx, 0.9)
	m.InvestigationDuration.Record(ctx, 1.5)
	m.IncidentResolutionSeconds.Record(ctx, 90)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// le="2.5" is an SLO boundary present only under the explicit view; absent
	// under the SDK defaults. Checking it on every latency histogram pins the view.
	for _, series := range []string{
		"runlore_tool_call_duration_seconds_bucket",
		"runlore_model_request_duration_seconds_bucket",
		"runlore_investigation_duration_seconds_bucket",
		"runlore_incident_resolution_seconds_bucket",
	} {
		if !bucketHasBoundary(body, series, "2.5") {
			t.Errorf("%s missing SLO boundary le=\"2.5\" — buckets not SLO-aligned\n", series)
		}
	}
}

// bucketHasBoundary reports whether the exposition body has a line for the given
// histogram bucket series carrying the given le boundary value.
func bucketHasBoundary(body, series, le string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, series) && strings.Contains(line, `le="`+le+`"`) {
			return true
		}
	}
	return false
}
