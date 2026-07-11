// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestLogFieldsResolvedDefaults pins the resolved defaults to the EXACT strings the
// code hardcoded before logs.fields was configurable. A wrong default here breaks
// every logs query on the maintainer's test cluster, so these literals are the
// safety contract — do not "clean them up".
func TestLogFieldsResolvedDefaults(t *testing.T) {
	got := LogFields{}.Resolved()
	want := LogFields{
		ContainerField: "kubernetes.container_name",
		NamespaceField: "kubernetes.pod_namespace",
		PodField:       "kubernetes.pod_name",
		LevelField:     "log.level",
		UnpackPipe:     "unpack_json",
	}
	if got != want {
		t.Fatalf("resolved defaults drifted from the shipped convention:\ngot  %+v\nwant %+v", got, want)
	}
}

// TestLogFieldsResolvedPartialOverride: setting one field must not disturb the
// others — they keep their shipped defaults.
func TestLogFieldsResolvedPartialOverride(t *testing.T) {
	got := LogFields{NamespaceField: "namespace"}.Resolved()
	if got.NamespaceField != "namespace" {
		t.Fatalf("override not honoured: %q", got.NamespaceField)
	}
	if got.ContainerField != "kubernetes.container_name" || got.LevelField != "log.level" {
		t.Fatalf("unset fields must keep defaults: %+v", got)
	}
}

// TestLogsConfigEndpointInline confirms the endpoint keys stay at the `logs:` level
// (no code change to url/token_env/headers) while `fields:` nests under it.
func TestLogsConfigEndpointInline(t *testing.T) {
	const y = `
url: http://vl:9428
token_env: VL_TOKEN
fields:
  namespace_field: namespace
`
	var lc LogsConfig
	if err := yaml.Unmarshal([]byte(y), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.URL != "http://vl:9428" || lc.TokenEnv != "VL_TOKEN" {
		t.Fatalf("endpoint keys not inlined: %+v", lc.Endpoint)
	}
	if lc.Fields.NamespaceField != "namespace" {
		t.Fatalf("fields.namespace_field not parsed: %+v", lc.Fields)
	}
}
