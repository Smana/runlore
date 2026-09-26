// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// RecordModelRequest counts one request to a model-shaped backend on
// runlore_model_requests_total{provider, result} and records its latency on
// runlore_model_request_duration_seconds{provider}. result is "ok" or "error" from err.
// Nil-safe on m, so a caller with no metrics wired records nothing.
//
// One recording site for every model-shaped backend: the inline copies of this pair
// (rerank, summarize, loop, and embed's recordRequest) are being migrated onto it, so
// that no consumer can be instrumented differently from the rest again — the
// curator's dedup decider was, and its outages showed up nowhere but a Warn line.
func (m *Metrics) RecordModelRequest(ctx context.Context, provider string, started time.Time, err error) {
	if m == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	m.ModelRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", provider), attribute.String("result", result)))
	m.ModelRequestDuration.Record(ctx, time.Since(started).Seconds(),
		metric.WithAttributes(attribute.String("provider", provider)))
}
