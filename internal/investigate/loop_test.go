package investigate

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

type fakeScored struct{ hits []catalog.ScoredEntry }

func (f fakeScored) SearchScored(string, int) ([]catalog.ScoredEntry, error) { return f.hits, nil }

func TestInstantRecallHit(t *testing.T) {
	model := &scriptModel{} // no responses scripted: a call would panic, proving the loop is skipped
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model: model,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "Known incident", Description: "chart bump", Path: "known.md", Resource: "tooling/harbor"}, Score: 5.0}}}},
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Fingerprint: "fp-recall", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.i != 0 {
		t.Fatalf("model was called %d times; a recall hit must skip the loop", model.i)
	}
	if got == nil || len(got.RootCauses) != 1 || !strings.Contains(got.RootCauses[0].Summary, "Known incident") {
		t.Fatalf("unexpected recalled investigation: %+v", got)
	}
	if !got.Recalled {
		t.Fatal("a recalled investigation must be flagged Recalled so the curator skips it")
	}
	if got.Fingerprint != "fp-recall" {
		t.Fatalf("recall path must carry the alert fingerprint for outcome attribution, got %q", got.Fingerprint)
	}
}

func TestInstantRecallBelowThreshold(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.5,"root_causes":[{"summary":"freshly investigated"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model: model,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "weak"}, Score: 0.5}}}}, // below threshold → loop runs
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.i == 0 {
		t.Fatal("model not called; a below-threshold score must run the loop")
	}
}

func TestLoopInvestigatorActions(t *testing.T) {
	// submit_findings proposes a reversible and an irreversible action.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "submit_findings", Args: `{"confidence":0.9,"root_causes":[{"summary":"x"}],"actions":[{"description":"flux rollback hr/harbor","reversible":true},{"description":"delete the pvc","reversible":false}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Actions:    action.New(config.ActionPolicy{Mode: config.ActionSuggest, Allow: config.ActionAllow{ReversibleOnly: true}}),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "t", Fingerprint: "fp-loop"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Only the reversible action is surfaced (suggest mode, reversible_only); none executed.
	if got == nil || len(got.Actions) != 1 || got.Actions[0].Description != "flux rollback hr/harbor" {
		t.Fatalf("expected only the reversible action surfaced, got %+v", got)
	}
	if got.Fingerprint != "fp-loop" {
		t.Fatalf("full-loop path must carry the alert fingerprint for outcome attribution, got %q", got.Fingerprint)
	}
}

func TestLoopInvestigatorActionsDisabled(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "submit_findings", Args: `{"confidence":0.9,"root_causes":[{"summary":"x"}],"actions":[{"description":"flux rollback","reversible":true}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{ // no Actions policy => read-only, actions dropped
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	_ = li.Investigate(context.Background(), Request{Title: "t"})
	if got == nil || len(got.Actions) != 0 {
		t.Fatalf("expected no actions surfaced when policy disabled, got %+v", got)
	}
}

func TestLoopNudgesOnProseTurn(t *testing.T) {
	// Some models (Gemini in particular) answer the final turn in prose instead of
	// calling submit_findings. The loop must nudge once and recover, not give up.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{Text: "Based on the evidence, the chart bump broke the DB. Confidence ~0.8."}, // prose, no tool call
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"chart bump broke db"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "chart bump broke db" {
		t.Fatalf("nudge did not recover findings: %+v", got)
	}
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (prose + nudged submit), got %d", model.i)
	}
}

func TestLoopInconclusiveAfterNudge(t *testing.T) {
	// If the model still won't call a tool after the nudge, give up — don't loop forever.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{Text: "I think it's the database."},
		{Text: "Still just the database, no tool call."},
	}}
	var delivered bool
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) { delivered = true },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if delivered {
		t.Fatal("expected no delivery when the model never calls submit_findings")
	}
	if model.i != 2 {
		t.Fatalf("expected exactly 2 model calls (initial + one nudge), got %d", model.i)
	}
}

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

func TestPreferDiscoveredResource(t *testing.T) {
	origin := providers.Workload{Namespace: "apps", Name: "web"}
	cases := []struct {
		name       string
		discovered providers.Workload
		want       providers.Workload
	}{
		{"discovered wins", providers.Workload{Namespace: "apps", Name: "payment-api", Kind: "Deployment"}, providers.Workload{Namespace: "apps", Name: "payment-api", Kind: "Deployment"}},
		{"namespace defaulted from origin", providers.Workload{Name: "payment-api"}, providers.Workload{Namespace: "apps", Name: "payment-api"}},
		{"empty falls back to origin", providers.Workload{}, origin},
		{"namespace-only discovered kept", providers.Workload{Namespace: "ops"}, providers.Workload{Namespace: "ops"}},
	}
	for _, c := range cases {
		if got := preferDiscoveredResource(c.discovered, origin); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// bigTool is a fake Tool that returns a string of length n filled with 'z'.
type bigTool struct{ size int }

func (b bigTool) Name() string        { return "big_tool" }
func (b bigTool) Description() string { return "returns large output" }
func (b bigTool) Schema() string      { return `{"type":"object","properties":{}}` }
func (b bigTool) Call(_ context.Context, _ string) (string, error) {
	return strings.Repeat("z", b.size), nil
}

// TestLoopToolOutputTruncatedMetric verifies that oversized tool outputs are
// truncated and that ToolOutputTruncatedBytes is incremented via OTel metrics.
func TestLoopToolOutputTruncatedMetric(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	m := telemetry.NewMetrics()

	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 1: call big_tool
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "big_tool", Args: `{}`}}},
		// step 2: submit findings
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"found it"}]}`}}},
	}}

	li := &LoopInvestigator{
		Model:              model,
		Tools:              []Tool{bigTool{size: 2000}},
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:           5,
		MaxToolOutputBytes: 200, // 2000-byte output must be truncated to ~200
		Metrics:            m,
		OnComplete:         func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "big output test"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// Scrape /metrics and confirm ToolOutputTruncatedBytes appeared.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "runlore_tool_output_truncated_bytes_total") {
		t.Fatalf("runlore_tool_output_truncated_bytes_total not in metrics:\n%s", body)
	}
}
