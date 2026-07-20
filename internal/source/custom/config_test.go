// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustNode(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0] // unwrap the document node
}

func TestParseConfigValid(t *testing.T) {
	n := mustNode(t, `
instances:
  grafana:
    items: alerts
    fields: {title: labels.alertname, severity: labels.severity, resolved: status}
    severity_map: {P1: critical}
    defaults: {environment: prod}
`)
	insts, err := parseConfig(n)
	if err != nil {
		t.Fatal(err)
	}
	g := insts["grafana"]
	if g == nil || g.items == nil || g.resolvedValue != "resolved" {
		t.Fatalf("compiled instance wrong: %+v", g)
	}
	if _, ok := g.fields["title"]; !ok {
		t.Fatal("title path not compiled")
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := []struct{ name, yml, wantErr string }{
		{"missing title", `
instances:
  a: {fields: {severity: s}}`, "fields.title is required"},
		{"bad path", `
instances:
  a: {fields: {title: "x["}}`, "unterminated index"},
		{"unknown instance key", `
instances:
  a: {fields: {title: t}, itms: alerts}`, "unknown key"},
		{"no instances", `instances: {}`, "at least one instance"},
	}
	for _, c := range cases {
		_, err := parseConfig(mustNode(t, c.yml))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err=%v, want containing %q", c.name, err, c.wantErr)
		}
	}
}
