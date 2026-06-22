package telemetry

import (
	"context"
	"testing"
)

func TestNewMetricsNoProvider(t *testing.T) {
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
