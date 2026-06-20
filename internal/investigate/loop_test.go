package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// scriptModel returns a fixed sequence of responses, ignoring its input.
type scriptModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (m *scriptModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := m.responses[m.i]
	m.i++
	return r, nil
}

func TestLoopInvestigator(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// turn 1: ask what changed
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{"namespace":"flux-system"}`}}},
		// turn 2: submit findings
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings", Args: `{"confidence":0.8,"root_causes":[{"summary":"chart bump broke db","confidence":0.8}]}`}}},
	}}
	gp := fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}, FromRev: "a", ToRev: "b",
	}}}

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{WhatChangedTool{GitOps: gp}},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Source: SourceAlert, Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "flux-system"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete was not called")
	}
	if got.Confidence != 0.8 || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "chart bump broke db" {
		t.Fatalf("unexpected investigation: %+v", got)
	}
	// submit_findings carried no title, so it defaults to the triggering incident.
	if got.Title != "HarborProbeFailure" {
		t.Fatalf("title = %q, want it to default to the request title", got.Title)
	}
	if model.i != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", model.i)
	}
}
