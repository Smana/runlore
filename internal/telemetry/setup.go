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

// Setup installs a global OTel meter provider backed by a Prometheus exporter
// and returns an http.Handler that serves the exposition format, plus a
// shutdown func. Call NewMetrics AFTER Setup so instruments bind to this provider.
func Setup(ctx context.Context) (http.Handler, func(context.Context) error, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg))
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	otel.SetMeterProvider(mp)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return handler, mp.Shutdown, nil
}
