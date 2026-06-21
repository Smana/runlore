package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Smana/runlore/internal/providers"
)

// Tool is a model-callable capability used during an investigation.
type Tool interface {
	Name() string
	Description() string
	Schema() string // JSON Schema for the arguments
	Call(ctx context.Context, args string) (string, error)
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
	Title      string  `json:"title"`
	Confidence float64 `json:"confidence"`
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

// parseFindings turns submit_findings arguments into a providers.Investigation.
func parseFindings(args string) (providers.Investigation, error) {
	var f findings
	if err := json.Unmarshal([]byte(args), &f); err != nil {
		return providers.Investigation{}, fmt.Errorf("parse findings: %w", err)
	}
	inv := providers.Investigation{Title: f.Title, Confidence: f.Confidence, Unresolved: f.Unresolved}
	for _, rc := range f.RootCauses {
		inv.RootCauses = append(inv.RootCauses, providers.Hypothesis{
			Summary:         rc.Summary,
			Confidence:      rc.Confidence,
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
	return inv, nil
}
