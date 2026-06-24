package kbvalidate

import (
	"context"
	"errors"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeModel struct {
	resp     providers.CompletionResponse
	err      error
	gotTools []string
}

func (f *fakeModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	for _, t := range req.Tools {
		f.gotTools = append(f.gotTools, t.Name)
	}
	return f.resp, f.err
}

func reviewCall(args string) providers.CompletionResponse {
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: submitReviewName, Args: args}}}
}

func TestReviewSemanticNilModel(t *testing.T) {
	adv, err := ReviewSemantic(context.Background(), validIncident(), nil)
	if err != nil || !adv.Skipped {
		t.Fatalf("nil model must skip cleanly, got skipped=%v err=%v", adv.Skipped, err)
	}
}

func TestReviewSemanticModelError(t *testing.T) {
	adv, _ := ReviewSemantic(context.Background(), validIncident(), &fakeModel{err: errors.New("boom")})
	if !adv.Skipped {
		t.Fatalf("a model error must degrade to Skipped (never gate), got %+v", adv)
	}
}

func TestReviewSemanticTransient(t *testing.T) {
	m := &fakeModel{resp: reviewCall(`{"cause_explains_symptom":{"ok":true,"rationale":"matches"},"durable":{"ok":false,"rationale":"transient bootstrap noise"}}`)}
	adv, err := ReviewSemantic(context.Background(), validIncident(), m)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if adv.Skipped {
		t.Fatal("a parsed advisory must not be Skipped")
	}
	if !adv.CauseExplainsSymptom.OK {
		t.Fatalf("expected cause-explains-symptom ok, got %+v", adv.CauseExplainsSymptom)
	}
	if adv.Durable.OK || adv.Durable.Rationale == "" {
		t.Fatalf("expected durable not-ok with a rationale, got %+v", adv.Durable)
	}
}

func TestReviewSemanticCauseMismatch(t *testing.T) {
	m := &fakeModel{resp: reviewCall(`{"cause_explains_symptom":{"ok":false,"rationale":"cause is about a different subsystem"},"durable":{"ok":true,"rationale":"durable"}}`)}
	adv, _ := ReviewSemantic(context.Background(), validIncident(), m)
	if adv.CauseExplainsSymptom.OK {
		t.Fatalf("expected cause-explains-symptom not-ok, got %+v", adv.CauseExplainsSymptom)
	}
	// the submit_review tool must have been offered to the model
	found := false
	for _, n := range m.gotTools {
		if n == submitReviewName {
			found = true
		}
	}
	if !found {
		t.Fatalf("submit_review tool was not offered; got %v", m.gotTools)
	}
}
