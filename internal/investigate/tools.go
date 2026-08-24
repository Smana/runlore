// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// clamp01 constrains a model-emitted confidence to [0,1]; NaN -> 0. A NaN score
// must never pass the auto-action gate, where NaN < threshold is always false,
// nor poison the max() that recomputes overall confidence after the verify pass.
// +Inf/-Inf fall through the >1 / <0 arms.
func clamp01(x float64) float64 {
	switch {
	case math.IsNaN(x):
		return 0
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}

// Tool is a model-callable capability used during an investigation.
type Tool interface {
	Name() string
	Description() string
	Schema() string // JSON Schema for the arguments
	Call(ctx context.Context, args string) (string, error)
}

// incidentScoped is implemented by tools that must be bound to the incident's own
// namespace before use (currently pod_logs, whose namespace allowlist includes the
// incident namespace). The loop calls this per investigation when assembling tools,
// since a single LoopInvestigator instance serves many requests. A tool not
// implementing this interface is used unchanged.
type incidentScoped interface {
	withIncidentNamespace(ns string) Tool
}

// scopeTools binds any incident-scoped tools to this investigation's namespace,
// returning a fresh slice so the shared li.Tools is never mutated. Non-scoped tools
// pass through unchanged.
func scopeTools(tools []Tool, incidentNamespace string) []Tool {
	scoped := make([]Tool, len(tools))
	for i, t := range tools {
		if s, ok := t.(incidentScoped); ok {
			scoped[i] = s.withIncidentNamespace(incidentNamespace)
			continue
		}
		scoped[i] = t
	}
	return scoped
}

// submitFindingsName is the reserved tool the model calls to finish, supplying
// the structured investigation result.
const submitFindingsName = "submit_findings"

// submitFindingsSpec advertises the structured-output tool to the model.
func submitFindingsSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        submitFindingsName,
		Description: "Submit the final investigation: ranked root causes with evidence, plus anything unresolved.",
		Schema: `{"type":"object","properties":{
"title":{"type":"string"},
"confidence":{"type":"number"},
"affected_resource":{"type":"object","description":"the workload your investigation identified as the failing/affected resource","properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}},
"root_causes":{"type":"array","items":{"type":"object","properties":{
"summary":{"type":"string"},
"confidence":{"type":"number","description":"how strongly the evidence below supports THIS root cause, 0-1. It measures support for the cause you STATED, not how sure you are of the narrative around it. Required: an omitted confidence is delivered as 0%, which reads to the on-call as 'no confidence' and buries a sound finding under a red badge"},
"change_ref":{"type":"string"},
"evidence":{"type":"array","minItems":1,"items":{"type":"string"},"description":"REQUIRED, at least one: the specific tool results that support this cause - name the tool and quote the value, error or log line it returned. This is what lets a human check the cause and what the verify pass traces. A cause asserted with no evidence is delivered as a bare paragraph under a confidence badge nothing backs"},
"suggested_action":{"type":"string","description":"what the human should DO about this cause. Required whenever the verdict is action_suggested or action_required, unless a top-level actions entry already covers it - a card whose verdict promises an action and then offers none is worse than one that admits it is inconclusive"},
"reversible":{"type":"boolean"}},
"required":["summary","confidence","evidence"]}},
"unresolved":{"type":"array","items":{"type":"string"},"description":"genuine open questions only a human can answer - put tool or data limitations in data_gaps instead"},
"verdict":{"type":"string","enum":["no_action","action_suggested","action_required","inconclusive"],"description":"actionability for the on-call: no_action (benign/self-healed/synthetic), action_suggested (a human should follow the next steps), action_required (live impact needing prompt action), inconclusive (you genuinely could not determine the cause, and data_gaps/unresolved say what blocked you). A recurrence of a fault you DID identify is NOT inconclusive - naming a known cause again is still a conclusion, so restate it with the actionability verdict it deserves. The converse also holds: if you could not name the failing resource or the cause THIS time, the verdict is inconclusive however sure you are of everything around it - do not report a high-confidence action_suggested whose actual finding is that you could not identify what failed"},
"ruled_out":{"type":"array","items":{"type":"string"},"description":"hypotheses you considered and REJECTED, one line each naming the disproving evidence"},
"data_gaps":{"type":"array","items":{"type":"string"},"description":"signals you could not obtain (tool errors, RBAC denials, truncated output) that limited the investigation - data limitations, NOT questions for a human. State the tool you called and the ACTUAL error it returned. Do NOT speculate about a cause you did not verify: if a metrics query returned nothing, call discover_metrics before claiming a metric or its name is absent; if a logs query returned nothing, call discover_log_fields; if a tool was refused, report the refusal rather than guessing why. An empty result is not evidence that the data does not exist."},
"actions":{"type":"array","description":"proposed remediations; prefer reversible, low-blast-radius","items":{"type":"object","properties":{
"description":{"type":"string"},"op":{"type":"string","enum":` + opEnumJSON() + `,"description":"executable GitOps op (Flux or Argo CD); omit for a suggestion only"},
"reversible":{"type":"boolean"},"blast_radius":{"type":"integer"},
"target":{"type":"object","properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}}},
"required":["description"]}}},"required":["root_causes","verdict"]}`,
	}
}

// opEnumJSON renders the executable-op enum for the schema from the canonical
// registry (providers.Ops, sorted), so the model-facing schema can't drift from
// what the gate and executor actually accept. A "suggestion only" action is
// expressed by omitting op (it is not a required field) — never by an empty enum
// value: Gemini's generateContent rejects empty enum members with HTTP 400.
func opEnumJSON() string {
	ops := make([]string, 0, len(providers.Ops))
	for op := range providers.Ops {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	b, _ := json.Marshal(ops)
	return string(b)
}

// findings is the JSON shape of submit_findings arguments.
type findings struct {
	Title            string  `json:"title"`
	Confidence       float64 `json:"confidence"`
	AffectedResource struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"affected_resource"`
	RootCauses []struct {
		Summary         string   `json:"summary"`
		Confidence      float64  `json:"confidence"`
		ChangeRef       string   `json:"change_ref"`
		Evidence        []string `json:"evidence"`
		SuggestedAction string   `json:"suggested_action"`
		Reversible      bool     `json:"reversible"`
	} `json:"root_causes"`
	Unresolved []string `json:"unresolved"`
	Verdict    string   `json:"verdict"`
	RuledOut   []string `json:"ruled_out"`
	DataGaps   []string `json:"data_gaps"`
	Actions    []struct {
		Description string `json:"description"`
		Op          string `json:"op"`
		Reversible  bool   `json:"reversible"`
		BlastRadius int    `json:"blast_radius"`
		Target      struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"target"`
	} `json:"actions"`
}

// unwrapToolArgs makes the tool-call arguments tolerant of two malformations some
// OpenAI-compatible backends emit instead of a bare JSON object:
//   - a ```json … ``` code fence wrapping the object, and
//   - a double-encoded payload (the object serialized into a JSON *string*).
//
// It is a best-effort normalizer applied only as a fallback after a direct parse
// fails, so a well-formed object is never touched. A single string-unwrap level is
// enough for the observed double-encoding; anything still invalid is returned for
// the caller to surface as a parse error.
func unwrapToolArgs(args string) string {
	s := strings.TrimSpace(args)
	// Strip a leading ```json (or ```) fence and its trailing ```.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		s = strings.TrimPrefix(s, "JSON")
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// Unwrap one level of double-encoding: a JSON string whose contents are the
	// real object (e.g. "\"{\\\"root_causes\\\":[…]}\"").
	if strings.HasPrefix(s, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			return strings.TrimSpace(inner)
		}
	}
	return s
}

// parseFindings turns submit_findings arguments into a providers.Investigation.
// It first parses the arguments verbatim; only if that fails does it retry against
// a normalized payload (fence-stripped / one level un-double-encoded), so
// well-formed args follow the fast path unchanged.
func parseFindings(args string) (providers.Investigation, error) {
	var f findings
	if err := json.Unmarshal([]byte(args), &f); err != nil {
		if cleaned := unwrapToolArgs(args); cleaned != args {
			if err2 := json.Unmarshal([]byte(cleaned), &f); err2 == nil {
				return buildInvestigation(f), nil
			}
		}
		return providers.Investigation{}, fmt.Errorf("parse findings: %w", err)
	}
	return buildInvestigation(f), nil
}

// unaccountedInconclusive reports whether a submitted finding claims it could not
// determine the cause while giving no account of what blocked it: no root cause, no
// question for a human, no data gap. That payload contradicts itself — an honest
// "I could not determine" always has something in one of those three channels — and
// it is what the model produced when it reached for `inconclusive` to mean "this is
// already known" (#471): a Slack card with no Why, no evidence and no next steps,
// and nothing for the verify pass to check. Cheap and pure, so the loop can say so
// at the source, where the payload is still attributable to the call that made it.
//
// The predicate itself lives on providers.Investigation, because the CARD acts on
// the same shape now (an inconclusive verdict that explains nothing says so in the
// notification instead of shipping blank) and a second copy here is how the writer's
// idea of "unaccounted" and the reader's drift apart. This name stays because it is
// the loop's own vocabulary, and its call site reads as a sentence.
//
// Deliberately NOT an error: the finding is still delivered. Rejecting it would
// spend another model call to re-ask a question the model just answered badly, and a
// thin answer beats no answer for the human reading the card.
func unaccountedInconclusive(inv providers.Investigation) bool {
	return inv.UnaccountedInconclusive()
}

// unevidencedConclusion reports whether a finding reaches a conclusive verdict while
// its leading root cause cites no evidence at all.
//
// Live on 2026-08-22: "High confidence · 85%" over a Why paragraph with not one
// bullet beneath it. The prose may well have been right, but nothing on the card let
// a human check it and nothing let the verify pass trace it — the confidence badge
// was backed by the model's say-so alone. `inconclusive` is exempt: having nothing
// to show is what that verdict is for. So is an empty root-cause list, which is
// unaccountedInconclusive's business.
func unevidencedConclusion(inv providers.Investigation) bool {
	// Conclusive() rather than "!= inconclusive": an omitted or unparseable verdict
	// leaves inv.Verdict empty, which renders no badge at all, so there is no claim
	// to hold to account. Testing it the other way warned about a badge never drawn.
	if !inv.Verdict.Conclusive() || len(inv.RootCauses) == 0 {
		return false
	}
	// Non-blank, because the schema's minItems:1 counts an empty string. Requiring a
	// present-but-empty array to silence the warning would make the cheapest possible
	// non-answer the way to pass, and the card then renders a bare bullet.
	for _, e := range inv.RootCauses[0].Evidence {
		if strings.TrimSpace(e) != "" {
			return false
		}
	}
	return true
}

// buildInvestigation maps the parsed findings shape onto a providers.Investigation,
// clamping confidences. Shared by the direct and the tolerant parse paths.
func buildInvestigation(f findings) providers.Investigation {
	inv := providers.Investigation{Title: f.Title, Confidence: clamp01(f.Confidence),
		Unresolved: f.Unresolved, RuledOut: f.RuledOut, DataGaps: f.DataGaps}
	if v := providers.Verdict(f.Verdict); providers.ValidVerdict(v) {
		inv.Verdict = v
	}
	for _, rc := range f.RootCauses {
		inv.RootCauses = append(inv.RootCauses, providers.Hypothesis{
			Summary:         rc.Summary,
			Confidence:      clamp01(rc.Confidence),
			ChangeRef:       rc.ChangeRef,
			Evidence:        rc.Evidence,
			SuggestedAction: rc.SuggestedAction,
			Reversible:      rc.Reversible,
		})
	}
	for _, a := range f.Actions {
		inv.Actions = append(inv.Actions, providers.Action{
			Name:        a.Description,
			Description: a.Description,
			Op:          a.Op,
			Target:      providers.Workload{Kind: a.Target.Kind, Name: a.Target.Name, Namespace: a.Target.Namespace},
			Mutating:    true,
			Reversible:  a.Reversible,
			BlastRadius: a.BlastRadius,
		})
	}
	inv.Resource = providers.Workload{
		Kind:      f.AffectedResource.Kind,
		Name:      f.AffectedResource.Name,
		Namespace: f.AffectedResource.Namespace,
	}
	// The top-level confidence is optional in the schema and some models (e.g.
	// GLM over the OpenAI-compatible path) only fill the per-root-cause field.
	// A missing overall would read as 0% and verify's min(overall, maxRootCause)
	// would pin it there, so fall back to the strongest root cause.
	if inv.Confidence == 0 {
		for _, rc := range inv.RootCauses {
			inv.Confidence = max(inv.Confidence, rc.Confidence)
		}
	}
	return inv
}
