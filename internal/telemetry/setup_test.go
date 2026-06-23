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

func TestSetupServesMetrics(t *testing.T) {
	// Reset to the no-op provider after the test so that other tests that call
	// NewMetrics() bind to a clean global provider and not this test's registry.
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics() // instruments now bind to the configured provider
	m.AlertsReceived.Add(context.Background(), 7)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "runlore_alerts_received_total") {
		t.Fatalf("metrics output missing series:\n%s", rec.Body.String())
	}
}
