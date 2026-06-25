package telemetry

import (
	"context"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// latencyBuckets are the SLO-aligned bucket boundaries (seconds) for RunLore's
// latency histograms. The OTel SDK defaults are tuned for millisecond-scale
// values, so seconds-scale tool/model/investigation latencies collapse into the
// first default bucket and make histogram_quantile useless below ~5s. This ladder
// resolves fast calls (50–250ms), the typical tool/model range (0.5–2.5s), slow
// calls (5–10s), and long investigations / incident resolution (30s–5min); the
// +Inf bucket captures the tail beyond 300s.
var latencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// latencyHistograms are the seconds-scale instrument names that get the SLO buckets.
// Names are the exported Prometheus series names (the names OTel sees), matched by
// the views below.
var latencyHistograms = []string{
	"runlore_tool_call_duration_seconds",
	"runlore_model_request_duration_seconds",
	"runlore_investigation_duration_seconds",
	"runlore_incident_resolution_seconds",
}

// sloLatencyViews builds one explicit-bucket-histogram view per latency instrument.
func sloLatencyViews() []sdkmetric.View {
	views := make([]sdkmetric.View, 0, len(latencyHistograms))
	for _, name := range latencyHistograms {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: latencyBuckets,
			}},
		))
	}
	return views
}

// Setup installs a global OTel meter provider backed by a Prometheus exporter
// and returns an http.Handler that serves the exposition format, plus a
// shutdown func. Call NewMetrics AFTER Setup so instruments bind to this provider.
func Setup(_ context.Context) (http.Handler, func(context.Context) error, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithView(sloLatencyViews()...),
	)
	otel.SetMeterProvider(mp)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return handler, mp.Shutdown, nil
}
