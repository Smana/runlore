package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestVerifyRejectsCorrelationFinding(t *testing.T) {
	// Mirrors the real PR #38 failure: a high-confidence root cause backed only by
	// "started after change X" with the diff unread. The reviewer rejects it.
	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 0: the investigator submits a correlation-only finding
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"failing due to a recent change to crds-actions-runner-controller","confidence":0.8,"evidence":["started after the change","exact diff unknown"]}]}`}}},
		// verify pass: reject it
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"reject","confidence":0.1,"reason":"correlation only; diff never read; unrelated component"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify:     true,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborInstallFailed", Workload: providers.Workload{Namespace: "tooling"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	if len(got.RootCauses) != 0 {
		t.Fatalf("rejected root cause should be removed, got %+v", got.RootCauses)
	}
	if got.Verified {
		t.Fatal("a finding with no surviving cause must not be marked Verified")
	}
	if got.Confidence != 0 {
		t.Fatalf("overall confidence should drop to 0 with no surviving root cause, got %v", got.Confidence)
	}
	found := false
	for _, u := range got.Unresolved {
		if strings.Contains(u, "Rejected hypothesis") && strings.Contains(u, "crds-actions-runner-controller") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejected hypothesis should be recorded in unresolved, got %v", got.Unresolved)
	}
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (findings + verify), got %d", model.i)
	}
}

func TestVerifyDowngradesUnproven(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.9,"root_causes":[{"summary":"db migration stalled","confidence":0.9,"evidence":["migration lock held"]}]}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"downgrade","confidence":0.4,"reason":"plausible but not confirmed"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{Model: model, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Verify: true,
		OnComplete: func(inv providers.Investigation) { got = &inv }}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Confidence != 0.4 || got.Confidence != 0.4 {
		t.Fatalf("expected downgraded confidence 0.4, got %+v", got)
	}
	if !got.Verified {
		t.Fatal("a finding with a surviving reviewed cause must be marked Verified")
	}
}
