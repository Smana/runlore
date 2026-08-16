// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"sync"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// runawayEvalModel never winds down: it calls the same benign tool on every free
// turn and only submits findings when ToolChoice forces it. Each completion reports
// a large provider usage, so an investigation's accumulated spend climbs fast.
// It counts its own calls, which is what the ceiling is asserted on.
type runawayEvalModel struct {
	mu    sync.Mutex
	calls int
}

func (m *runawayEvalModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	usage := providers.Usage{InputTokens: 30_000, OutputTokens: 10}
	if req.ToolChoice == "submit_findings" {
		// Refuse to conclude even when nudged, so the ladder must reach its hard-kill
		// rung: this pins the ceiling, not the model's good manners.
		return providers.CompletionResponse{Usage: usage, Text: "still investigating"}, nil
	}
	return providers.CompletionResponse{
		Usage:     usage,
		ToolCalls: []providers.ToolCall{{ID: "t", Name: "what_changed", Args: `{}`}},
	}, nil
}

func (m *runawayEvalModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// runawayCase is the fixture every arm below replays: one benign tool, a model that
// will call it forever.
func runawayCase() Case {
	return Case{
		Name:     "runaway",
		Prompt:   "pods not starting",
		Tools:    map[string]string{"what_changed": "image tag changed"},
		Expected: Expected{MustContain: []string{"image"}},
	}
}

// tokenCeiling is small enough that a 30_010-token completion crosses it on the
// first call, so an arm that forwards it stops within a couple of calls while an
// arm that drops it runs the loop's full default step budget (20).
const tokenCeiling = 20_000

// unboundedCalls is the number of completions a runaway replay makes when NO
// ceiling reaches the loop — the loop's default MaxSteps. Asserting against it is
// what makes the bounded assertions below meaningful rather than vacuous.
const unboundedCalls = 20

// TestEvalRunnersForwardTheirSpendCeiling pins that each of the three eval runners
// actually HANDS its Spend to the LoopInvestigator it builds — not merely that it
// has a field of that name.
//
// The module-wide guard in internal/investigate is structural: it reads field names
// off the composite literal and cannot tell `MaxTokensPerInvestigation: r.Spend.Max…`
// from `MaxTokensPerInvestigation: 0`. That is the right division of labour (the
// guard's job is that no NEW site is built without the fields), but it leaves the
// forwarding itself unpinned. This test closes that: a runaway model against a tight
// ceiling must be stopped, and the same model with a zero Spend must not be — so a
// forwarding that silently drops the value fails here.
func TestEvalRunnersForwardTheirSpendCeiling(t *testing.T) {
	spend := Spend{MaxTokensPerInvestigation: tokenCeiling}

	t.Run("replay", func(t *testing.T) {
		bounded := &runawayEvalModel{}
		r := &Runner{Model: bounded, Log: discardLog(), Spend: spend}
		r.runOne(context.Background(), runawayCase())

		free := &runawayEvalModel{}
		u := &Runner{Model: free, Log: discardLog()}
		u.runOne(context.Background(), runawayCase())

		assertCeilingBit(t, "replay", bounded.count(), free.count())
	})

	t.Run("compare", func(t *testing.T) {
		bounded := &runawayEvalModel{}
		cr := &ComparisonRunner{Model: bounded, Log: discardLog(), Spend: spend}
		cr.runOnce(context.Background(), runawayCase())

		free := &runawayEvalModel{}
		u := &ComparisonRunner{Model: free, Log: discardLog()}
		u.runOnce(context.Background(), runawayCase())

		assertCeilingBit(t, "compare", bounded.count(), free.count())
	})

	t.Run("live", func(t *testing.T) {
		scn := Scenario{ID: "runaway"}
		scn.Trigger.Symptom = "pods not starting"

		bounded := &runawayEvalModel{}
		lr := &LiveRunner{
			Model:     bounded,
			BaseTools: []investigate.Tool{fakeTool{name: "what_changed", out: "image tag changed"}},
			Steps:     &recordingSteps{}, Log: discardLog(), N: 1, Spend: spend,
		}
		lr.RunScenario(context.Background(), scn)

		free := &runawayEvalModel{}
		u := &LiveRunner{
			Model:     free,
			BaseTools: []investigate.Tool{fakeTool{name: "what_changed", out: "image tag changed"}},
			Steps:     &recordingSteps{}, Log: discardLog(), N: 1,
		}
		u.RunScenario(context.Background(), scn)

		assertCeilingBit(t, "live", bounded.count(), free.count())
	})
}

// assertCeilingBit checks both halves of the property: the ceiling stopped the
// bounded arm early, AND the unbounded arm really did run away (without which the
// first half could pass for any reason at all).
func assertCeilingBit(t *testing.T, arm string, bounded, free int) {
	t.Helper()
	if free < unboundedCalls {
		t.Fatalf("%s: the no-ceiling control made %d completions, want the loop's full %d step budget — "+
			"the fixture no longer runs away, so the bounded assertion below proves nothing",
			arm, free, unboundedCalls)
	}
	if bounded >= free {
		t.Errorf("%s: a runner with Spend.MaxTokensPerInvestigation=%d made %d completions, "+
			"the same as the unbounded control (%d) — the ceiling is not reaching the loop",
			arm, tokenCeiling, bounded, free)
	}
}

// TestEvalSpendCarriesPricingToTheLoop pins the other half of Spend: the cost
// ceiling is INERT without Pricing (investigate.overCostBudget refuses to compare an
// unpriced total), so a runner that forwarded MaxCostPerInvestigation but dropped
// Pricing would present a dollar limit that can never fire. The unpriced arm is the
// control that makes the priced arm's stop attributable to the cost ceiling and not
// to something else in the loop.
func TestEvalSpendCarriesPricingToTheLoop(t *testing.T) {
	// $10/Mtok input: one 30_000-token completion costs $0.30, so a $0.10 ceiling is
	// crossed by the first call.
	pricing := &investigate.Pricing{InputUSDPerMTok: 10}

	priced := &runawayEvalModel{}
	r := &Runner{Model: priced, Log: discardLog(), Spend: Spend{
		MaxCostPerInvestigation: 0.10, Pricing: pricing,
	}}
	r.runOne(context.Background(), runawayCase())

	unpriced := &runawayEvalModel{}
	u := &Runner{Model: unpriced, Log: discardLog(), Spend: Spend{
		MaxCostPerInvestigation: 0.10, // no Pricing: nothing to compare, ceiling inert
	}}
	u.runOne(context.Background(), runawayCase())

	if unpriced.count() < unboundedCalls {
		t.Fatalf("the unpriced control made %d completions, want %d: without Pricing the cost "+
			"ceiling cannot fire, so this arm must run away", unpriced.count(), unboundedCalls)
	}
	if priced.count() >= unpriced.count() {
		t.Errorf("a $%.2f cost ceiling with Pricing made %d completions, the same as the unpriced "+
			"control (%d) — Spend.Pricing is not reaching the loop, so the cost ceiling is inert",
			0.10, priced.count(), unpriced.count())
	}
}
