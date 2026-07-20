// SPDX-License-Identifier: Apache-2.0

package templated

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeExtra(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0] // unwrap the document node
}

func TestBuildParsesInstances(t *testing.T) {
	t.Setenv("T_URL", "https://example.com/hook")
	node := decodeExtra(t, `
- name: teams
  url_env: T_URL
  template: '{"text": {{ toJSON .Title }}}'
`)
	n, err := build(node)
	if err != nil || n == nil || len(n.instances) != 1 {
		t.Fatalf("n=%+v err=%v", n, err)
	}
	if got := n.instances[0].contentType; got != "application/json" {
		t.Errorf("default content_type = %q", got)
	}
}

func TestBuildFailsClosedOnBadConfig(t *testing.T) {
	t.Setenv("T_URL", "https://example.com/hook")
	for name, y := range map[string]string{
		"parse error":    "- {name: a, url_env: T_URL, template: '{{ .Title }'}",
		"missing name":   "- {url_env: T_URL, template: ok}",
		"missing url":    "- {name: a, template: ok}",
		"missing tmpl":   "- {name: a, url_env: T_URL}",
		"duplicate name": "- {name: a, url_env: T_URL, template: ok}\n- {name: a, url_env: T_URL, template: ok}",
	} {
		if _, err := build(decodeExtra(t, y)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestBuildDisablesInstanceOnUnsetEnv(t *testing.T) {
	node := decodeExtra(t, "- {name: a, url_env: T_UNSET_NEVER, template: ok}")
	n, err := build(node)
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Errorf("all instances env-disabled ⇒ nil notifier, got %+v", n)
	}
}
