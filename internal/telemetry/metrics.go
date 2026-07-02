// Package telemetry provides RunLore's self-instrumentation: an OpenTelemetry
// metric set plus a Prometheus-exporter HTTP handler. Instruments are safe to
// call even when no provider is configured (the global meter is a no-op), so
// callers never need nil checks.
package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const scope = "github.com/Smana/runlore"

// Metrics is the RunLore instrument set, created once and shared.
type Metrics struct {
	AlertsReceived            metric.Int64Counter
	AlertsCoalesced           metric.Int64Counter
	AlertsSuppressed          metric.Int64Counter
	InvestigationsStarted     metric.Int64Counter
	InvestigationsThrottled   metric.Int64Counter
	InvestigationsDropped     metric.Int64Counter
	GitOpsFailuresDebounced   metric.Int64Counter // GitOps failures dropped as transient (cleared within the debounce window)
	IncidentsDebounced        metric.Int64Counter // firing alerts dropped as self-resolving (resolved within the incident debounce window)
	ToolOutputTruncatedBytes  metric.Int64Counter
	HistoryCompactions        metric.Int64Counter // mid-loop history compaction events
	HistoryElidedBytes        metric.Int64Counter // tool-output bytes elided by compaction
	HistorySummarizations     metric.Int64Counter // compaction events whose elided batch was replaced by a model digest
	HistorySummarizeFallbacks metric.Int64Counter // summarize-mode compactions that fell back to plain elision (summarizer error/refusal/truncation)
	RecallHits                metric.Int64Counter // KB cache hits, labelled by verify result
	RecallTokensSaved         metric.Int64Counter // estimated tokens saved by a recall short-circuit
	RecallRejections          metric.Int64Counter // recalls rejected before short-circuit (label: reason)
	CoalesceBatchSize         metric.Int64Histogram
	InvestigationTokens       metric.Int64Histogram
	// Per-investigation model usage totals (loop + verify), recorded once at
	// delivery — the actual provider-reported spend, distinct from the pre-request
	// InvestigationTokens estimate.
	InvestigationModelCalls        metric.Int64Histogram
	InvestigationInputTokens       metric.Int64Histogram
	InvestigationOutputTokens      metric.Int64Histogram
	InvestigationCachedInputTokens metric.Int64Histogram
	InvestigationCostUSD           metric.Float64Histogram // estimated per-investigation cost, USD (only when model.pricing is configured)
	RecallScore                    metric.Float64Histogram // BM25 score at the recall decision (tunes min_score)
	CurationDedupScore             metric.Float64Histogram // catalog top-hit BM25 score at the curation dedup decision
	OutcomesOpened                 metric.Int64Counter     // investigations recorded as open (label: kind)
	IncidentsResolved              metric.Int64Counter     // resolve events that matched an open investigation
	RecallOutcome                  metric.Int64Counter     // resolved incidents whose open was a recall (label: result)
	IncidentResolutionSeconds      metric.Float64Histogram // open→resolve duration, seconds

	InvestigationDuration   metric.Float64Histogram // wall-clock per investigation (label: result)
	InvestigationsCompleted metric.Int64Counter     // investigations finished (label: result)
	ToolCalls               metric.Int64Counter     // investigation tool calls (label: tool, result)
	ToolCallDuration        metric.Float64Histogram // tool call latency, seconds (label: tool)
	ModelRequests           metric.Int64Counter     // LLM completion requests (label: provider, result)
	ModelRequestDuration    metric.Float64Histogram // LLM completion latency, seconds (label: provider)
	ModelResponsesTruncated metric.Int64Counter     // LLM completions cut off at the output-token ceiling (label: provider)
	ModelInputTokens        metric.Int64Counter     // total input tokens across LLM requests (label: provider)
	ModelCachedInputTokens  metric.Int64Counter     // input tokens served from cache (label: provider)
	Curations               metric.Int64Counter     // curation outcomes (label: kind, result)
	CatalogInvalidEntries   metric.Int64Counter     // structurally-invalid entries found at catalog load
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
		AlertsReceived:            ctr("alerts_received_total", "incidents passing Decide into the coalescer"),
		AlertsCoalesced:           ctr("alerts_coalesced_total", "incidents folded into an existing batch"),
		AlertsSuppressed:          ctr("alerts_suppressed_total", "incidents dropped by cooldown"),
		InvestigationsStarted:     ctr("investigations_started_total", "investigations actually begun"),
		InvestigationsThrottled:   ctr("investigations_throttled_total", "starts requeued by the rate limiter"),
		InvestigationsDropped:     ctr("investigations_dropped_total", "investigations dropped — rate-limiter max_requeues or token-budget hard-kill"),
		GitOpsFailuresDebounced:   ctr("gitops_failures_debounced_total", "GitOps failures dropped as transient: cleared within the debounce window before investigating"),
		IncidentsDebounced:        ctr("incidents_debounced_total", "firing alerts dropped as self-resolving: a matching resolved webhook arrived within the incident debounce window before investigating"),
		ToolOutputTruncatedBytes:  ctr("tool_output_truncated_bytes_total", "bytes elided by output truncation"),
		HistoryCompactions:        ctr("history_compactions_total", "mid-loop tool-output history compaction events"),
		HistoryElidedBytes:        ctr("history_elided_bytes_total", "tool-output bytes elided by mid-loop compaction"),
		HistorySummarizations:     ctr("history_summarizations_total", "compaction events whose elided batch was replaced by a model-produced digest"),
		HistorySummarizeFallbacks: ctr("history_summarize_fallbacks_total", "summarize-mode compactions that fell back to plain elision (summarizer error/refusal/truncation)"),
		RecallHits:                ctr("recall_hits_total", "KB instant-recall short-circuits (label: result)"),
		RecallTokensSaved:         ctr("recall_tokens_saved_total", "estimated tokens saved by recall short-circuits"),
		RecallRejections:          ctr("recall_rejections_total", "recalls rejected before short-circuit (label: reason)"),
		CoalesceBatchSize:         hist("coalesce_batch_size", "incidents per flushed batch"),
		InvestigationTokens:       hist("investigation_tokens_estimated", "per-investigation token estimate (investigation loop only; excludes the adversarial verify phase)"),

		InvestigationModelCalls:        hist("investigation_model_calls", "model completions per investigation (loop + verify)"),
		InvestigationInputTokens:       hist("investigation_input_tokens", "provider-reported input tokens per investigation, including cached (loop + verify)"),
		InvestigationOutputTokens:      hist("investigation_output_tokens", "provider-reported output tokens per investigation (loop + verify)"),
		InvestigationCachedInputTokens: hist("investigation_cached_input_tokens", "input tokens served from cache per investigation (loop + verify)"),
		InvestigationCostUSD:           histF("investigation_cost_usd", "estimated per-investigation cost in USD (only when model.pricing is configured)"),
		RecallScore:                    histF("recall_score", "BM25 score at the recall decision point"),
		CurationDedupScore:             histF("curation_dedup_score", "catalog top-hit BM25 score at the curation dedup decision"),
		OutcomesOpened:                 ctr("outcomes_opened_total", "investigations recorded in the outcome ledger (label: kind)"),
		IncidentsResolved:              ctr("incidents_resolved_total", "resolve events that matched an open investigation"),
		RecallOutcome:                  ctr("recall_outcome_total", "resolved incidents whose answer was a recall (label: result)"),
		IncidentResolutionSeconds:      histF("incident_resolution_seconds", "open→resolve duration in seconds"),

		InvestigationDuration:   histF("investigation_duration_seconds", "wall-clock duration of an investigation (label: result)"),
		InvestigationsCompleted: ctr("investigations_completed_total", "investigations that finished (label: result)"),
		ToolCalls:               ctr("tool_calls_total", "investigation tool calls (label: tool, result)"),
		ToolCallDuration:        histF("tool_call_duration_seconds", "investigation tool call latency in seconds (label: tool)"),
		ModelRequests:           ctr("model_requests_total", "LLM completion requests (label: provider, result)"),
		ModelRequestDuration:    histF("model_request_duration_seconds", "LLM completion latency in seconds (label: provider)"),
		ModelResponsesTruncated: ctr("model_responses_truncated_total", "LLM completions cut off at the output-token ceiling — the provider stop/finish reason indicated truncation (label: provider)"),
		ModelInputTokens:        ctr("model_input_tokens_total", "total LLM input tokens, including cached (label: provider)"),
		ModelCachedInputTokens:  ctr("model_cached_input_tokens_total", "LLM input tokens served from cache (label: provider)"),
		Curations:               ctr("curations_total", "curation outcomes written to the forge (label: kind, result)"),
		CatalogInvalidEntries:   ctr("catalog_invalid_entries_total", "structurally-invalid entries surfaced at catalog load"),
	}
}

// RegisterRuntimeGauges registers async gauges for build info and leadership. Call
// once after Setup (when a real provider is installed); with the no-op provider it
// is a harmless no-op. isLeader may be nil (always reports standby). It exposes:
//   - runlore_build_info{version} = 1  (for `absent()` liveness + version display)
//   - runlore_leader = 1 when this replica is the elected leader, else 0
func RegisterRuntimeGauges(version string, isLeader func() bool) error {
	m := otel.Meter(scope)
	if _, err := m.Int64ObservableGauge("runlore_build_info",
		metric.WithDescription("build info; constant 1 carrying a version label"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, metric.WithAttributes(attribute.String("version", version)))
			return nil
		}),
	); err != nil {
		return err
	}
	_, err := m.Int64ObservableGauge("runlore_leader",
		metric.WithDescription("1 when this replica is the elected leader, else 0"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			var v int64
			if isLeader != nil && isLeader() {
				v = 1
			}
			o.Observe(v)
			return nil
		}),
	)
	return err
}
