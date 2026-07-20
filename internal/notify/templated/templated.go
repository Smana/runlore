// SPDX-License-Identifier: Apache-2.0

// Package templated is a config-only generic notifier: it renders a
// user-supplied Go text/template over notify.Payload and POSTs the result to
// any endpoint (Teams, Discord, ntfy, incident.io, …). One YAML block per
// target — no Go, no rebuild. The Investigation is already secret-redacted
// before any notifier runs (investigate.redactInvestigation), so templates
// only ever see redacted data.
package templated

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

var funcs = template.FuncMap{
	// toJSON is the escaping-correct way to splice a value into a JSON body.
	"toJSON": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"mulPct": func(f float64) float64 { return f * 100 },
}

type instanceCfg struct {
	Name        string `yaml:"name"`
	URLEnv      string `yaml:"url_env"`
	TokenEnv    string `yaml:"token_env"`
	ContentType string `yaml:"content_type"`
	Template    string `yaml:"template"`
}

type instance struct {
	name        string
	url         string
	token       string
	contentType string
	tmpl        *template.Template
}

// Notifier fans one delivery out to every configured template instance.
type Notifier struct {
	instances []instance
	client    *http.Client
}

var _ providers.Notifier = (*Notifier)(nil)

// Deliver is filled in by the delivery step (render, cap, POST). It is declared
// here so *Notifier satisfies providers.Notifier and the notifier can
// self-register at parse time.
func (n *Notifier) Deliver(context.Context, providers.Investigation) error {
	return fmt.Errorf("templated: delivery not implemented")
}

// build decodes and validates the notify.templated block. Schema/template
// errors are returned (⇒ BuildEnabled fails ⇒ serve refuses to start); an
// instance whose url_env is unset is silently disabled (webhook parity).
func build(node yaml.Node) (*Notifier, error) {
	var cfgs []instanceCfg
	if err := node.Decode(&cfgs); err != nil {
		return nil, fmt.Errorf("templated: %w", err)
	}
	seen := map[string]bool{}
	var ins []instance
	for i, c := range cfgs {
		if c.Name == "" || c.URLEnv == "" || c.Template == "" {
			return nil, fmt.Errorf("templated[%d]: name, url_env and template are required", i)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("templated: duplicate instance name %q", c.Name)
		}
		seen[c.Name] = true
		tmpl, err := template.New(c.Name).Funcs(funcs).Parse(c.Template)
		if err != nil {
			return nil, fmt.Errorf("templated %q: parse template: %w", c.Name, err)
		}
		url := os.Getenv(c.URLEnv)
		if url == "" {
			continue // env unset ⇒ this instance is disabled
		}
		ct := c.ContentType
		if ct == "" {
			ct = "application/json"
		}
		token := ""
		if c.TokenEnv != "" {
			token = os.Getenv(c.TokenEnv)
		}
		ins = append(ins, instance{name: c.Name, url: url, token: token, contentType: ct, tmpl: tmpl})
	}
	if len(ins) == 0 {
		return nil, nil // nothing enabled
	}
	return &Notifier{instances: ins, client: httpx.SecureClient(10 * time.Second)}, nil
}

func init() {
	notify.Register(notify.Descriptor{
		Name: "templated",
		Build: func(d notify.Deps) (providers.Notifier, error) {
			node, ok := d.Cfg.Notify.Extra["templated"]
			if !ok {
				return nil, nil
			}
			n, err := build(node)
			if err != nil || n == nil {
				return nil, err // typed-nil guard: never return (*Notifier)(nil) as a non-nil interface
			}
			return n, nil
		},
	})
}
