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
