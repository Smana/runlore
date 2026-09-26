// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// gatingModel signals entered when Complete is called and answers only once the
// decider has been asked. With rendezvousDecider this pins that the two shadow arms
// overlap: under either serial order one side waits for a signal the other has not
// yet been reached to give, the context deadline expires, rank falls through, and
// the assertion catches it.
type gatingModel struct {
	entered chan struct{}
	asked   <-chan struct{}
	resp    providers.CompletionResponse
}

func (m *gatingModel) Complete(ctx context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	close(m.entered)
	select {
	case <-m.asked:
		return m.resp, nil
	case <-ctx.Done():
		return providers.CompletionResponse{}, ctx.Err()
	}
}

// rendezvousDecider is a fakeDecider that waits until the model has been entered
// before answering, and signals asked once it has.
type rendezvousDecider struct {
	fakeDecider
	entered <-chan struct{}
	asked   chan struct{}
}

func (d *rendezvousDecider) Decide(ctx context.Context, state string, qs []providers.Question) (providers.Answers, error) {
	select {
	case <-d.entered:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	close(d.asked)
	return d.fakeDecider.Decide(ctx, state, qs)
}

// TestRerankShadowAsksTheDeciderWhileTheLLMRuns: shadow mode costs the recall path
// max(LLM, decider), not their sum (see rankShadow).
func TestRerankShadowAsksTheDeciderWhileTheLLMRuns(t *testing.T) {
	entered, asked := make(chan struct{}), make(chan struct{})
	dec := &rendezvousDecider{
		fakeDecider: fakeDecider{answers: providers.Answers{rerankQuestionID: {Choice: "a.md", Confidence: 0.9}}},
		entered:     entered,
		asked:       asked,
	}
	rr := &Reranker{
		Model:     &gatingModel{entered: entered, asked: asked, resp: rerankResp(`{"match":true,"entry_id":"a.md","confidence":0.95}`)},
		Decider:   dec,
		Backend:   "shadow",
		Threshold: 0.7, ThresholdJev: 0.7,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, conf, ok := rr.rank(ctx, Request{Title: "t"}, []catalog.ScoredEntry{webHit("a.md", 1)}, &recallSpend{})
	if !ok || got.Path != "a.md" || conf != 0.95 {
		t.Fatalf("the arms did not overlap (one waited on the other): ok=%v path=%s conf=%v", ok, got.Path, conf)
	}
	if dec.calls != 1 {
		t.Fatalf("the shadow arm must be asked exactly once, got %d", dec.calls)
	}
}

// TestDecisionConfidenceSeparatesNoneFromACandidate: the rerank consumer labels each
// confidence sample by choice (see decideRerank), so a bar is read off the candidate
// answers alone.
func TestDecisionConfidenceSeparatesNoneFromACandidate(t *testing.T) {
	m, reader := installMeterReader(t)
	cands := []catalog.ScoredEntry{webHit("a.md", 1)}
	for _, a := range []providers.Answer{
		{Choice: rerankNoneOption, Confidence: 0.9},
		{Choice: "a.md", Confidence: 0.6},
	} {
		rr := &Reranker{Decider: &fakeDecider{answers: providers.Answers{rerankQuestionID: a}}, Backend: "jev", Metrics: m}
		if _, err := rr.decideRerank(context.Background(), Request{Title: "t"}, cands); err != nil {
			t.Fatalf("decideRerank: %v", err)
		}
	}
	md, ok := findSeries(t, reader, "runlore_decision_model_confidence")
	if !ok {
		t.Fatal("no confidence sample was recorded")
	}
	h, ok := md.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("runlore_decision_model_confidence is not a float64 histogram (%T)", md.Data)
	}
	got := map[string]metricdata.HistogramDataPoint[float64]{}
	for _, dp := range h.DataPoints {
		choice, ok := dp.Attributes.Value(attribute.Key("choice"))
		if !ok {
			t.Fatalf("a rerank sample carries no `choice` label: %v", dp.Attributes.ToSlice())
		}
		got[choice.AsString()] = dp
	}
	for choice, want := range map[string]float64{"none": 0.9, "candidate": 0.6} {
		dp, ok := got[choice]
		if !ok || dp.Count != 1 || math.Abs(dp.Sum-want) > 1e-9 {
			t.Errorf("want one %s sample at %v, got count=%d sum=%v", choice, want, dp.Count, dp.Sum)
		}
	}
}
