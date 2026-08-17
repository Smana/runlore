// SPDX-License-Identifier: Apache-2.0

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

// scoreBuckets are the bucket boundaries for RunLore's BM25 retrieval-score
// histograms. The OTel SDK defaults start at 5, so an entire real corpus lands in
// the first bucket and the distribution is unreadable — the panels built on these
// histograms render as a single bar. An enriched real-corpus BM25 score is
// ~0.1–1.2 (see the InstantRecall doc comment in internal/config/config.go), so
// the ladder resolves that dense region finely and thins out above it.
//
// Every boundary that is also a DECISION THRESHOLD is deliberate, so a bucket
// count answers "how much of the corpus clears this gate?" directly:
//
//	0.1 → instant_recall.rerank_min_score (skip the paid rerank call below this)
//	1.0 → instant_recall.min_score / margin_gap
//	4.0 → instant_recall.solo_floor
//	5.0 → forge.dup_score
//
// Boundaries above 5 exist for deployments that hand-tuned their thresholds to a
// differently-scaled corpus; +Inf captures anything beyond 10.
var scoreBuckets = []float64{0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 4, 5, 7.5, 10}

// scoreHistograms are the BM25-score instrument names that get the score buckets.
var scoreHistograms = []string{
	"runlore_recall_score",
	"runlore_curation_dedup_score",
}

// bucketViews builds one explicit-bucket-histogram view per named instrument.
func bucketViews(names []string, boundaries []float64) []sdkmetric.View {
	views := make([]sdkmetric.View, 0, len(names))
	for _, name := range names {
		views = append(views, sdkmetric.NewView(
			sdkmetric.Instrument{Name: name},
			sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
				Boundaries: boundaries,
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
	views := bucketViews(latencyHistograms, latencyBuckets)
	views = append(views, bucketViews(scoreHistograms, scoreBuckets)...)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithView(views...),
	)
	otel.SetMeterProvider(mp)
	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	return handler, mp.Shutdown, nil
}
