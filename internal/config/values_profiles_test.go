// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// chartDir is the packaged Helm chart, relative to this package.
const chartDir = "../../deploy/helm/runlore"

// valuesProfiles are the ready-made chart values files Getting Started's
// production tier offers as starting points, in escalating order: minimal is
// investigate + notify, standard adds the knowledge catalog / curation loop plus
// the metrics + logs evidence backends, full adds HA, persistence, a locked-down
// NetworkPolicy, the action ladder and the optional cloud / network signals.
//
// They are the copy-paste entry point for every in-cluster install — the literal
// first YAML a new operator runs — so all three are pinned here rather than left
// to rot behind the one that happened to be guarded first. Add any new profile
// to this list.
var valuesProfiles = []string{
	"values-minimal.yaml",
	"values-standard.yaml",
	"values-full.yaml",
}

// readValuesProfile decodes a whole values profile into the generic shape Helm
// itself sees (the raw values tree), failing the test if the file is missing or
// is not valid YAML.
func readValuesProfile(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s is not valid YAML: %v", name, err)
	}
	return v
}

// TestValuesProfilesConfigPassesStrictLoader pins every shipped values profile to
// the real config schema: the `config:` block of each file must survive the strict
// (KnownFields) loader AND Validate. The chart renders that block verbatim into the
// agent's config file, so a schema change that breaks a profile fails here instead
// of on a new user's first install.
//
// Note the shape: a values profile is NOT a runlore.yaml. Everything the agent
// reads is nested under `config:` (the chart unwraps it into the ConfigMap), while
// the sibling top-level keys — replicaCount, catalog, persistence, rbac… — are
// chart-level and never reach the loader. Only the `config:` sub-tree is fed to
// Load below; the rest is covered by TestValuesProfilesMatchChartSchema.
func TestValuesProfilesConfigPassesStrictLoader(t *testing.T) {
	for _, name := range valuesProfiles {
		t.Run(name, func(t *testing.T) {
			values := readValuesProfile(t, name)
			block, ok := values["config"].(map[string]any)
			if !ok || len(block) == 0 {
				t.Fatalf("%s carries no config block", name)
			}
			blob, err := yaml.Marshal(block)
			if err != nil {
				t.Fatalf("marshal config block: %v", err)
			}
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, blob, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := Load(p)
			if err != nil {
				t.Fatalf("strict Load rejected the %s config: %v", name, err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate rejected the %s config: %v", name, err)
			}
		})
	}
}

// jsonSchema is the SUBSET of JSON Schema draft-07 that
// deploy/helm/runlore/values.schema.json actually uses.
//
// Helm validates a merged values tree against that file on every install, but CI
// only ever runs `helm lint` on values.yaml — a profile shipped next to it is
// never checked there, so a `replicaCount: "2"` (string) or a
// `workloadKind: Daemonset` in one would first surface at a user's `helm install`.
// This repo carries no JSON Schema dependency and is not adding one for a
// six-keyword schema, hence the small reimplementation below.
//
// TestChartSchemaUsesOnlySupportedKeywords guards it: a keyword this struct does
// not decode is silently ignored, which would turn the check into theatre, so the
// schema growing one fails there rather than here.
type jsonSchema struct {
	Type                 any                    `json:"type"` // a string, or []string for a union
	Enum                 []any                  `json:"enum"`
	Minimum              *float64               `json:"minimum"`
	Properties           map[string]*jsonSchema `json:"properties"`
	Items                *jsonSchema            `json:"items"`
	AdditionalProperties *bool                  `json:"additionalProperties"`
}

// supportedSchemaKeywords lists what the walk above understands: the three
// annotation keywords (no validation effect) plus the six jsonSchema decodes.
var supportedSchemaKeywords = map[string]bool{
	"$schema": true, "title": true, "description": true,
	"type": true, "enum": true, "minimum": true,
	"properties": true, "items": true, "additionalProperties": true,
}

// loadChartSchema decodes values.schema.json into the checkable subset.
func loadChartSchema(t *testing.T) *jsonSchema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatalf("read values.schema.json: %v", err)
	}
	var s jsonSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("values.schema.json is not valid JSON Schema: %v", err)
	}
	return &s
}

// TestChartSchemaUsesOnlySupportedKeywords guards the guard. checkAgainstSchema
// enforces exactly the keywords jsonSchema decodes; a `pattern`, `required` or
// `oneOf` added to values.schema.json later would be dropped on decode and the
// profile check would keep reporting success while checking less than it claims.
// Fail loudly at that moment instead.
func TestChartSchemaUsesOnlySupportedKeywords(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatalf("read values.schema.json: %v", err)
	}
	var node map[string]any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("values.schema.json is not valid JSON: %v", err)
	}
	walkSchemaKeywords(t, "$", node)
}

// walkSchemaKeywords reports any keyword the mini-validator does not implement.
// It has to know the schema's own structure to do it: at a schema node the keys
// are keywords, but under `properties` they are user-chosen VALUE names (which
// may legitimately be called "pattern"), so only the nested schemas are recursed.
func walkSchemaKeywords(t *testing.T, path string, node map[string]any) {
	t.Helper()
	for k, v := range node {
		if !supportedSchemaKeywords[k] {
			t.Errorf("%s: values.schema.json uses the JSON Schema keyword %q, which the "+
				"mini-validator in this file ignores — decode it in jsonSchema and enforce "+
				"it in checkAgainstSchema, or the profile check silently under-checks", path, k)
			continue
		}
		switch k {
		case "properties":
			props, _ := v.(map[string]any)
			for name, sub := range props {
				if m, ok := sub.(map[string]any); ok {
					walkSchemaKeywords(t, path+".properties."+name, m)
				}
			}
		case "items":
			if m, ok := v.(map[string]any); ok {
				walkSchemaKeywords(t, path+".items", m)
			}
		}
	}
}

// TestValuesProfilesMatchChartSchema validates every shipped profile against the
// chart's own values.schema.json — the same file Helm enforces at install time —
// so a profile that drifts from it fails CI rather than a user's install.
func TestValuesProfilesMatchChartSchema(t *testing.T) {
	schema := loadChartSchema(t)
	for _, name := range valuesProfiles {
		t.Run(name, func(t *testing.T) {
			for _, problem := range checkAgainstSchema(name, schema, readValuesProfile(t, name)) {
				t.Errorf("values.schema.json violation: %s", problem)
			}
		})
	}
}

// checkAgainstSchema walks value against s and returns one message per violation.
// A nil schema (no constraint declared for this key) or a nil value (a YAML key
// written with no value, which Helm reads as unset) is not a violation.
func checkAgainstSchema(path string, s *jsonSchema, value any) []string {
	if s == nil || value == nil {
		return nil
	}
	if !matchesJSONType(s.Type, value) {
		// Stop here: every nested check below assumes the type held.
		return []string{fmt.Sprintf("%s: want type %v, got %T (%v)", path, s.Type, value, value)}
	}
	var problems []string
	if len(s.Enum) > 0 && !containsValue(s.Enum, value) {
		problems = append(problems, fmt.Sprintf("%s: %v is not one of %v", path, value, s.Enum))
	}
	if n, ok := asNumber(value); ok && s.Minimum != nil && n < *s.Minimum {
		problems = append(problems, fmt.Sprintf("%s: %v is below the minimum %v", path, value, *s.Minimum))
	}
	switch v := value.(type) {
	case map[string]any:
		for k, child := range v {
			sub, declared := s.Properties[k]
			if !declared {
				// The schema is deliberately open (additionalProperties: true everywhere,
				// notably under `config`), so an undeclared key is normal — flag it only
				// where the schema actually closes the object.
				if s.AdditionalProperties != nil && !*s.AdditionalProperties {
					problems = append(problems, fmt.Sprintf("%s.%s: not allowed (additionalProperties is false)", path, k))
				}
				continue
			}
			problems = append(problems, checkAgainstSchema(path+"."+k, sub, child)...)
		}
	case []any:
		for i, item := range v {
			problems = append(problems, checkAgainstSchema(fmt.Sprintf("%s[%d]", path, i), s.Items, item)...)
		}
	}
	return problems
}

// matchesJSONType reports whether value satisfies a `type` keyword, which is
// either a single type name or a union list of them.
func matchesJSONType(spec, value any) bool {
	switch t := spec.(type) {
	case string:
		return isJSONType(t, value)
	case []any:
		for _, alt := range t {
			if name, ok := alt.(string); ok && isJSONType(name, value) {
				return true
			}
		}
		return false
	default:
		return true // no `type` keyword on this node — nothing to check
	}
}

// isJSONType maps one JSON Schema type name onto the Go values yaml.v3 produces.
func isJSONType(want string, value any) bool {
	switch want {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		// YAML gives int for whole numbers, but JSON-sourced defaults can arrive as
		// float64; a float64 is an integer only when it has no fractional part.
		n, ok := asNumber(value)
		return ok && n == math.Trunc(n)
	case "number":
		_, ok := asNumber(value)
		return ok
	default:
		return true // unknown type name — not this guard's business
	}
}

// asNumber widens the numeric kinds yaml.v3 and encoding/json produce.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// containsValue reports whether value equals any enum member. DeepEqual rather
// than == because an enum member is an arbitrary JSON value and == panics on the
// uncomparable ones.
func containsValue(enum []any, value any) bool {
	for _, e := range enum {
		if reflect.DeepEqual(e, value) {
			return true
		}
	}
	return false
}
