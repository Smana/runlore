// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// costOnlyOnDeadlineModel is a hung provider that reports what it had already been
// billed for when the per-investigation deadline kills its request — the exact shape
// every RunLore model client returns on failure (CompletionResponse.CostOnly()
// alongside the error, see internal/model/clientcore.Stream). It exists because
// blockingModel returns a ZERO response, which cannot tell "the tokens were charged
// and then dropped" from "there were no tokens".
type costOnlyOnDeadlineModel struct {
	usage providers.Usage
	calls int
}

func (m *costOnlyOnDeadlineModel) Complete(ctx context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	<-ctx.Done()
	return providers.CompletionResponse{Usage: m.usage}, ctx.Err()
}

// TestFailedLoopStepIsChargedToTheInvestigation pins the accounting half of the
// check-then-spend-then-account rule at the loop's own model call: a completion that
// FAILED still billed for whatever the provider reported before it broke, and the
// investigation must carry that spend.
//
// The clients make the figure available on purpose — every one of them returns
// out.CostOnly() next to its error precisely "so a caller charging a spend budget"
// can read it (providers.CompletionResponse.CostOnly). Dropping it makes the running
// total under-report by a whole request, and the finding this run delivers reports a
// cost it did not incur — downwards, which is the direction the repo has already
// ruled out for the summarize digest (see summarizeElided).
//
// The deadline exit is the one that DELIVERS, so it is where the under-report is
// observable: the plain-error exit returns an error and hands the queue a retry, but
// the same accumulation covers both.
func TestFailedLoopStepIsChargedToTheInvestigation(t *testing.T) {
	model := &costOnlyOnDeadlineModel{usage: providers.Usage{InputTokens: 12_000, OutputTokens: 300}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		Timeout:    30 * time.Millisecond,
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "hung", Fingerprint: "fp-hung"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got.Usage.InputTokens != 12_000 || got.Usage.OutputTokens != 300 {
		t.Fatalf("a failed completion's reported tokens must count toward the investigation: "+
			"got %d input / %d output, want 12000 / 300. The provider billed them and said so "+
			"(CompletionResponse.CostOnly on the error path); a total that skips them under-reports "+
			"the run and lets a ceiling compare against a number that is too small.",
			got.Usage.InputTokens, got.Usage.OutputTokens)
	}
	if got.Usage.ModelCalls != 1 {
		t.Fatalf("a failed completion is still a model call; got ModelCalls=%d", got.Usage.ModelCalls)
	}
}

// TestRefusedLoopStepIsChargedToTheInvestigation is a CHARACTERISATION pin, not a
// fix: the refusal exit already accumulates, because addUsage sits above the
// Refused() check. It is written down so the ordering that makes it true cannot be
// undone by a later edit that moves accumulation back below the terminal branches —
// the refusal path is the one where a provider reliably reports full usage for a turn
// that produced no answer at all.
func TestRefusedLoopStepIsChargedToTheInvestigation(t *testing.T) {
	model := &mixedModel{steps: []mixedStep{{resp: providers.CompletionResponse{
		StopReason: "refusal",
		Usage:      providers.Usage{InputTokens: 9_000, OutputTokens: 5},
	}}}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "refused", Fingerprint: "fp-ref"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got.Usage.InputTokens != 9_000 || got.Usage.OutputTokens != 5 {
		t.Fatalf("a refused turn's tokens must count toward the investigation: got %d/%d, want 9000/5",
			got.Usage.InputTokens, got.Usage.OutputTokens)
	}
}

// TestFailedVerifyPassIsChargedToTheInvestigation is the same rule at the adversarial
// verify call. Verify runs after the last budget check by design (documented), so
// what is at stake here is the delivered figure rather than a ceiling: an
// investigation that reports its own cost must not report a verify pass it paid for
// as free.
func TestFailedVerifyPassIsChargedToTheInvestigation(t *testing.T) {
	model := &mixedModel{steps: []mixedStep{
		// step 0: the loop concludes with a real finding, cheaply.
		{resp: providers.CompletionResponse{
			ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
				Args: `{"confidence":0.8,"root_causes":[{"summary":"oom","confidence":0.8,"evidence":["OOMKilled"]}]}`}},
			Usage: providers.Usage{InputTokens: 1_000, OutputTokens: 50},
		}},
		// step 1: the verify pass reports its prompt cost and then dies mid-stream.
		{resp: providers.CompletionResponse{Usage: providers.Usage{InputTokens: 4_000, OutputTokens: 20}},
			err: errors.New("read stream: unexpected EOF")},
	}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		Verify:     true,
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "verify-down", Fingerprint: "fp-vd"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if want := 5_000; got.Usage.InputTokens != want {
		t.Fatalf("the failed verify pass's reported input tokens must be in the investigation's total: "+
			"got %d, want %d (1000 loop + 4000 verify)", got.Usage.InputTokens, want)
	}
	if want := 70; got.Usage.OutputTokens != want {
		t.Fatalf("the failed verify pass's reported output tokens must be in the total: got %d, want %d",
			got.Usage.OutputTokens, want)
	}
}

// costOnlyErrReranker reports its prompt cost and then fails — the mid-stream death
// errReranker cannot express (it returns a zero response).
type costOnlyErrReranker struct {
	usage providers.Usage
	calls int
}

func (m *costOnlyErrReranker) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{Usage: m.usage}, errors.New("reranker unavailable")
}

// TestFailedRerankIsChargedToTheInvestigation is the sharpest of the three, because
// the reranker's failure does NOT end the run: a rank error falls through to a full
// ReAct loop, which then spends its whole budget on top. Tokens dropped here are
// therefore tokens the loop's own ceiling never sees — the running total is short by
// the failed rank call for the entire rest of the investigation.
func TestFailedRerankIsChargedToTheInvestigation(t *testing.T) {
	rr := &costOnlyErrReranker{usage: providers.Usage{InputTokens: 2_000, OutputTokens: 30}}
	loop := &mixedModel{steps: []mixedStep{{resp: providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"oom","confidence":0.8}]}`}},
		Usage: providers.Usage{InputTokens: 1_000, OutputTokens: 10},
	}}}}
	var got providers.Investigation
	li := &LoopInvestigator{
		Model:      loop,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		Recall:     rerankRecall(rr, []catalog.ScoredEntry{webHit("web.md", 0.6)}),
		OnComplete: func(inv providers.Investigation) { got = inv },
	}
	if err := li.Investigate(context.Background(), okReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if rr.calls != 1 {
		t.Fatalf("the reranker must have been asked exactly once, got %d", rr.calls)
	}
	if want := 3_000; got.Usage.InputTokens != want {
		t.Fatalf("the failed rerank call's input tokens must be in the investigation's total: got %d, want %d "+
			"(2000 rerank + 1000 loop). A rank failure falls THROUGH to the full loop, so tokens dropped "+
			"here stay missing from the running total for the whole rest of the run.",
			got.Usage.InputTokens, want)
	}
	if want := 40; got.Usage.OutputTokens != want {
		t.Fatalf("the failed rerank call's output tokens must be in the total: got %d, want %d",
			got.Usage.OutputTokens, want)
	}
}
