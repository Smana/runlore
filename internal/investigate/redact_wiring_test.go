package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// recordModel captures the messages of every Complete call, so a test can assert
// what actually crossed the boundary to the model.
type recordModel struct {
	seen      []providers.Message
	responses []providers.CompletionResponse
	i         int
}

func (m *recordModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.seen = append(m.seen, req.Messages...)
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// leakyTool returns tool output containing secrets, as a real log line / diff might.
type leakyTool struct{}

func (leakyTool) Name() string        { return "leaky_logs" }
func (leakyTool) Description() string { return "returns logs" }
func (leakyTool) Schema() string      { return `{"type":"object","properties":{}}` }
func (leakyTool) Call(context.Context, string) (string, error) {
	return "log: password=hunter2horse token ghp_0123456789abcdefghijABCDEFGHIJ0123", nil
}

// TestLoopRedactsToolOutputBeforeModel verifies tool output is redacted before it
// reaches the model — secrets in logs/diffs never cross the LLM-vendor boundary,
// and since the model only sees redacted text, its quoted evidence is clean too (F1).
func TestLoopRedactsToolOutputBeforeModel(t *testing.T) {
	model := &recordModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "leaky_logs", Args: "{}"}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName, Args: `{"confidence":0.5,"root_causes":[{"summary":"x","confidence":0.5,"evidence":["e"]}]}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "3", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.5}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{leakyTool{}},
		MaxSteps:   5,
		Verify:     true,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	for _, m := range model.seen {
		if strings.Contains(m.Content, "hunter2horse") || strings.Contains(m.Content, "ghp_0123456789abcdefghijABCDEFGHIJ0123") {
			t.Fatalf("tool output reached the model with a secret unredacted: %q", m.Content)
		}
	}
	// Sanity: the redacted tool result WAS delivered (the marker is present), so the
	// assertion above isn't passing simply because the tool result never arrived.
	var sawRedacted bool
	for _, m := range model.seen {
		if strings.Contains(m.Content, "[REDACTED]") {
			sawRedacted = true
		}
	}
	if !sawRedacted {
		t.Fatal("expected the redacted tool result to reach the model")
	}
}
