// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"errors"
	"testing"
)

// billedFailureModel is what every model client in this repo returns when a
// completion breaks after the provider has already reported usage: the response
// reduced to what the exchange COST (CompletionResponse.CostOnly), alongside the
// error. The reply is missing; the invoice is not.
type billedFailureModel struct {
	usage Usage
	err   error
}

func (m *billedFailureModel) Complete(context.Context, CompletionRequest) (CompletionResponse, error) {
	return CompletionResponse{Usage: m.usage, Text: "half an ans"}.CostOnly(), m.err
}

// okModel succeeds and reports usage.
type okModel struct{ usage Usage }

func (m *okModel) Complete(context.Context, CompletionRequest) (CompletionResponse, error) {
	return CompletionResponse{Usage: m.usage, Text: "ok"}, nil
}

// TestCountingModelCountsFailedCallsThatWereBilled pins the reason CostOnly exists.
//
// CountingModel is what every ONE-SHOT command reports its spend from — `lore eval`,
// `lore eval --compare`, `lore kb import`, `lore validate-kb` — and in a CLI process
// that line is the ONLY place the operator can see what the command cost. Counting
// successes alone meant a provider that flapped its way through a run printed a
// figure that was not merely imprecise but structurally too low, in the one direction
// that matters: the operator was billed for tokens the report said were never spent.
func TestCountingModelCountsFailedCallsThatWereBilled(t *testing.T) {
	boom := errors.New("upstream 529 overloaded")
	c := &CountingModel{Inner: &billedFailureModel{
		usage: Usage{InputTokens: 4000, OutputTokens: 120, CachedInputTokens: 900, CacheWriteTokens: 300},
		err:   boom,
	}}
	for i := 0; i < 3; i++ {
		if _, err := c.Complete(context.Background(), CompletionRequest{}); !errors.Is(err, boom) {
			t.Fatalf("call %d: the error must still reach the caller, got %v", i, err)
		}
	}
	got := c.Total()
	want := Usage{InputTokens: 12_000, OutputTokens: 360, CachedInputTokens: 2700, CacheWriteTokens: 900}
	if got != want {
		t.Errorf("three failed-but-billed completions totalled %+v, want %+v — the tokens the "+
			"provider reported before it broke are real and billed, and a spend line that omits "+
			"them under-reports what the command cost", got, want)
	}
}

// TestCountingModelDoesNotChargeForUsageItNeverSaw is the over-count direction.
// Accounting a failed call must not slide into fabricating one: a failure BEFORE the
// provider reported anything (dial error, cancelled context) carries no usage, and
// "unknown" must add zero rather than a guess. A successful call must likewise be
// counted exactly once.
func TestCountingModelDoesNotChargeForUsageItNeverSaw(t *testing.T) {
	silent := &CountingModel{Inner: &billedFailureModel{err: errors.New("dial tcp: connection refused")}}
	for i := 0; i < 4; i++ {
		_, _ = silent.Complete(context.Background(), CompletionRequest{})
	}
	if got := silent.Total(); got != (Usage{}) {
		t.Errorf("four failures that reported no usage totalled %+v, want zero: a wrapper that "+
			"invents a figure for an unknown is no more honest than one that drops a known", got)
	}

	once := &CountingModel{Inner: &okModel{usage: Usage{InputTokens: 500, OutputTokens: 50}}}
	if _, err := once.Complete(context.Background(), CompletionRequest{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := (once.Total()); got != (Usage{InputTokens: 500, OutputTokens: 50}) {
		t.Errorf("one successful completion totalled %+v, want it counted exactly once", got)
	}
}
