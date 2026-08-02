// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

// TestDefaultErrorsStillNameTheCustomPath is the other half of the
// config-path guard in internal/source/grafana: making an adapter's errors
// name its own key must not change what a PLAIN sources.custom instance
// reports. A typo under sources.custom.instances.a still has to say exactly
// that — the operator's own path.
func TestDefaultErrorsStillNameTheCustomPath(t *testing.T) {
	cases := []struct{ name, yml string }{
		{"unknown key", `
instances:
  a: {fields: {title: t}, feilds: x}`},
		{"missing title", `
instances:
  a: {fields: {severity: s}}`},
		{"bad field path", `
instances:
  a: {fields: {title: "x["}}`},
		{"bad items path", `
instances:
  a: {fields: {title: t}, items: "x["}`},
		{"bad labels path", `
instances:
  a: {fields: {title: t}, labels: "x["}`},
	}
	for _, c := range cases {
		_, err := parseConfig(mustNode(t, c.yml))
		if err == nil {
			t.Errorf("%s: want an error, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "sources.custom.instances.a") {
			t.Errorf("%s: error %q must name sources.custom.instances.a", c.name, err)
		}
	}
}

// TestDefaultBuildErrorsNameTheCustomPath covers the two messages built in
// Build rather than parseConfig (token_env resolution and the mode=auto
// fail-closed check).
func TestDefaultBuildErrorsNameTheCustomPath(t *testing.T) {
	node := mustNode(t, `
instances:
  a: {token_env: CUSTOM_TOK_DEFINITELY_UNSET, fields: {title: t}}
`)
	_, err := Build(node, &config.Config{})
	if err == nil || !strings.Contains(err.Error(), "sources.custom.instances.a: token_env") {
		t.Errorf("empty token_env error = %v, want it to name sources.custom.instances.a", err)
	}

	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionAuto
	_, err = Build(mustNode(t, `
instances:
  a: {fields: {title: t}}
`), cfg)
	if err == nil || !strings.Contains(err.Error(), "sources.custom.instances.a") {
		t.Errorf("mode=auto fail-closed error = %v, want it to name sources.custom.instances.a", err)
	}
}

// TestWithConfigPathAndSource pins the two knobs an adapter relies on, at the
// package that owns them: errors name the adapter's key (never the synthetic
// custom path), and requests carry the adapter's source.
func TestWithConfigPathAndSource(t *testing.T) {
	_, err := parseConfig(mustNode(t, `
instances:
  grafana: {fields: {title: t}, feilds: x}
`), WithConfigPath("sources.grafana"))
	if err == nil {
		t.Fatal("want an unknown-key error")
	}
	if !strings.Contains(err.Error(), "sources.grafana") || strings.Contains(err.Error(), "sources.custom") {
		t.Errorf("error = %q, want it to name sources.grafana and never sources.custom", err)
	}

	insts, err := parseConfig(mustNode(t, `
instances:
  grafana: {fields: {title: t}}
`), WithSource(investigate.SourceGrafana))
	if err != nil {
		t.Fatal(err)
	}
	if got := insts["grafana"].src; got != investigate.SourceGrafana {
		t.Errorf("compiled source = %q, want %q", got, investigate.SourceGrafana)
	}

	// No options at all: unchanged sources.custom semantics.
	insts, err = parseConfig(mustNode(t, `
instances:
  a: {fields: {title: t}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := insts["a"].src; got != investigate.SourceCustom {
		t.Errorf("default compiled source = %q, want %q", got, investigate.SourceCustom)
	}
}
