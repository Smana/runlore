// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// Dimension is one rubric axis and its max score.
type Dimension struct {
	Key string
	Max int
}

// Rubric is the RCA-quality grading rubric (matches the design spec §5).
var Rubric = []Dimension{
	{"root_cause", 3},  // 0 wrong / 1 symptom-only / 2 correct-shallow / 3 correct+root
	{"evidence", 3},    // cited facts pertinent & true
	{"solution", 3},    // suggested action vs expected: correct, actionable, reversibility right
	{"description", 3}, // clarity, completeness, honest unresolved
	{"calibration", 2}, // high confidence only when correct
}

// Verdict is the judge's structured grade for one investigation.
type Verdict struct {
	Scores         map[string]int `json:"scores"`
	ConfidentWrong bool           `json:"confident_wrong"`
	Rationale      string         `json:"rationale"`
}

// Total sums the dimension scores.
func (v Verdict) Total() int {
	n := 0
	for _, d := range Rubric {
		n += v.Scores[d.Key]
	}
	return n
}

// Judge grades an investigation against a scenario's ground truth. An optional
// tool-transcript excerpt may be supplied so the judge can check that cited
// evidence traces to a real tool result (groundedness) rather than grading the
// findings text in isolation; callers with no transcript omit it.
type Judge interface {
	Grade(ctx context.Context, scn Scenario, inv providers.Investigation, transcript ...string) (Verdict, error)
}

// ModelJudge grades with an LLM (use a stronger model than the one under test).
type ModelJudge struct {
	Model providers.ModelProvider
}

const judgeSystem = `You are a strict, impartial SRE incident-investigation grader.
You are given the GROUND TRUTH for an incident and an ANONYMOUS investigation result
(you do not know which model produced it — grade only on merit).
Score each rubric dimension as an integer in [0, max]:
- root_cause (max 3): 0 wrong, 1 symptom-only, 2 correct but shallow, 3 correct AND reaches the true root.
- evidence (max 3): cited facts pertinent and true, not hallucinated or correlation-only.
- solution (max 3): suggested action vs expected — correct, actionable, reversibility flagged right.
- description (max 3): clarity, completeness, honest about what is unresolved.
- calibration (max 2): high confidence only when correct; penalise confident-and-wrong hardest.
When a tool-transcript excerpt is provided, judge evidence groundedness against it: cited facts must
trace to a tool result in the transcript, not be hallucinated or correlation-only. The excerpt is
bounded and may omit some results, so a missing line is not by itself proof the evidence is false.
Set confident_wrong=true if the result states a wrong root cause with confidence >= 0.7.
Record your grade by calling the submit_grade tool exactly once. If you cannot call tools, reply with
ONLY a JSON object: {"scores":{"root_cause":N,"evidence":N,"solution":N,"description":N,"calibration":N},"confident_wrong":bool,"rationale":"..."}.`

const submitGradeName = "submit_grade"

// submitGradeSpec is the structured-output tool for the judge. Its schema is
// generated from Rubric so the rubric and the tool contract cannot drift, and it
// mirrors the Verdict struct so the tool args unmarshal directly into a Verdict.
func submitGradeSpec() providers.ToolSpec {
	scoreProps := map[string]any{}
	keys := make([]string, 0, len(Rubric))
	for _, d := range Rubric {
		scoreProps[d.Key] = map[string]any{"type": "integer", "minimum": 0, "maximum": d.Max}
		keys = append(keys, d.Key)
	}
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scores":          map[string]any{"type": "object", "properties": scoreProps, "required": keys},
			"confident_wrong": map[string]any{"type": "boolean"},
			"rationale":       map[string]any{"type": "string"},
		},
		"required": []string{"scores", "confident_wrong", "rationale"},
	})
	if err != nil {
		panic(fmt.Sprintf("marshal submit_grade schema: %v", err)) // static input; unreachable
	}
	return providers.ToolSpec{
		Name:        submitGradeName,
		Description: "Record the rubric grade for the investigation.",
		Schema:      string(schema),
	}
}

// maxJudgeTranscriptBytes hard-caps the tool-transcript excerpt appended to the
// judge prompt, mirroring the verify pass. Kept small so grounding context never
// dominates the grading prompt or its cost.
const maxJudgeTranscriptBytes = 4000

// Grade builds a blind grading prompt and parses the JSON verdict. An optional
// transcript excerpt (first variadic arg) is appended, bounded, so the judge can
// check evidence groundedness against the tool results the investigation saw.
func (j ModelJudge) Grade(ctx context.Context, scn Scenario, inv providers.Investigation, transcript ...string) (Verdict, error) {
	user := fmt.Sprintf(`GROUND TRUTH
root_cause: %s
expected_action: %s
must_reach_root: %t

INVESTIGATION RESULT
%s`, scn.GroundTruth.RootCause, scn.GroundTruth.ExpectedAction, scn.GroundTruth.MustReachRoot, investigationText(inv)+confidenceLine(inv))
	if ex := boundedTranscript(transcript); ex != "" {
		user += "\n\nTOOL TRANSCRIPT EXCERPT (bounded, may be truncated — check cited evidence against it)\n" + ex
	}

	resp, err := j.Model.Complete(ctx, providers.CompletionRequest{
		System:   judgeSystem,
		Messages: []providers.Message{{Role: "user", Content: user}},
		Tools:    []providers.ToolSpec{submitGradeSpec()},
		// Force the grade through the tool so the verdict arrives as schema-shaped
		// args instead of free text that needs brace-slicing out of prose.
		ToolChoice: submitGradeName,
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("judge model: %w", err)
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name != submitGradeName {
			continue
		}
		var v Verdict
		if err := json.Unmarshal([]byte(tc.Args), &v); err != nil {
			return Verdict{}, fmt.Errorf("parse submit_grade args %q: %w", tc.Args, err)
		}
		if v.Scores == nil {
			v.Scores = map[string]int{}
		}
		return v, nil
	}
	// Fallback ONLY when the response carries no submit_grade tool call: weak
	// OpenAI-compatible judges sometimes ignore a forced tool_choice and reply in
	// prose, so the legacy first-'{'..last-'}' text parser is kept for them.
	v, err := parseVerdict(resp.Text)
	if err != nil {
		return Verdict{}, fmt.Errorf("parse verdict from %q: %w", resp.Text, err)
	}
	return v, nil
}

func confidenceLine(inv providers.Investigation) string {
	return fmt.Sprintf(" (overall confidence %.2f)", inv.Confidence)
}

// boundedTranscript returns the first supplied transcript excerpt, redacted and
// hard-capped to maxJudgeTranscriptBytes (keeping the tail — the most
// decision-relevant, latest tool results). Empty when no transcript is supplied.
// redact.Secrets is idempotent, so re-applying it over an already-redacted loop
// transcript is safe and guarantees no secret reaches the judge.
func boundedTranscript(transcript []string) string {
	if len(transcript) == 0 || transcript[0] == "" {
		return ""
	}
	s := redact.Secrets(transcript[0])
	if len(s) > maxJudgeTranscriptBytes {
		s = s[len(s)-maxJudgeTranscriptBytes:] // keep the tail (latest results)
	}
	return s
}

// parseVerdict extracts the first JSON object from the model text (models often
// wrap JSON in prose despite instructions).
func parseVerdict(s string) (Verdict, error) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return Verdict{}, fmt.Errorf("no JSON object found")
	}
	var v Verdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return Verdict{}, err
	}
	if v.Scores == nil {
		v.Scores = map[string]int{}
	}
	return v, nil
}
