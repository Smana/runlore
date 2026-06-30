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
"summary":{"type":"string"},"confidence":{"type":"number"},"change_ref":{"type":"string"},
"evidence":{"type":"array","items":{"type":"string"}},"suggested_action":{"type":"string"},"reversible":{"type":"boolean"}},
"required":["summary"]}},
"unresolved":{"type":"array","items":{"type":"string"}},
"actions":{"type":"array","description":"proposed remediations; prefer reversible, low-blast-radius","items":{"type":"object","properties":{
"description":{"type":"string"},"op":{"type":"string","enum":` + opEnumJSON() + `,"description":"executable op (Flux); omit for a suggestion only"},
"reversible":{"type":"boolean"},"blast_radius":{"type":"integer"},
"target":{"type":"object","properties":{"kind":{"type":"string"},"name":{"type":"string"},"namespace":{"type":"string"}}}},
"required":["description"]}}},"required":["root_causes"]}`,
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

// buildInvestigation maps the parsed findings shape onto a providers.Investigation,
// clamping confidences. Shared by the direct and the tolerant parse paths.
func buildInvestigation(f findings) providers.Investigation {
	inv := providers.Investigation{Title: f.Title, Confidence: clamp01(f.Confidence), Unresolved: f.Unresolved}
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
	return inv
}
