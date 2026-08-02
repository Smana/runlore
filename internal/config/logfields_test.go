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

// TestLogFieldsResolvedForLoki: ResolvedFor(loki) fills the promtail/alloy
// stream-label convention and Loki 3.x's parser-less detected_level severity,
// and deliberately leaves UnpackPipe empty (no parser stage needed). Any
// explicitly-set field wins. ResolvedFor(victorialogs) must equal Resolved().
func TestLogFieldsResolvedForLoki(t *testing.T) {
	got := LogFields{}.ResolvedFor(LogsProviderLoki)
	want := LogFields{ContainerField: "container", NamespaceField: "namespace",
		PodField: "pod", LevelField: "detected_level", UnpackPipe: ""}
	if got != want {
		t.Fatalf("loki defaults = %+v, want %+v", got, want)
	}
	over := LogFields{LevelField: "level", UnpackPipe: "logfmt"}.ResolvedFor(LogsProviderLoki)
	if over.LevelField != "level" || over.UnpackPipe != "logfmt" || over.PodField != "pod" {
		t.Fatalf("overrides must win, defaults fill the rest: %+v", over)
	}
	if (LogFields{}.ResolvedFor(LogsProviderVictoriaLogs)) != (LogFields{}.Resolved()) {
		t.Fatalf("victorialogs resolution must be unchanged")
	}
}

// TestLogFieldsResolvedForECS: ResolvedFor(elasticsearch) and
// ResolvedFor(opensearch) both fill the SAME ECS (Elastic Common Schema)
// convention — the two distributions speak an identical wire format, so they
// must not drift into two different defaults. TimestampField/MessageField are
// ECS-specific fields with no VictoriaLogs/Loki analogue. Any explicitly-set
// field wins over the default.
func TestLogFieldsResolvedForECS(t *testing.T) {
	want := LogFields{
		ContainerField: "kubernetes.container.name",
		NamespaceField: "kubernetes.namespace",
		PodField:       "kubernetes.pod.name",
		LevelField:     "log.level",
		TimestampField: "@timestamp",
		MessageField:   "message",
	}
	for _, provider := range []string{LogsProviderElasticsearch, LogsProviderOpenSearch} {
		got := LogFields{}.ResolvedFor(provider)
		if got != want {
			t.Fatalf("%s defaults = %+v, want %+v", provider, got, want)
		}
	}
	over := LogFields{LevelField: "severity", MessageField: "log.message"}.ResolvedFor(LogsProviderElasticsearch)
	if over.LevelField != "severity" || over.MessageField != "log.message" || over.TimestampField != "@timestamp" {
		t.Fatalf("overrides must win, defaults fill the rest: %+v", over)
	}
}

// TestLogsConfigIndexField confirms `logs.index` parses at the top level
// (Elasticsearch/OpenSearch-specific, ignored by victorialogs/loki).
func TestLogsConfigIndexField(t *testing.T) {
	const y = `
url: https://es:9200
provider: elasticsearch
index: logs-*
`
	var lc LogsConfig
	if err := yaml.Unmarshal([]byte(y), &lc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lc.Index != "logs-*" {
		t.Fatalf("index not parsed: %+v", lc)
	}
}
