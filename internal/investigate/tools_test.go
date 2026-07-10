// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestParseFindings(t *testing.T) {
	args := `{"confidence":0.82,"root_causes":[
	  {"summary":"chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff","confidence":0.82,"suggested_action":"flux rollback hr/harbor","reversible":true,"evidence":["pg_up=0","migration lock timeout"]}
	],"unresolved":["why the migration lock never released"]}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Confidence != 0.82 || len(inv.RootCauses) != 1 || len(inv.Unresolved) != 1 {
		t.Fatalf("unexpected investigation: %+v", inv)
	}
	rc := inv.RootCauses[0]
	if rc.Confidence != 0.82 || rc.SuggestedAction != "flux rollback hr/harbor" || !rc.Reversible || len(rc.Evidence) != 2 {
		t.Fatalf("unexpected root cause: %+v", rc)
	}
}

// TestParseFindingsOmittedOverallConfidenceFallsBackToRootCauses covers models
// (observed with GLM-5.2 over the OpenAI-compatible path) that fill per-root-cause
// confidence but omit the optional top-level field: the overall confidence must
// fall back to the highest root-cause confidence, not default to zero — a zero
// overall makes verify's min(overall, maxRootCause) pin the investigation to 0%.
func TestParseFindingsOmittedOverallConfidenceFallsBackToRootCauses(t *testing.T) {
	args := `{"root_causes":[
	  {"summary":"low-memory OOMKill","confidence":0.88},
	  {"summary":"secondary hypothesis","confidence":0.4}
	],"verdict":"action_required"}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Confidence != 0.88 {
		t.Fatalf("Confidence = %v, want fallback to max root-cause confidence 0.88", inv.Confidence)
	}
}

// TestParseFindingsExplicitOverallConfidenceKept asserts the fallback does not
// override a model that sets the top-level field, even when it is lower than the
// per-root-cause confidences.
func TestParseFindingsExplicitOverallConfidenceKept(t *testing.T) {
	args := `{"confidence":0.3,"root_causes":[{"summary":"s","confidence":0.9}]}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Confidence != 0.3 {
		t.Fatalf("Confidence = %v, want explicit 0.3 preserved", inv.Confidence)
	}
}

// TestParseFindingsNoConfidenceAnywhereStaysZero asserts the fallback only fires
// when a root cause actually carries confidence — all-omitted stays 0.
func TestParseFindingsNoConfidenceAnywhereStaysZero(t *testing.T) {
	inv, err := parseFindings(`{"root_causes":[{"summary":"s"}],"verdict":"inconclusive"}`)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Confidence != 0 {
		t.Fatalf("Confidence = %v, want 0 when no confidence supplied anywhere", inv.Confidence)
	}
}

// TestParseFindingsVerdictRuledOutDataGaps checks the three model-contract fields
// added for the notification overhaul map through parseFindings: a valid verdict
// enum, and the ruled_out / data_gaps honesty channels distinct from unresolved.
func TestParseFindingsVerdictRuledOutDataGaps(t *testing.T) {
	args := `{"title":"t","verdict":"no_action",
		"ruled_out":["plan deleted — plans still discovered in aws_backup_info"],
		"data_gaps":["CloudTrail truncated at 25 rows by SSM noise"],
		"root_causes":[{"summary":"s"}]}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Verdict != providers.VerdictNoAction {
		t.Fatalf("Verdict = %q, want no_action", inv.Verdict)
	}
	if len(inv.RuledOut) != 1 || len(inv.DataGaps) != 1 {
		t.Fatalf("RuledOut/DataGaps not mapped: %+v", inv)
	}
}

// TestParseFindingsUnknownVerdictNormalized asserts a verdict outside the enum is
// normalized to "" so formatters can safely omit it rather than render garbage.
func TestParseFindingsUnknownVerdictNormalized(t *testing.T) {
	inv, err := parseFindings(`{"verdict":"looks_fine","root_causes":[{"summary":"s"}]}`)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Verdict != "" {
		t.Fatalf("unknown verdict must map to empty, got %q", inv.Verdict)
	}
}

// TestOpEnumNoEmptyValue guards against re-introducing an empty-string enum
// member: Gemini's generateContent rejects empty enum values with HTTP 400
// ("enum[n]: cannot be empty"). The op field is optional — "suggestion only" is
// expressed by omitting it, not by an empty enum value — so the advertised enum
// must equal the canonical executable-op set exactly.
func TestOpEnumNoEmptyValue(t *testing.T) {
	var ops []string
	if err := json.Unmarshal([]byte(opEnumJSON()), &ops); err != nil {
		t.Fatalf("opEnumJSON is not valid JSON: %v", err)
	}
	if slices.Contains(ops, "") {
		t.Fatalf("op enum contains an empty value (Gemini rejects it): %q", ops)
	}
	want := []string{"reconcile", "resume", "suspend"}
	if len(ops) != len(providers.Ops) || !slices.Equal(ops, want) {
		t.Fatalf("op enum = %q, want %q (canonical providers.Ops, sorted)", ops, want)
	}
}

// TestSubmitFindingsSchemaNoEmptyEnum walks the whole submit_findings parameter
// schema and asserts no enum anywhere carries an empty value — the shape sent
// verbatim to every model provider must be Gemini-compatible.
func TestSubmitFindingsSchemaNoEmptyEnum(t *testing.T) {
	var schema any
	if err := json.Unmarshal([]byte(submitFindingsSpec().Schema), &schema); err != nil {
		t.Fatalf("submit_findings schema is not valid JSON: %v", err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch n := v.(type) {
		case map[string]any:
			if e, ok := n["enum"].([]any); ok {
				for i, val := range e {
					if s, ok := val.(string); ok && s == "" {
						t.Fatalf("empty enum value at %s.enum[%d]", path, i)
					}
				}
			}
			for k, child := range n {
				walk(path+"."+k, child)
			}
		case []any:
			for _, child := range n {
				walk(path, child)
			}
		}
	}
	walk("$", schema)
}

// TestClamp01 covers the helper directly, including the NaN case JSON cannot
// express (JSON has no NaN literal). NaN must clamp to 0 so it can never slip
// through the auto-action gate, where NaN < x is always false.
func TestClamp01(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"above one", 1.7, 1},
		{"below zero", -0.2, 0},
		{"nan", math.NaN(), 0},
		{"pos inf", math.Inf(1), 1},
		{"neg inf", math.Inf(-1), 0},
		{"in range", 0.42, 0.42},
		{"exactly one", 1, 1},
		{"exactly zero", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clamp01(tc.in); got != tc.want {
				t.Fatalf("clamp01(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseFindingsClampsConfidence verifies model-emitted confidence is clamped
// to [0,1] for both the overall and per-root-cause scores, so an out-of-range
// value can never reach the auto-action gate or the renderers.
func TestParseFindingsClampsConfidence(t *testing.T) {
	cases := []struct {
		name string
		val  string // JSON number for confidence (both overall and per-cause)
		want float64
	}{
		{"above one", "1.7", 1},
		{"below zero", "-0.2", 0},
		{"in range", "0.42", 0.42},
		{"exactly one", "1", 1},
		{"exactly zero", "0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := `{"confidence":` + tc.val + `,"root_causes":[{"summary":"x","confidence":` + tc.val + `}]}`
			inv, err := parseFindings(args)
			if err != nil {
				t.Fatalf("parseFindings: %v", err)
			}
			if inv.Confidence != tc.want {
				t.Fatalf("overall confidence = %v, want %v", inv.Confidence, tc.want)
			}
			if len(inv.RootCauses) != 1 || inv.RootCauses[0].Confidence != tc.want {
				t.Fatalf("root-cause confidence = %v, want %v", inv.RootCauses[0].Confidence, tc.want)
			}
		})
	}
}

// TestParseFindingsMalformed asserts that genuinely-unparseable args (not merely
// fenced/double-encoded) return a parse error rather than a zero-value
// Investigation passed off as success.
func TestParseFindingsMalformed(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{"truncated object", `{"root_causes":[{"summary":`},
		{"not json at all", `I think the harbor chart bump broke the db.`},
		{"empty", ``},
		{"fenced but still broken", "```json\n{\"root_causes\":[\n```"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFindings(tc.args)
			if err == nil {
				t.Fatalf("parseFindings(%q) = nil error, want a parse error", tc.args)
			}
			if !strings.HasPrefix(err.Error(), "parse findings:") {
				t.Fatalf("error must be wrapped as parse findings: …, got %q", err.Error())
			}
		})
	}
}

// TestParseFindingsTolerant covers the best-effort normalizer: a ```json fence or a
// double-encoded payload (some OpenAI-compatible backends) still parses, while a
// well-formed object is unaffected.
func TestParseFindingsTolerant(t *testing.T) {
	want := "chart bump broke db"
	cases := []struct {
		name string
		args string
	}{
		{"plain object (fast path)", `{"root_causes":[{"summary":"chart bump broke db"}]}`},
		{"json code fence", "```json\n{\"root_causes\":[{\"summary\":\"chart bump broke db\"}]}\n```"},
		{"bare code fence", "```\n{\"root_causes\":[{\"summary\":\"chart bump broke db\"}]}\n```"},
		{"double-encoded", `"{\"root_causes\":[{\"summary\":\"chart bump broke db\"}]}"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := parseFindings(tc.args)
			if err != nil {
				t.Fatalf("parseFindings(%q): %v", tc.args, err)
			}
			if len(inv.RootCauses) != 1 || inv.RootCauses[0].Summary != want {
				t.Fatalf("expected the unwrapped finding %q, got %+v", want, inv.RootCauses)
			}
		})
	}
}

func TestParseFindingsAffectedResource(t *testing.T) {
	args := `{"root_causes":[{"summary":"OOM in payment-api"}],
	  "affected_resource":{"kind":"Deployment","name":"payment-api","namespace":"apps"}}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Resource.Namespace != "apps" || inv.Resource.Name != "payment-api" || inv.Resource.Kind != "Deployment" {
		t.Fatalf("affected_resource not parsed into inv.Resource: %+v", inv.Resource)
	}
}
