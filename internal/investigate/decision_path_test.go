// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestAffordRerankNeverGatesTheFreeDecider: the spend ceiling is a guard on LLM
// tokens, and the jev backend spends none (rankJev is unaccounted by design). Gating
// it on the LLM's request estimate skipped a call that costs nothing — and on the
// outcomeFallback path, whose second rank call runs with real prior spend, an
// over-budget incident then got neither the recall the free decider would have
// produced nor the investigation the ceiling was about to stop.
//
// The shadow backend still makes the LLM call, so it stays gated; a jev Backend with
// no Decider is the kill-switch fallback to the LLM, and stays gated too.
func TestAffordRerankNeverGatesTheFreeDecider(t *testing.T) {
	over := &recallSpend{afford: func(int) string { return "tokens_total" }}
	cands := []catalog.ScoredEntry{{Entry: catalog.Entry{Path: "a.md", Title: "A"}, Score: 1}}
	for _, tc := range []struct {
		name string
		rr   *Reranker
		want bool
	}{
		{"jev spends no tokens, so the ceiling cannot refuse it", &Reranker{Decider: &fakeDecider{}, Backend: "jev"}, true},
		{"shadow still pays for the LLM call", &Reranker{Decider: &fakeDecider{}, Backend: "shadow"}, false},
		{"the LLM default is gated", &Reranker{}, false},
		{"jev with no decider is the LLM fallback, and gated", &Reranker{Backend: "jev"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Recall{Rerank: tc.rr}
			if got := r.affordRerank(context.Background(), Request{Title: "t"}, cands, over); got != tc.want {
				t.Fatalf("affordRerank = %v, want %v", got, tc.want)
			}
		})
	}
}

// gatingModel answers only once the decider has been asked. With the shadow arm run
// AFTER the LLM returns, Complete can never proceed: the test's context deadline
// expires, rankLLM falls through, and the assertion below fails — which is the point.
type gatingModel struct {
	deciderAsked <-chan struct{}
	resp         providers.CompletionResponse
}

func (m *gatingModel) Complete(ctx context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	select {
	case <-m.deciderAsked:
		return m.resp, nil
	case <-ctx.Done():
		return providers.CompletionResponse{}, ctx.Err()
	}
}

// signallingDecider closes asked on its first call and answers from answers.
type signallingDecider struct {
	asked   chan struct{}
	once    sync.Once
	answers providers.Answers
}

func (d *signallingDecider) Decide(_ context.Context, _ string, _ []providers.Question) (providers.Answers, error) {
	d.once.Do(func() { close(d.asked) })
	return d.answers, nil
}

// TestRerankShadowAsksTheDeciderWhileTheLLMRuns pins that shadow mode costs the recall
// path max(LLM, decider) latency, not their sum. Shadow is sold as safe to enable, but
// with the decider asked only after the LLM answered, a slow or black-holed endpoint
// added up to the client timeout to every instant recall — twice per investigation on
// the outcomeFallback path — for a verdict the LLM had already given.
func TestRerankShadowAsksTheDeciderWhileTheLLMRuns(t *testing.T) {
	cands := []catalog.ScoredEntry{{Entry: catalog.Entry{Path: "a.md", Title: "A"}, Score: 1}}
	dec := &signallingDecider{
		asked:   make(chan struct{}),
		answers: providers.Answers{rerankQuestionID: {Choice: "a.md", Confidence: 0.9}},
	}
	rr := &Reranker{
		Model:     &gatingModel{deciderAsked: dec.asked, resp: rerankResp(`{"match":true,"entry_id":"a.md","confidence":0.95}`)},
		Decider:   dec,
		Backend:   "shadow",
		Threshold: 0.7, ThresholdJev: 0.7, K: 5,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, conf, ok := rr.rank(ctx, Request{Title: "t"}, cands, &recallSpend{})
	if !ok || got.Path != "a.md" || conf != 0.95 {
		t.Fatalf("the LLM never answered, so the decider was not asked until after it: ok=%v path=%s conf=%v",
			ok, got.Path, conf)
	}
}

// histogramPoints installs a real SDK meter provider behind a manual reader and returns
// the instrument set bound to it, plus a reader of one float64 histogram's data points
// by exported series name. Global provider, so one test at a time (no t.Parallel).
func histogramPoints(t *testing.T) (*telemetry.Metrics, func(series string) []metricdata.HistogramDataPoint[float64]) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func(series string) []metricdata.HistogramDataPoint[float64] {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != series {
					continue
				}
				h, ok := md.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("series %q is not a float64 histogram (%T)", series, md.Data)
				}
				return h.DataPoints
			}
		}
		return nil
	}
}

// TestDecisionConfidenceSeparatesNoneFromACandidate: the confidence histogram is what
// the promotion procedure reads a fire bar off, and it recorded the decider's
// confidence in "none of these" alongside its confidence in a named candidate. On a
// corpus where most incidents have no runbook, the histogram's mass then sits where
// the decider is sure there is nothing to recall — and a threshold set "from data"
// against it lands above every candidate answer, so in jev mode nothing fires. The
// rerank consumer records which kind of answer each sample is.
func TestDecisionConfidenceSeparatesNoneFromACandidate(t *testing.T) {
	m, points := histogramPoints(t)
	cands := []catalog.ScoredEntry{{Entry: catalog.Entry{Path: "a.md", Title: "A"}, Score: 1}}
	for _, a := range []providers.Answer{
		{Choice: rerankNoneOption, Confidence: 0.9},
		{Choice: "a.md", Confidence: 0.6},
	} {
		rr := &Reranker{Decider: &fakeDecider{answers: providers.Answers{rerankQuestionID: a}}, Backend: "jev", Metrics: m}
		if _, err := rr.decideRerank(context.Background(), Request{Title: "t"}, cands); err != nil {
			t.Fatalf("decideRerank: %v", err)
		}
	}
	got := map[string]float64{} // choice label -> recorded sum
	for _, dp := range points("runlore_decision_model_confidence") {
		choice, ok := dp.Attributes.Value(attribute.Key("choice"))
		if !ok {
			t.Fatalf("a rerank sample carries no `choice` label: %v", dp.Attributes.ToSlice())
		}
		got[choice.AsString()] += dp.Sum
	}
	if got["none"] != 0.9 || got["candidate"] != 0.6 {
		t.Fatalf("want none=0.9 and candidate=0.6 recorded separately, got %v", got)
	}
}
