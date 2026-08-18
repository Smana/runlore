// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fixedUsageModel reports the same usage on every completion and counts its calls.
type fixedUsageModel struct {
	usage providers.Usage
	calls int
}

func (m *fixedUsageModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{Usage: m.usage, Text: "ok"}, nil
}

// TestCampaignBudgetStopsSpendingOnceTheCeilingIsCrossed pins the bound the
// per-investigation ceilings cannot express.
//
// investigation.max_tokens_per_investigation bounds ONE case. A campaign is
// cases x n investigations plus a judge call each, run unattended, so the
// per-investigation ceiling multiplies by the corpus size instead of capping the run
// — a 30-case corpus at n=10 under a 100k per-case ceiling authorises 30 million
// tokens and nothing says no. This ceiling is the run-level one.
func TestCampaignBudgetStopsSpendingOnceTheCeilingIsCrossed(t *testing.T) {
	m := &fixedUsageModel{usage: providers.Usage{InputTokens: 400, OutputTokens: 100}}
	b := &CampaignBudget{MaxTokens: 1000}
	wrapped := b.Wrap(m)

	// Two calls spend exactly the ceiling (2 x 500); neither may be refused, since
	// the ceiling is a ceiling and not a budget the run must stay strictly under.
	for i := 1; i <= 2; i++ {
		if _, err := wrapped.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
			t.Fatalf("call %d: spend is still within the %d-token ceiling, want no error, got %v", i, b.MaxTokens, err)
		}
	}
	if b.Exceeded() {
		t.Fatalf("spend of exactly the ceiling (%d) must not trip it", b.SpentTokens())
	}

	// The third crosses it. A ceiling checked BEFORE the call cannot know a call's
	// size in advance, so the crossing call itself completes and the ones after it
	// are refused — the overshoot is bounded by one completion, and stated.
	if _, err := wrapped.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
		t.Fatalf("the crossing call itself must complete (its size is unknowable in advance), got %v", err)
	}
	if !b.Exceeded() {
		t.Fatalf("spend %d is over the %d-token ceiling but the budget did not trip", b.SpentTokens(), b.MaxTokens)
	}

	callsBefore := m.calls
	_, err := wrapped.Complete(context.Background(), providers.CompletionRequest{})
	if !errors.Is(err, ErrCampaignBudgetExceeded) {
		t.Fatalf("after the ceiling is crossed every further completion must be refused with "+
			"ErrCampaignBudgetExceeded, got err=%v", err)
	}
	if m.calls != callsBefore {
		t.Errorf("the refused completion still reached the provider (%d → %d calls): the campaign "+
			"ceiling must stop spend, not merely report it", callsBefore, m.calls)
	}
}

// TestCampaignBudgetWithoutACeilingOnlyCounts pins that the budget is opt-in: with
// MaxTokens unset it never refuses anything, but it still accumulates — which is how
// `lore eval` reports what a campaign spent across models the per-case counters do
// not see (notably the judge).
func TestCampaignBudgetWithoutACeilingOnlyCounts(t *testing.T) {
	m := &fixedUsageModel{usage: providers.Usage{InputTokens: 900_000, OutputTokens: 100_000}}
	b := &CampaignBudget{}
	wrapped := b.Wrap(m)
	for i := 0; i < 5; i++ {
		if _, err := wrapped.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
			t.Fatalf("no ceiling is configured; nothing may be refused, got %v", err)
		}
	}
	if b.Exceeded() {
		t.Error("a budget with no ceiling can never be exceeded")
	}
	if got := b.SpentTokens(); got != 5_000_000 {
		t.Errorf("spend accounting: got %d tokens, want 5000000", got)
	}
}

// TestCampaignBudgetAccumulatesAcrossEveryWrappedModel pins that ONE budget spans
// the whole run. An eval drives several distinct models — the investigation model,
// the judge, and under --compare one per entry — and a ceiling that each of them
// gets a private copy of is not a run-level ceiling at all, it is the per-case bug
// again at a different scale.
func TestCampaignBudgetAccumulatesAcrossEveryWrappedModel(t *testing.T) {
	investigation := &fixedUsageModel{usage: providers.Usage{InputTokens: 600}}
	judge := &fixedUsageModel{usage: providers.Usage{InputTokens: 600}}
	b := &CampaignBudget{MaxTokens: 1000}
	wi, wj := b.Wrap(investigation), b.Wrap(judge)

	if _, err := wi.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
		t.Fatalf("first call is within the ceiling: %v", err)
	}
	if _, err := wj.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
		t.Fatalf("the judge's first call crosses the ceiling and must itself complete: %v", err)
	}
	if !b.Exceeded() {
		t.Fatalf("600 (investigation) + 600 (judge) = %d exceeds the %d-token ceiling, "+
			"but the budget did not trip — the models are not sharing one budget",
			b.SpentTokens(), b.MaxTokens)
	}
	if _, err := wi.Complete(context.Background(), providers.CompletionRequest{}); !errors.Is(err, ErrCampaignBudgetExceeded) {
		t.Errorf("the judge's spend must stop the investigation model too, got %v", err)
	}
}

// TestCampaignBudgetFiresOnExceededOnce pins the halt signal `lore eval` hangs the
// campaign's early stop on: it must fire exactly once, on the crossing, so a
// cancel-the-campaign callback is not re-entered for every refused call afterwards.
func TestCampaignBudgetFiresOnExceededOnce(t *testing.T) {
	fired := 0
	b := &CampaignBudget{MaxTokens: 100, OnExceeded: func() { fired++ }}
	wrapped := b.Wrap(&fixedUsageModel{usage: providers.Usage{InputTokens: 500}})
	for i := 0; i < 4; i++ {
		_, _ = wrapped.Complete(context.Background(), providers.CompletionRequest{})
	}
	if fired != 1 {
		t.Errorf("OnExceeded fired %d times, want exactly 1 (on the crossing)", fired)
	}
}

// TestCampaignLoopsStopOnACancelledContext pins the other half of the halt: the
// budget's refusal stops the SPEND, but on its own it leaves every remaining case
// still being walked and reported as a broken run. `lore eval` cancels the campaign
// context when the ceiling trips, so the case loops must honour it — a halted
// campaign should report the cases it actually ran, not a wall of identical errors.
// It is also what makes Ctrl-C stop a nightly campaign promptly.
func TestCampaignLoopsStopOnACancelledContext(t *testing.T) {
	cases := []Case{runawayCase(), runawayCase(), runawayCase()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("replay", func(t *testing.T) {
		r := &Runner{Model: &runawayEvalModel{}, Log: discardLog()}
		if got := r.RunN(ctx, cases, 1); len(got.Aggregates) != 0 {
			t.Errorf("RunN walked %d cases under a cancelled context, want 0", len(got.Aggregates))
		}
	})
	t.Run("compare", func(t *testing.T) {
		cr := &ComparisonRunner{Model: &runawayEvalModel{}, Log: discardLog()}
		if got := cr.RunCases(ctx, cases, 1); len(got) != 0 {
			t.Errorf("RunCases walked %d cases under a cancelled context, want 0", len(got))
		}
	})
}

// TestNilCampaignBudgetWrapsToThePlainModel pins the zero-plumbing path: callers
// that never build a budget hand the real provider straight through, so nothing in
// the eval paths needs a nil check of its own.
func TestNilCampaignBudgetWrapsToThePlainModel(t *testing.T) {
	m := &fixedUsageModel{}
	var b *CampaignBudget
	if got := b.Wrap(m); got != providers.ModelProvider(m) {
		t.Errorf("a nil budget must return the model unchanged, got %T", got)
	}
}

// flappingModel fails every completion but reports the usage the provider billed
// before it broke — exactly the shape every model client returns on its error path
// (providers.CompletionResponse.CostOnly). A provider that 500s after reading a
// 120k-token prompt bills for that prompt; the response body is what is missing, not
// the invoice.
type flappingModel struct {
	usage providers.Usage
	calls int
}

func (m *flappingModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{Usage: m.usage, Text: "half a"}.CostOnly(),
		errors.New("upstream 500 mid-stream")
}

// campaignCases is the corpus size the reproduction below walks, and perCaseTokens
// what one case costs a flapping provider. 30 x 126_000 = 3_780_000 tokens against a
// 50_000-token ceiling.
const (
	campaignCases  = 30
	perCaseTokens  = 126_000
	reproCeiling   = 50_000
	reproUncounted = campaignCases * perCaseTokens
)

// TestCampaignBudgetCountsAFailedCallThatWasStillBilled pins the house rule the
// campaign ceiling was the last place in the repo not to follow: a completion that
// FAILED still cost whatever the provider reported before it broke.
//
// internal/investigate/loop.go accumulates resp.Usage BEFORE its failure branch for
// exactly this reason, and providers.CompletionResponse.CostOnly exists solely to
// carry a failed call's usage back to a caller. The budget returned early on error
// instead — so against a provider flapping at 126_000 tokens a call, a 30-case
// campaign under a 50_000-token ceiling billed 3_780_000 tokens, ran every case,
// never tripped, and never fired the halt.
func TestCampaignBudgetCountsAFailedCallThatWasStillBilled(t *testing.T) {
	m := &flappingModel{usage: providers.Usage{InputTokens: 120_000, OutputTokens: 6_000}}
	fired := 0
	b := &CampaignBudget{MaxTokens: reproCeiling, OnExceeded: func() { fired++ }}
	wrapped := b.Wrap(m)

	ran := 0
	for i := 0; i < campaignCases; i++ {
		_, err := wrapped.Complete(context.Background(), providers.CompletionRequest{})
		if errors.Is(err, ErrCampaignBudgetExceeded) {
			break // the campaign stopped paying, which is the whole point
		}
		ran++
	}

	if !b.Exceeded() {
		t.Errorf("after %d failed-but-billed calls of %d tokens the budget reports spent=%d and "+
			"Exceeded=false against a %d-token ceiling: a flapping provider can bill %d tokens "+
			"without the campaign ceiling ever seeing one of them",
			ran, perCaseTokens, b.SpentTokens(), b.MaxTokens, reproUncounted)
	}
	if fired != 1 {
		t.Errorf("OnExceeded fired %d times, want exactly 1 — `lore eval` hangs the campaign's "+
			"early stop on it, so a ceiling crossed by failures halts nothing", fired)
	}
	if ran >= campaignCases {
		t.Errorf("all %d cases ran under a %d-token ceiling; the crossing call is the last one "+
			"allowed, so the run must stop within a couple of calls", campaignCases, reproCeiling)
	}
	if got := b.SpentTokens(); got != ran*perCaseTokens {
		t.Errorf("spend accounting: got %d tokens over %d billed calls, want %d",
			got, ran, ran*perCaseTokens)
	}
}

// TestCampaignBudgetDoesNotInventSpendItCannotSee is the other direction: accounting
// a failed call must not become accounting things that were never billed.
//
//   - A completion refused by the budget itself never reached the provider, so it adds
//     nothing — the total must not grow after the ceiling trips.
//   - A failure BEFORE the provider reported anything (dial error, context cancelled)
//     carries zero usage. That is "unknown", not a licence to charge a guess: it adds
//     no tokens, exactly as investigate.addUsage treats it.
func TestCampaignBudgetDoesNotInventSpendItCannotSee(t *testing.T) {
	m := &flappingModel{usage: providers.Usage{InputTokens: 60_000}}
	b := &CampaignBudget{MaxTokens: reproCeiling}
	wrapped := b.Wrap(m)

	if _, err := wrapped.Complete(context.Background(), providers.CompletionRequest{}); err == nil {
		t.Fatal("the fixture must fail; it is the failure path under test")
	}
	crossedAt, callsAt := b.SpentTokens(), m.calls
	for i := 0; i < 5; i++ {
		if _, err := wrapped.Complete(context.Background(), providers.CompletionRequest{}); !errors.Is(err, ErrCampaignBudgetExceeded) {
			t.Fatalf("call %d after the crossing: want ErrCampaignBudgetExceeded, got %v", i, err)
		}
	}
	if got := b.SpentTokens(); got != crossedAt {
		t.Errorf("spend grew %d → %d across 5 REFUSED completions: a call the budget itself "+
			"turned back never reached the provider and was never billed", crossedAt, got)
	}
	if m.calls != callsAt {
		t.Errorf("a refused completion reached the provider (%d → %d calls)", callsAt, m.calls)
	}

	// A failure that reported no usage at all: counted as an attempt, charged nothing.
	silent := &flappingModel{}
	nb := &CampaignBudget{}
	sw := nb.Wrap(silent)
	if _, err := sw.Complete(context.Background(), providers.CompletionRequest{}); err == nil {
		t.Fatal("the silent fixture must fail too")
	}
	if got := nb.SpentTokens(); got != 0 {
		t.Errorf("a failure that reported no usage added %d tokens: zero usage is UNKNOWN, "+
			"and a ceiling that invents a figure for it is no more honest than one that drops it", got)
	}
}
