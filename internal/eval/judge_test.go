package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// jsonModel returns a fixed reply (a submit_grade tool call when toolArgs is set,
// free text otherwise), and records the request it saw.
type jsonModel struct {
	reply         string
	toolArgs      string
	gotSystem     string
	gotUser       string
	gotTools      []string
	gotToolChoice string
}

func (m *jsonModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.gotSystem = req.System
	for _, msg := range req.Messages {
		m.gotUser += msg.Content
	}
	for _, tool := range req.Tools {
		m.gotTools = append(m.gotTools, tool.Name)
	}
	m.gotToolChoice = req.ToolChoice
	if m.toolArgs != "" {
		return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{ID: "g1", Name: submitGradeName, Args: m.toolArgs}}}, nil
	}
	return providers.CompletionResponse{Text: m.reply}, nil
}

// TestModelJudgeForcedToolCall asserts the judge is a forced tool call: the
// request offers submit_grade and forces it via ToolChoice, and the verdict is
// parsed from the tool-call args (not from free text).
func TestModelJudgeForcedToolCall(t *testing.T) {
	m := &jsonModel{toolArgs: `{"scores":{"root_cause":3,"evidence":2,"solution":2,"description":3,"calibration":2},"confident_wrong":false,"rationale":"correct + deep"}`}
	j := ModelJudge{Model: m}
	scn := Scenario{ID: "x", GroundTruth: GroundTruth{RootCause: "valkey down", ExpectedAction: "restart valkey"}}
	inv := providers.Investigation{Title: "Harbor down", Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "valkey refused", SuggestedAction: "restart valkey"}}}
	v, err := j.Grade(context.Background(), scn, inv)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if v.Scores["root_cause"] != 3 || v.Total() != 12 || v.ConfidentWrong {
		t.Fatalf("verdict parse from tool args: %+v", v)
	}
	if m.gotToolChoice != submitGradeName {
		t.Fatalf("judge must force %q, got ToolChoice=%q", submitGradeName, m.gotToolChoice)
	}
	offered := false
	for _, n := range m.gotTools {
		if n == submitGradeName {
			offered = true
		}
	}
	if !offered {
		t.Fatalf("submit_grade tool was not offered; got %v", m.gotTools)
	}
}

// TestModelJudgeParsesVerdict covers the free-text FALLBACK: a weak
// OpenAI-compatible judge that ignores the forced tool call and replies in prose
// still yields a verdict via the brace-slice parser.
func TestModelJudgeParsesVerdict(t *testing.T) {
	m := &jsonModel{reply: "prefix junk\n" + `{"scores":{"root_cause":3,"evidence":2,"solution":2,"description":3,"calibration":2},"confident_wrong":false,"rationale":"correct + deep"}` + "\ntrailing"}
	j := ModelJudge{Model: m}
	scn := Scenario{ID: "x", GroundTruth: GroundTruth{RootCause: "valkey down", ExpectedAction: "restart valkey"}}
	inv := providers.Investigation{Title: "Harbor down", Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "valkey refused", SuggestedAction: "restart valkey"}}}
	v, err := j.Grade(context.Background(), scn, inv)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if v.Scores["root_cause"] != 3 || v.ConfidentWrong {
		t.Fatalf("verdict parse: %+v", v)
	}
	// judge prompt must carry the ground truth and the investigation, and must NOT
	// reveal which model produced it (blind).
	if !strings.Contains(m.gotUser, "valkey down") || !strings.Contains(m.gotUser, "valkey refused") {
		t.Fatalf("prompt missing ground truth/investigation: %q", m.gotUser)
	}
}

func TestRubricMax(t *testing.T) {
	if RubricMax() != 14 { // 3+3+3+3+2
		t.Fatalf("rubric max = %d, want 14", RubricMax())
	}
}
