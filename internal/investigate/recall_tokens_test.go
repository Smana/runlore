// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// meteredInstruments installs a REAL SDK meter provider backed by a manual reader and
// returns the instrument set bound to it, plus a reader that sums an int64 counter by
// its exported series name (0, false when the series was never recorded). The provider
// is global, so the returned instruments must be used by exactly one test at a time
// (no t.Parallel here) and the cleanup restores the no-op provider.
func meteredInstruments(t *testing.T) (*telemetry.Metrics, func(series string) (int64, bool)) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func(series string) (int64, bool) {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != series {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("series %q is not an int64 sum (%T)", series, md.Data)
				}
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				return total, true
			}
		}
		return 0, false
	}
}

// TestRecallTokensSpentTracksObservedUsage pins the recall cost metric to what the
// recall ACTUALLY spent, not to the configured budget ceiling.
//
// The regression it guards: the counter used to record MaxTokensPerInvestigation (the
// ceiling — 400k by default, many times a real investigation's observed spend) and
// never subtracted the recall's own model calls, so the series asserted a saving nobody
// measured. Both cases below configure a 100_000 ceiling precisely so a metric that
// still reads the ceiling cannot pass.
//
// The rerank case matters because a fired recall under the SHIPPED defaults is two
// model calls, not one — the LLM reranker plus the adversarial verify pass — and both
// land in the same verifyTotals the short-circuit prices from.
func TestRecallTokensSpentTracksObservedUsage(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	keepVerdict := `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`

	tests := []struct {
		name string
		// build wires an investigator whose recall fires and short-circuits, with the
		// instrument set under test attached to both the loop and the Recall.
		build func(m *telemetry.Metrics) (*LoopInvestigator, Request)
		// want is the sum of the provider-reported input+output tokens of every model
		// call the recall path makes.
		want int64
	}{
		{
			// Verify only (no reranker): one model call, the adversarial review of the
			// recalled entry.
			name: "verify_only",
			build: func(m *telemetry.Metrics) (*LoopInvestigator, Request) {
				verify := &scriptModel{responses: []providers.CompletionResponse{{
					ToolCalls: []providers.ToolCall{{ID: "v1", Name: submitVerdictsName, Args: keepVerdict}},
					Usage:     providers.Usage{InputTokens: 900, OutputTokens: 60},
				}}}
				li := &LoopInvestigator{
					Model:                     verify,
					Verify:                    true,
					MaxTokensPerInvestigation: 100_000,
					Log:                       discard,
					Metrics:                   m,
					Recall: &Recall{
						MinScore: 2.0, Metrics: m,
						Catalog: fakeScored{hits: []catalog.ScoredEntry{{
							Entry: catalog.Entry{Title: "Known incident", Description: "chart bump", Path: "known.md", Resource: "tooling/harbor"},
							Score: 5.0,
						}}},
					},
				}
				return li, Request{Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}
			},
			want: 960,
		},
		{
			// The shipped default: reranker ON, verify ON — TWO model calls, so a metric
			// that prices only the verify pass under-reports the recall's cost.
			name: "rerank_and_verify",
			build: func(m *telemetry.Metrics) (*LoopInvestigator, Request) {
				reranker := &countingReranker{resp: providers.CompletionResponse{
					ToolCalls: []providers.ToolCall{{ID: "rr", Name: rerankToolName, Args: `{"match":true,"entry_id":"web.md","confidence":0.85}`}},
					Usage:     providers.Usage{InputTokens: 1200, OutputTokens: 40},
				}}
				verify := &scriptModel{responses: []providers.CompletionResponse{{
					ToolCalls: []providers.ToolCall{{ID: "v1", Name: submitVerdictsName, Args: keepVerdict}},
					Usage:     providers.Usage{InputTokens: 900, OutputTokens: 60},
				}}}
				r := rerankRecall(reranker, []catalog.ScoredEntry{webHit("web.md", 0.6)})
				r.Metrics = m
				li := &LoopInvestigator{
					Model:                     verify,
					Verify:                    true,
					MaxTokensPerInvestigation: 100_000,
					Log:                       discard,
					Metrics:                   m,
					Recall:                    r,
				}
				return li, okReq()
			},
			want: 2200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, read := meteredInstruments(t)
			li, req := tc.build(m)
			var delivered providers.Investigation
			li.OnComplete = func(inv providers.Investigation) { delivered = inv }
			if err := li.Investigate(context.Background(), req); err != nil {
				t.Fatalf("Investigate: %v", err)
			}
			// Premise: the recall really did short-circuit the loop.
			if !delivered.Recalled {
				t.Fatalf("premise failed — the recall must short-circuit for this metric to be recorded: %+v", delivered)
			}
			got, ok := read("runlore_recall_tokens_spent_total")
			if !ok {
				t.Fatal("runlore_recall_tokens_spent_total was never recorded on a delivered recall short-circuit")
			}
			if got != tc.want {
				t.Fatalf("runlore_recall_tokens_spent_total = %d, want %d (the recall's observed input+output tokens, NOT the %d budget ceiling)",
					got, tc.want, li.MaxTokensPerInvestigation)
			}
			// The recall's spend is a strict subset of the per-investigation usage the
			// short-circuit already reports, so a dashboard can difference the two.
			if delivered.Usage.InputTokens+delivered.Usage.OutputTokens != int(tc.want) {
				t.Fatalf("delivered usage = in %d / out %d; the counter must be the same observed spend (%d)",
					delivered.Usage.InputTokens, delivered.Usage.OutputTokens, tc.want)
			}
		})
	}
}

// TestRecallTokensSpentNotRecordedOnFallThrough keeps the counter scoped to recalls
// that were actually DELIVERED: a recall the adversarial verify pass refutes falls
// through to a full investigation, and its tokens are already carried in that
// investigation's own usage totals. Counting them here too would double-count them
// and make the series stop meaning "what a short-circuit costs".
func TestRecallTokensSpentNotRecordedOnFallThrough(t *testing.T) {
	m, read := meteredInstruments(t)
	model := &scriptModel{responses: []providers.CompletionResponse{
		// verify the recalled entry → reject it entirely (falls through).
		{ToolCalls: []providers.ToolCall{{ID: "v1", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"reject","reason":"correlation only"}]}`}},
			Usage: providers.Usage{InputTokens: 900, OutputTokens: 60}},
		// the fall-through loop concludes on its own.
		{ToolCalls: []providers.ToolCall{{ID: "f1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"freshly investigated","confidence":0.8}]}`}},
			Usage: providers.Usage{InputTokens: 2000, OutputTokens: 100}},
		// …and verifies it.
		{ToolCalls: []providers.ToolCall{{ID: "v2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`}},
			Usage: providers.Usage{InputTokens: 300, OutputTokens: 20}},
	}}
	li := &LoopInvestigator{
		Model:                     model,
		Verify:                    true,
		MaxTokensPerInvestigation: 100_000,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:                   m,
		Recall: &Recall{
			MinScore: 2.0, Metrics: m,
			Catalog: fakeScored{hits: []catalog.ScoredEntry{{
				Entry: catalog.Entry{Title: "Stale entry", Description: "no longer applies", Path: "stale.md", Resource: "tooling/harbor"},
				Score: 5.0,
			}}},
		},
	}
	var delivered providers.Investigation
	li.OnComplete = func(inv providers.Investigation) { delivered = inv }
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got, ok := read("runlore_recall_tokens_spent_total"); ok {
		t.Fatalf("runlore_recall_tokens_spent_total = %d on a verify-rejected recall; it must only count delivered short-circuits", got)
	}
	// …and the other half of "not double-counted": the refuted recall's tokens are not
	// DROPPED either, they are carried by the full investigation that ran instead. Both
	// halves have to hold — skipping the counter would otherwise be indistinguishable
	// from losing the spend entirely, and the recall's 960 tokens were really paid.
	const (
		refutedRecall = 900 + 60   // the verify pass that refuted the recalled entry
		fullLoop      = 2000 + 100 // the fall-through loop's own submit_findings turn
		loopVerify    = 300 + 20   // …and the verify pass over its findings
	)
	if got, want := delivered.Usage.InputTokens+delivered.Usage.OutputTokens, refutedRecall+fullLoop+loopVerify; got != want {
		t.Fatalf("delivered usage = %d tokens, want %d — the refuted recall's %d tokens must still be billed to the full investigation that replaced it (in %d / out %d)",
			got, want, refutedRecall, delivered.Usage.InputTokens, delivered.Usage.OutputTokens)
	}
}
