package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
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

// RubricMax is the maximum total score across all dimensions.
func RubricMax() int {
	n := 0
	for _, d := range Rubric {
		n += d.Max
	}
	return n
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

// Judge grades an investigation against a scenario's ground truth.
type Judge interface {
	Grade(ctx context.Context, scn Scenario, inv providers.Investigation) (Verdict, error)
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
Set confident_wrong=true if the result states a wrong root cause with confidence >= 0.7.
Reply with ONLY a JSON object: {"scores":{"root_cause":N,"evidence":N,"solution":N,"description":N,"calibration":N},"confident_wrong":bool,"rationale":"..."}.`

// Grade builds a blind grading prompt and parses the JSON verdict.
func (j ModelJudge) Grade(ctx context.Context, scn Scenario, inv providers.Investigation) (Verdict, error) {
	user := fmt.Sprintf(`GROUND TRUTH
root_cause: %s
expected_action: %s
must_reach_root: %t

INVESTIGATION RESULT
%s`, scn.GroundTruth.RootCause, scn.GroundTruth.ExpectedAction, scn.GroundTruth.MustReachRoot, investigationText(inv)+confidenceLine(inv))

	resp, err := j.Model.Complete(ctx, providers.CompletionRequest{
		System:   judgeSystem,
		Messages: []providers.Message{{Role: "user", Content: user}},
	})
	if err != nil {
		return Verdict{}, fmt.Errorf("judge model: %w", err)
	}
	v, err := parseVerdict(resp.Text)
	if err != nil {
		return Verdict{}, fmt.Errorf("parse verdict from %q: %w", resp.Text, err)
	}
	return v, nil
}

func confidenceLine(inv providers.Investigation) string {
	return fmt.Sprintf(" (overall confidence %.2f)", inv.Confidence)
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
