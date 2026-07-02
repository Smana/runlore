package kbvalidate

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Verdict is one advisory judgement: whether the property holds, with a reason.
type Verdict struct {
	OK        bool
	Rationale string
}

// Advisory is the LLM-assisted semantic review of an entry. It is ADVISORY only
// — never a merge gate. Skipped is true when no model is configured or the
// review could not be obtained (the structural gate still applies).
type Advisory struct {
	CauseExplainsSymptom Verdict
	Durable              Verdict
	Skipped              bool
}

const submitReviewName = "submit_review"

const reviewSystemPrompt = `You review a proposed SRE knowledge-base entry for two qualities the deterministic
checks cannot judge. Be skeptical and call submit_review exactly once.

1. cause_explains_symptom — does the entry's top Cause plausibly explain THIS Symptom
   (not merely a nearby/related problem)?
2. durable — is this a durable, generalizable incident worth keeping, or transient/
   environmental/bootstrap-convergence noise unlikely to recur or teach anything?

Default ok=false when uncertain; give a one-line rationale for each.`

func submitReviewSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        submitReviewName,
		Description: "Submit the two-part semantic review of the KB entry.",
		Schema: `{
  "type": "object",
  "properties": {
    "cause_explains_symptom": {
      "type": "object",
      "properties": {"ok": {"type": "boolean"}, "rationale": {"type": "string"}},
      "required": ["ok", "rationale"]
    },
    "durable": {
      "type": "object",
      "properties": {"ok": {"type": "boolean"}, "rationale": {"type": "string"}},
      "required": ["ok", "rationale"]
    }
  },
  "required": ["cause_explains_symptom", "durable"]
}`,
	}
}

// ReviewSemantic asks the model to judge cause-explains-symptom and durability.
// It NEVER gates: a nil model, a model error, or a missing tool-call all return
// an Advisory with Skipped=true. The returned error is for logging only.
func ReviewSemantic(ctx context.Context, e catalog.Entry, m providers.ModelProvider) (Advisory, error) {
	if m == nil {
		return Advisory{Skipped: true}, nil
	}
	resp, err := m.Complete(ctx, providers.CompletionRequest{
		System:   reviewSystemPrompt,
		Messages: []providers.Message{{Role: "user", Content: renderEntry(e)}},
		Tools:    []providers.ToolSpec{submitReviewSpec()},
		// Force the tool: a prose reply degrades to Skipped (no advisory), so the
		// model must record its review through submit_review, not text.
		ToolChoice: submitReviewName,
	})
	if err != nil {
		return Advisory{Skipped: true}, fmt.Errorf("semantic review: %w", err)
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name != submitReviewName {
			continue
		}
		var raw struct {
			CauseExplainsSymptom struct {
				OK        bool   `json:"ok"`
				Rationale string `json:"rationale"`
			} `json:"cause_explains_symptom"`
			Durable struct {
				OK        bool   `json:"ok"`
				Rationale string `json:"rationale"`
			} `json:"durable"`
		}
		if err := json.Unmarshal([]byte(tc.Args), &raw); err != nil {
			return Advisory{Skipped: true}, fmt.Errorf("parse semantic review: %w", err)
		}
		return Advisory{
			CauseExplainsSymptom: Verdict{OK: raw.CauseExplainsSymptom.OK, Rationale: raw.CauseExplainsSymptom.Rationale},
			Durable:              Verdict{OK: raw.Durable.OK, Rationale: raw.Durable.Rationale},
		}, nil
	}
	// Model answered without calling the tool — nothing to act on.
	return Advisory{Skipped: true}, nil
}

// renderEntry presents the entry to the reviewer model.
func renderEntry(e catalog.Entry) string {
	return fmt.Sprintf("Type: %s\nTitle: %s\nResource: %s\nDescription: %s\n\n%s",
		e.Type, e.Title, e.Resource, e.Description, e.Body)
}
