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
// One helper for every consumer of a backend, because the two decision-model
// consumers instrumented this pair separately and one of them (the curator's dedup
// decider) did not: a decider returning 401 on every curation fell back to BM25
// behind a Warn line while the series the observability page points operators at
// stayed flat. Now the recorder is the same call in both places.
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
