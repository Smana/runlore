// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"testing"
)

func TestNewMetricsNoProvider(_ *testing.T) {
	// With no provider configured, the global meter is a no-op; instruments must
	// still construct and be safe to call.
	m := NewMetrics()
	ctx := context.Background()
	m.AlertsReceived.Add(ctx, 1)
	m.AlertsCoalesced.Add(ctx, 3)
	m.InvestigationsStarted.Add(ctx, 1)
	m.ToolOutputTruncatedBytes.Add(ctx, 4096)
	m.CoalesceBatchSize.Record(ctx, 12)
	m.InvestigationTokens.Record(ctx, 5000)
}

func TestNewMetricsOutcomeInstruments(_ *testing.T) {
	m := NewMetrics()
	ctx := context.Background()
	m.OutcomesOpened.Add(ctx, 1)
	m.IncidentsResolved.Add(ctx, 1)
	m.RecallOutcome.Add(ctx, 1)
	m.IncidentResolutionSeconds.Record(ctx, 90)
}

func TestNewMetricsCurationInstruments(_ *testing.T) {
	// With no provider configured the global meter is a no-op; the instrument must
	// still construct and be safe to record.
	m := NewMetrics()
	ctx := context.Background()
	m.CurationDedupScore.Record(ctx, 4.2)
}

func TestHistoryCompactionCountersConstructed(t *testing.T) {
	m := NewMetrics()
	if m.HistoryCompactions == nil || m.HistoryElidedBytes == nil {
		t.Fatal("NewMetrics must construct HistoryCompactions and HistoryElidedBytes")
	}
}

func TestModelTokenCountersConstructed(t *testing.T) {
	m := NewMetrics()
	if m.ModelInputTokens == nil || m.ModelCachedInputTokens == nil {
		t.Fatal("NewMetrics must construct ModelInputTokens and ModelCachedInputTokens")
	}
}

func TestMentionsDroppedOnSaturationCounterConstructed(t *testing.T) {
	// Same shape as IncidentsDroppedOnShutdown: accepted, acked, never processed.
	// With no provider configured the global meter is a no-op; the instrument must
	// still construct and be safe to record.
	m := NewMetrics()
	if m.MentionsDroppedOnSaturation == nil {
		t.Fatal("NewMetrics must construct MentionsDroppedOnSaturation")
	}
	m.MentionsDroppedOnSaturation.Add(context.Background(), 1)
}

func TestNewMetricsInvestigationUsageInstruments(t *testing.T) {
	m := NewMetrics()
	if m.InvestigationModelCalls == nil || m.InvestigationInputTokens == nil ||
		m.InvestigationOutputTokens == nil || m.InvestigationCachedInputTokens == nil ||
		m.InvestigationCostUSD == nil {
		t.Fatal("NewMetrics must construct the per-investigation usage/cost instruments")
	}
}

// TestKBDraftDefectsCounterConstructed pins the draft-time defect counter.
//
// It is the number that separates a healthy catalog from one silently filling
// with entries recall can never match: runlore_curations_total{result="opened"}
// counts a doomed entry as a success, so without this the two deployments are
// metrically identical. With no provider configured the global meter is a no-op;
// the instrument must still construct and be safe to record.
func TestKBDraftDefectsCounterConstructed(t *testing.T) {
	m := NewMetrics()
	if m.KBDraftDefects == nil {
		t.Fatal("NewMetrics must construct KBDraftDefects")
	}
	m.KBDraftDefects.Add(context.Background(), 1)
}
