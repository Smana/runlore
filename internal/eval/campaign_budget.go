// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"sync"

	"github.com/Smana/runlore/internal/providers"
)

// ErrCampaignBudgetExceeded is returned by every completion attempted after an eval
// campaign's token ceiling has been crossed. It is a distinct sentinel so a halted
// run reads as "the operator's ceiling stopped this", not as a provider outage.
var ErrCampaignBudgetExceeded = errors.New("eval campaign token ceiling exceeded (--max-total-tokens)")

// CampaignBudget bounds the TOTAL provider-reported token spend of ONE `lore eval`
// invocation, across every model that invocation drives.
//
// It exists because Spend — the per-investigation ceiling set — bounds a case and
// nothing else. A campaign is cases x n investigations plus a judge call each, run
// unattended, so those ceilings MULTIPLY by the corpus size rather than capping the
// run: a 30-case corpus at n=10 under a 100k per-case ceiling authorises 30 million
// tokens, and before this nothing said no.
//
// WHY TOKENS AND NOT DOLLARS. A campaign routinely drives several DIFFERENT models
// at once — the investigation model, a separate (usually stronger) judge, and under
// --compare one model per entry, each with its own rate card. A single USD ceiling
// would have to price all of them from one rate card and would therefore misreport
// exactly the multi-model run it most needs to bound. A token is provider-neutral
// (the same argument config.Investigation makes for max_tokens_per_investigation
// carrying a default while max_cost_per_investigation cannot), so one number means
// one thing across every model in the run. Cost is still REPORTED where model.pricing
// exists; it is just not what the run-level ceiling is denominated in.
//
// WHAT IT COVERS: every completion made through a model the caller passed to Wrap.
// WHAT IT DOES NOT COVER: any model the caller forgot to Wrap, embeddings (a catalog
// fixture's embed calls do not go through a ModelProvider), and the wall-clock and
// cluster-mutating cost of a --live campaign's setup/teardown shell steps, which
// spend no tokens at all. It also cannot be a strict cap: a completion's size is
// unknowable before it is made, so the crossing call itself always completes and the
// overshoot is bounded by one completion, not zero.
type CampaignBudget struct {
	// MaxTokens is the ceiling on the campaign's accumulated input+output tokens.
	// <= 0 ⇒ no ceiling: the budget still ACCUMULATES (so the run can report what it
	// spent across models the per-case counters never see, notably the judge) but
	// refuses nothing.
	MaxTokens int
	// OnExceeded, when set, fires exactly once, on the crossing. `lore eval` hangs
	// the campaign's early stop on it — cancelling the campaign context so the case
	// loops break instead of grinding every remaining case into the same refusal.
	OnExceeded func()

	mu       sync.Mutex
	spent    providers.Usage
	exceeded bool
}

// Wrap returns m guarded by this budget. A nil budget returns m unchanged, so no
// caller needs its own nil check.
func (b *CampaignBudget) Wrap(m providers.ModelProvider) providers.ModelProvider {
	if b == nil {
		return m
	}
	return &budgetedModel{budget: b, inner: m}
}

// Spent returns the usage accumulated across every wrapped model so far.
func (b *CampaignBudget) Spent() providers.Usage {
	if b == nil {
		return providers.Usage{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// SpentTokens is the figure MaxTokens is compared against: input (which INCLUDES
// cached — see providers.Usage) plus output, the same arithmetic
// investigate.spentTokens and recall_tokens_spent_total use, so "tokens spent" means
// one thing across RunLore.
func (b *CampaignBudget) SpentTokens() int {
	u := b.Spent()
	return u.InputTokens + u.OutputTokens
}

// Exceeded reports whether the ceiling has been crossed.
func (b *CampaignBudget) Exceeded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exceeded
}

// observe folds one completion's usage into the total and reports whether this call
// is the one that crossed the ceiling.
func (b *CampaignBudget) observe(u providers.Usage) (crossed bool) {
	b.mu.Lock()
	b.spent.InputTokens += u.InputTokens
	b.spent.OutputTokens += u.OutputTokens
	b.spent.CachedInputTokens += u.CachedInputTokens
	b.spent.CacheWriteTokens += u.CacheWriteTokens
	if b.MaxTokens > 0 && !b.exceeded &&
		b.spent.InputTokens+b.spent.OutputTokens > b.MaxTokens {
		b.exceeded, crossed = true, true
	}
	b.mu.Unlock()
	return crossed
}

// budgetedModel is the chokepoint: it refuses a completion once the campaign ceiling
// has been crossed, and accumulates the ones it lets through.
type budgetedModel struct {
	budget *CampaignBudget
	inner  providers.ModelProvider
}

func (m *budgetedModel) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	if m.budget.Exceeded() {
		return providers.CompletionResponse{}, ErrCampaignBudgetExceeded
	}
	resp, err := m.inner.Complete(ctx, req)
	if err != nil {
		return resp, err // a failed call billed nothing we can account for
	}
	if m.budget.observe(resp.Usage) && m.budget.OnExceeded != nil {
		m.budget.OnExceeded()
	}
	return resp, nil
}
