package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// jsonModel returns a fixed Text payload, and records the request it saw.
type jsonModel struct {
	reply     string
	gotSystem string
	gotUser   string
}

func (m *jsonModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.gotSystem = req.System
	for _, msg := range req.Messages {
		m.gotUser += msg.Content
	}
	return providers.CompletionResponse{Text: m.reply}, nil
}

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
