package investigate

import (
	"encoding/json"
	"slices"
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
