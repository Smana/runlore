// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"net/http"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/source"
)

func authSource(t *testing.T, instToken, sharedToken string) *Source {
	t.Helper()
	s := grafanaInstance(t)
	s.instances["grafana"].token = instToken
	s.shared = sharedToken
	return s
}

func bearer(instance, tok string) http.Header {
	h := hdr(instance)
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h
}

func TestAuthenticate(t *testing.T) {
	cases := []struct {
		name                string
		instToken, shared   string
		instance, presented string
		want                bool
	}{
		{"instance token ok", "sec1", "shared", "grafana", "sec1", true},
		{"instance token wrong", "sec1", "shared", "grafana", "shared", false}, // instance token set: shared no longer accepted
		{"fallback to shared", "", "shared", "grafana", "shared", true},
		{"fallback wrong", "", "shared", "grafana", "bad", false},
		{"both empty = open", "", "", "grafana", "", true},
		{"unknown instance fails closed", "sec1", "shared", "nope", "sec1", false},
	}
	for _, c := range cases {
		s := authSource(t, c.instToken, c.shared)
		if got := s.Authenticate(nil, bearer(c.instance, c.presented)); got != c.want {
			t.Errorf("%s: Authenticate = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildFailClosedUnderAuto(t *testing.T) {
	// Registered descriptor is package-global; drive Build via source.BuildEnabled
	// in an integration test OR call the registered Build through Registered().
	// Simplest: look it up.
	var build func(source.Deps) (any, error)
	for _, d := range source.Registered() {
		if d.Name == "custom" {
			build = d.Build
		}
	}
	if build == nil {
		t.Fatal("custom source not registered")
	}
	raw := map[string]yaml.Node{"custom": mustNode(t, `
instances:
  a: {fields: {title: t}}
`)}
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionAuto
	if _, err := build(source.Deps{Cfg: cfg, Raw: raw}); err == nil {
		t.Fatal("want fail-closed error under mode=auto with no token")
	}
	// And with a token it builds.
	t.Setenv("CUSTOM_TOK", "s3cret")
	raw["custom"] = mustNode(t, `
instances:
  a: {token_env: CUSTOM_TOK, fields: {title: t}}
`)
	impl, err := build(source.Deps{Cfg: cfg, Raw: raw})
	if err != nil || impl == nil {
		t.Fatalf("build with token: impl=%v err=%v", impl, err)
	}
}
