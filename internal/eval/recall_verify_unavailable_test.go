// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

// alwaysErrModel fails every completion request — the shape a model outage takes at
// the provider boundary (401/5xx/timeout all surface as Complete returning an error).
// This is the exact fake used by the poisoned-recall trust-gate investigation's
// reproduction (.superpowers/sdd/poisoned-recall-investigation.md §5).
type alwaysErrModel struct {
	err   error
	calls int
}

func (m *alwaysErrModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{}, m.err
}

// TestShippedPoisonedRecallCaseWithdrawnWhenModelDown is the regression test for the
// recall-verify trust gate. It replays the REAL shipped
// examples/eval/poisoned-recall-verify.yaml case (the same fixture the nightly replay
// eval runs) against a model that ALWAYS errors — reproducing a full model outage.
//
// Before the fix: verifyFindings' fail-open `return inv` on a model error let the
// untrusted, unreviewed catalog answer straight through. tryRecall then fired on
// len(rec.RootCauses) > 0 alone, so "verify approved it" and "verify never ran" were
// indistinguishable — the poisoned entry short-circuited at 0.90 confidence on every
// run (5/5 in the investigation's reproduction).
//
// After the fix: "verify could not run" is a forced fall-through, the same path
// already taken when no gate clears at all — so the poisoned answer must be
// WITHDRAWN (Fired && !ShortCircuited), never short-circuited and never delivered.
func TestShippedPoisonedRecallCaseWithdrawnWhenModelDown(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	var pc *Case
	for i := range cases {
		if cases[i].Name == "poisoned-recall-verify" {
			pc = &cases[i]
		}
	}
	if pc == nil {
		t.Fatal("examples/eval/poisoned-recall-verify.yaml not loaded")
	}

	model := &alwaysErrModel{err: errors.New("401 authentication_error: invalid x-api-key")}

	// Wire the real fixture's catalog + recall gates exactly as Runner.runOne does
	// (internal/eval/eval.go) — this is the same recall→verify stack the nightly
	// replay eval exercises, just with an always-failing model in place of a live one.
	cat, err := catalog.New(filepath.Join(pc.dir, pc.CatalogDir))
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	rc := pc.recallConfig()
	recall := &investigate.Recall{
		Catalog:   cat,
		MinScore:  rc.MinScore,
		MarginGap: rc.MarginGap,
		SoloFloor: rc.SoloFloor,
	}

	var delivered int
	var decision investigate.RecallDecision
	li := &investigate.LoopInvestigator{
		Model:      model,
		Tools:      pc.FakeTools(),
		Log:        discardLog(),
		Recall:     recall,
		Verify:     true, // mirrors runOne: Verify: recall != nil
		OnRecall:   func(d investigate.RecallDecision) { decision = d },
		OnComplete: func(providers.Investigation) { delivered++ },
	}
	req := investigate.Request{Source: investigate.SourceAlert, Title: pc.Name, Message: pc.Prompt, Workload: pc.workload()}
	invErr := li.Investigate(context.Background(), req)

	// THE REGRESSION: the fixture is tuned to clear every recall gate (min_score,
	// solo_floor, margin_gap all 0.01), so recall must fire regardless of the model.
	// The bug is what happens NEXT.
	if !decision.Fired {
		t.Fatalf("recall must fire on the seeded fixture even when verify is unavailable, got %+v", decision)
	}
	if decision.ShortCircuited {
		t.Fatal("FAIL-OPEN REGRESSION: recall short-circuited on the poisoned entry although verify could not run (model error) " +
			"— the poisoned, unreviewed catalog answer was delivered as a confident finding")
	}
	// Nothing was ever delivered: the forced fall-through to a full investigation then
	// hits the SAME always-erroring model on its own first step, so Investigate returns
	// an error and OnComplete never fires. This proves the poisoned answer was
	// genuinely withdrawn, not silently swapped for an equally-unreviewed substitute.
	if delivered != 0 {
		t.Fatalf("nothing should have been delivered (poisoned recall withdrawn; the fallback loop also hit the down model), got %d deliveries", delivered)
	}
	if invErr == nil {
		t.Fatal("the fallback full investigation must also fail against the always-erroring model (Investigate should return an error, not silently succeed)")
	}
	if model.calls < 2 {
		t.Fatalf("expected >=2 model calls (the failed verify pass, then the fallback loop's first step), got %d", model.calls)
	}
}
