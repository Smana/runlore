// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/embed"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// embeddingHybrid is a HybridSearcher shaped like the real one: it embeds the query
// through a REAL embed.Client (against a local endpoint that reports usage) and then
// returns fixed hits. Using the real client rather than a hand-charged fake is the
// point — it exercises the actual seam under test, from the /embeddings response's
// usage block through embed.UsageSink to the investigation's totals.
type embeddingHybrid struct {
	client *embed.Client
	hits   []catalog.ScoredEntry
	calls  atomic.Int64
}

func (h *embeddingHybrid) SearchHybrid(ctx context.Context, query string, _ int) ([]catalog.ScoredEntry, error) {
	h.calls.Add(1)
	if _, err := h.client.Embed(ctx, []string{query}); err != nil {
		return nil, err
	}
	return h.hits, nil
}

func (h *embeddingHybrid) HasVectors() bool { return true }

// newEmbeddingHybrid stands up an /embeddings endpoint that bills promptTokens per
// request and wires a hybrid searcher to it.
func newEmbeddingHybrid(t *testing.T, promptTokens int, hits []catalog.ScoredEntry) *embeddingHybrid {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0,0.0]}],` +
			`"usage":{"prompt_tokens":` + strconv.Itoa(promptTokens) + `}}`))
	}))
	t.Cleanup(srv.Close)
	return &embeddingHybrid{client: embed.New(srv.URL, "text-embed", ""), hits: hits}
}

// offTargetHit is a candidate that will NOT structurally agree with okReq()
// (apps/web), so recall cannot fire and the loop runs — which is what puts BOTH
// hybrid queries on the path: the recall lookup and the near-miss lookup that follows
// a non-fire.
func offTargetHit() []catalog.ScoredEntry {
	return []catalog.ScoredEntry{{
		Entry: catalog.Entry{Title: "Unrelated", Path: "other.md", Resource: "infra/etcd"},
		Score: 0.9,
	}}
}

// TestHybridRecallQueryEmbedCountsTowardTheInvestigation closes the half of the
// embeddings gap that an investigation-scoped ceiling CAN cover.
//
// The /embeddings endpoint is a paid API called once per hybrid-recall query, from
// inside an investigation that has spend ceilings — but the tokens never reached that
// investigation's totals, because the call goes out through catalog.Embedder, which
// returns vectors and nothing else. So an embed-heavy recall was free by construction:
// the more queries it made, the more of the run's real spend the ceiling could not
// see.
//
// Both queries on the non-fire path are charged: the recall lookup, and the near-miss
// lookup that follows it. Missing the second would leave the commonest path — recall
// consulted, recall did not fire — undercounted by half.
func TestHybridRecallQueryEmbedCountsTowardTheInvestigation(t *testing.T) {
	const perQuery = 500
	hyb := newEmbeddingHybrid(t, perQuery, offTargetHit())
	loop := &mixedModel{steps: []mixedStep{{resp: providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"oom","confidence":0.8}]}`}},
		Usage: providers.Usage{InputTokens: 1_000, OutputTokens: 50},
	}}}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      loop,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		Recall:     &Recall{Catalog: fakeScored{}, Hybrid: hyb, HybridMinScore: 0.8, HybridMarginGap: 0.1},
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if n := hyb.calls.Load(); n != 2 {
		t.Fatalf("expected 2 hybrid queries (recall lookup + near-miss), got %d — the test no "+
			"longer exercises the path it claims to", n)
	}
	if want := 1_000 + 2*perQuery; got.Usage.InputTokens != want {
		t.Fatalf("the hybrid-recall query embeds must count toward the investigation's tokens: "+
			"got %d, want %d (1000 loop + 2 x %d embed). The embeddings endpoint bills these and "+
			"reports them; a per-investigation ceiling that cannot see them bounds less than it says.",
			got.Usage.InputTokens, want, perQuery)
	}
	// ModelCalls stays a count of COMPLETIONS. An embedding is not a turn, and this is
	// the number an operator reads as "how deep did the loop go"; the embeddings
	// endpoint's own call volume is already published under provider="embed".
	if got.Usage.ModelCalls != 1 {
		t.Fatalf("ModelCalls must stay a completion count, got %d", got.Usage.ModelCalls)
	}
}

// TestEmbedSpendTripsTheCumulativeTokenCeiling proves the folded-in embed spend
// reaches the ladder, and — the question this answers for the metric's label set —
// that it needs NO new `reason` value: reason names WHICH CEILING an operator has to
// raise, not which call pushed it over, and the ceiling here is
// max_tokens_per_investigation exactly as it would be for a loop-only overrun.
//
// The numbers are scaled so ONE embed charge is material against a deliberately small
// ceiling; a shipped-default 400 000 ceiling would need hundreds of query embeds to
// move the ladder, which is not a unit test. The arithmetic is what is under test.
func TestEmbedSpendTripsTheCumulativeTokenCeiling(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// perQuery x 2 queries = 12 000 charged before the loop's first request. Each loop
	// call reports 7 000 input (under the 7 500 the 30 000 ceiling derives for ONE
	// request, so the per-request arm cannot be what fires) and the next request
	// projects another 7 000 on top.
	//   counting the embeds:  12 000 + 2 calls (14 020) + 7 000 = 33 020  → over at call 2
	//   ignoring them:                 4 calls (28 040) + 7 000 = 35 040  → over at call 4
	const (
		perQuery = 6_000
		ceiling  = 30_000
	)
	hyb := newEmbeddingHybrid(t, perQuery, offTargetHit())
	model := &reportingRunawayModel{inputTokens: 7_000}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: ceiling,
		Metrics:                   telemetry.NewMetrics(),
		Recall:                    &Recall{Catalog: fakeScored{}, Hybrid: hyb, HybridMinScore: 0.8, HybridMarginGap: 0.1},
		OnComplete:                func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.calls != 3 {
		t.Fatalf("the ladder must engage after 2 calls once the %d embed tokens are counted "+
			"(2 free calls, 1 nudged, then the kill = 3); got %d calls. Uncounted, this run gets "+
			"5 calls against the same ceiling.", 2*perQuery, model.calls)
	}
	if !mentions(got.Unresolved, "budget") {
		t.Fatalf("the hard-kill result must name the budget it hit; got: %v", got.Unresolved)
	}
	body := scrapeMetrics(t, h)
	if !strings.Contains(body, `reason="tokens_total"`) {
		t.Fatalf("an embed-driven overrun must trip the EXISTING cumulative-token reason — the "+
			"label says which ceiling to raise, not which call spent it:\n%s", body)
	}
	if strings.Contains(body, `reason="embed"`) {
		t.Fatalf("there must be no per-call-site reason value; reason names the ceiling:\n%s", body)
	}
}

// TestEmbedTokensAreCountedButNotPriced pins the honest half of the decision. There is
// no `model.embeddings.pricing`, so RunLore has no rate for an embedding token. The
// tokens are real and provider-reported, so they belong in the token total and under
// the token ceiling; the dollar figure is NOT derivable, so pricing them at the main
// or verify model's rate — the only rates in hand, both wrong by an order of
// magnitude — would fabricate a number the operator reads as measured.
//
// So the cost ceiling deliberately does not cover them, and errs LOW, which is the
// direction the projection already errs and the direction the page states.
func TestEmbedTokensAreCountedButNotPriced(t *testing.T) {
	li := &LoopInvestigator{Pricing: &Pricing{InputUSDPerMTok: 1_000_000}} // $1 per token
	loop := providers.UsageTotals{ModelCalls: 1, InputTokens: 3}
	embedded := providers.UsageTotals{InputTokens: 7}

	base := li.aggregateUsage(loop, providers.UsageTotals{}, providers.UsageTotals{})
	withEmbed := li.aggregateUsage(loop, providers.UsageTotals{}, embedded)

	if withEmbed.InputTokens != 10 {
		t.Fatalf("embedding tokens must be in the token total: got %d, want 10", withEmbed.InputTokens)
	}
	if withEmbed.CostUSD != base.CostUSD {
		t.Fatalf("embedding tokens must not be priced at the completion model's rate: cost moved "+
			"from %v to %v. RunLore has no embeddings rate, and inventing one makes the reported "+
			"figure look measured when it is a guess.", base.CostUSD, withEmbed.CostUSD)
	}
	if !withEmbed.Priced {
		t.Fatal("Priced must still reflect that model.pricing is configured")
	}
}
