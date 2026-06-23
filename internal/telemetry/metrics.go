// Package telemetry provides RunLore's self-instrumentation: an OpenTelemetry
// metric set plus a Prometheus-exporter HTTP handler. Instruments are safe to
// call even when no provider is configured (the global meter is a no-op), so
// callers never need nil checks.
package telemetry

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const scope = "github.com/Smana/runlore"

// Metrics is the RunLore instrument set, created once and shared.
type Metrics struct {
	AlertsReceived           metric.Int64Counter
	AlertsCoalesced          metric.Int64Counter
	AlertsSuppressed         metric.Int64Counter
	InvestigationsStarted    metric.Int64Counter
	InvestigationsThrottled  metric.Int64Counter
	InvestigationsDropped    metric.Int64Counter
	ToolOutputTruncatedBytes metric.Int64Counter
	RecallHits               metric.Int64Counter // KB cache hits, labelled by verify result
	RecallTokensSaved        metric.Int64Counter // estimated tokens saved by a recall short-circuit
	CoalesceBatchSize        metric.Int64Histogram
	InvestigationTokens      metric.Int64Histogram
	RecallScore              metric.Float64Histogram // BM25 score at the recall decision (tunes min_score)
}

// NewMetrics builds the instrument set from the global meter provider.
func NewMetrics() *Metrics {
	m := otel.Meter(scope)
	ctr := func(name, desc string) metric.Int64Counter {
		c, _ := m.Int64Counter("runlore_"+name, metric.WithDescription(desc))
		return c
	}
	hist := func(name, desc string) metric.Int64Histogram {
		h, _ := m.Int64Histogram("runlore_"+name, metric.WithDescription(desc))
		return h
	}
	histF := func(name, desc string) metric.Float64Histogram {
		h, _ := m.Float64Histogram("runlore_"+name, metric.WithDescription(desc))
		return h
	}
	return &Metrics{
		AlertsReceived:           ctr("alerts_received_total", "incidents passing Decide into the coalescer"),
		AlertsCoalesced:          ctr("alerts_coalesced_total", "incidents folded into an existing batch"),
		AlertsSuppressed:         ctr("alerts_suppressed_total", "incidents dropped by cooldown"),
		InvestigationsStarted:    ctr("investigations_started_total", "investigations actually begun"),
		InvestigationsThrottled:  ctr("investigations_throttled_total", "starts requeued by the rate limiter"),
		InvestigationsDropped:    ctr("investigations_dropped_total", "keys dropped after max_requeues"),
		ToolOutputTruncatedBytes: ctr("tool_output_truncated_bytes_total", "bytes elided by output truncation"),
		RecallHits:               ctr("recall_hits_total", "KB instant-recall short-circuits (label: result)"),
		RecallTokensSaved:        ctr("recall_tokens_saved_total", "estimated tokens saved by recall short-circuits"),
		CoalesceBatchSize:        hist("coalesce_batch_size", "incidents per flushed batch"),
		InvestigationTokens:      hist("investigation_tokens_estimated", "per-investigation token estimate (investigation loop only; excludes the adversarial verify phase)"),
		RecallScore:              histF("recall_score", "BM25 score at the recall decision point"),
	}
}
