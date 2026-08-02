// SPDX-License-Identifier: Apache-2.0

// Package grafana registers the "grafana" webhook source: Grafana Alerting's
// firing/resolved contact-point payload, mapped to investigations by
// delegating to the custom source's field-extraction mapper
// (internal/source/custom) with the documented default field paths baked
// in. It does not parse alert JSON itself — dot-path extraction, `items`
// batching, severity_map, resolved-event handling, per-instance tokens, the
// body cap and startup mapping validation all stay custom's job. The point
// is that Grafana users stop hand-copying nine field paths into
// sources.custom.instances.*, not that Grafana gets its own parser. Every
// default stays overridable via sources.grafana's own keys.
package grafana

import (
	"fmt"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/source"
	"github.com/Smana/runlore/internal/source/custom"
)

// instanceName is the fixed synthetic custom-instance key the merged mapping
// is compiled under, and the X-Runlore-Instance value stamped on every
// delegated call. Grafana has one mapping per RunLore deployment — the
// route is /webhook/grafana with no {instance} wildcard — unlike `custom`,
// which multiplexes several instances behind /webhook/custom/{instance}.
const instanceName = "grafana"

// defaultFields is the built-in Grafana → investigation field mapping. It
// must equal the table documented at
// website/content/docs/integrations/grafana.md — TestDefaultMappingMatchesDocs
// pins the two together so a change to either shows up as a visible test
// diff rather than a silent behaviour change.
var defaultFields = map[string]string{
	"title":         "labels.alertname",
	"message":       "annotations.summary",
	"severity":      "labels.severity",
	"namespace":     "labels.namespace",
	"workload_name": "labels.pod",
	"fingerprint":   "fingerprint",
	"resolved":      "status",
}

const (
	defaultItems  = "alerts" // Grafana's contact-point payload batches alerts under "alerts"
	defaultLabels = "labels"
)

// Source wraps a custom.Source pre-compiled with Grafana's baked-in mapping.
// All extraction, batching, severity mapping, resolved-event handling and
// per-instance auth live in custom.Source (see internal/source/custom); this
// type only fixes the delegated call's instance selector to instanceName,
// since /webhook/grafana carries no {instance} path wildcard for the core to
// stamp one from.
type Source struct {
	inner *custom.Source
}

// Decode implements source.WebhookSource by delegating to the compiled
// custom mapping.
func (s *Source) Decode(body []byte, h http.Header) (source.DecodeResult, error) {
	return s.inner.Decode(body, withInstance(h))
}

// Authenticate implements source.Authenticator, mirroring custom.Source: the
// delegated instance's own token_env (falling back to the shared
// server.webhook_token_env) gates the request before Decode ever runs.
func (s *Source) Authenticate(body []byte, h http.Header) bool {
	return s.inner.Authenticate(body, withInstance(h))
}

// withInstance stamps the fixed instance name the merged mapping was
// compiled under, so custom.Source's instance lookup resolves regardless of
// what the core did (or, on this wildcard-less route, did not) set on the
// inbound request.
func withInstance(h http.Header) http.Header {
	h2 := h.Clone()
	h2.Set(source.InstanceHeader, instanceName)
	return h2
}

// mergedNode renders the baked-in defaults overlaid by the user's
// sources.grafana block as the `instances: {grafana: ...}` yaml.Node
// custom.Build expects. It never inspects mapping semantics itself (dot
// paths, items, severity_map, ...) — assembling the config is all this
// function does; compiling and validating it stays custom's job.
func mergedNode(user yaml.Node) (yaml.Node, error) {
	var userMap map[string]any
	if err := user.Decode(&userMap); err != nil {
		return yaml.Node{}, fmt.Errorf("decode sources.grafana: %w", err)
	}

	fields := make(map[string]any, len(defaultFields))
	for k, v := range defaultFields {
		fields[k] = v
	}
	defaults := map[string]any{
		"items":  defaultItems,
		"labels": defaultLabels,
		"fields": fields,
	}
	merged := mergeMaps(defaults, userMap)
	outer := map[string]any{"instances": map[string]any{instanceName: merged}}

	// Round-trip through YAML text (rather than yaml.Node.Encode) to build the
	// node custom.Build expects — the same approach internal/source/custom's
	// own tests use (mustNode) to build a yaml.Node from a Go value.
	b, err := yaml.Marshal(outer)
	if err != nil {
		return yaml.Node{}, fmt.Errorf("marshal merged sources.grafana: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return yaml.Node{}, fmt.Errorf("reparse merged sources.grafana: %w", err)
	}
	return *doc.Content[0], nil
}

// mergeMaps overlays user onto defaults. A key present as a map on both
// sides is merged key-by-key — so overriding fields.workload_name leaves
// every other default field path in place — while every other key is
// wholesale-replaced when the user sets it. This is what makes "every field
// stays overridable" true without forcing an all-or-nothing block.
func mergeMaps(defaults, user map[string]any) map[string]any {
	out := make(map[string]any, len(defaults)+len(user))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range user {
		if dm, ok := out[k].(map[string]any); ok {
			if um, ok := v.(map[string]any); ok {
				merged := make(map[string]any, len(dm)+len(um))
				for kk, vv := range dm {
					merged[kk] = vv
				}
				for kk, vv := range um {
					merged[kk] = vv
				}
				out[k] = merged
				continue
			}
		}
		out[k] = v
	}
	return out
}

func init() {
	source.Register(source.Descriptor{
		Name: "grafana",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/grafana",
		Build: func(d source.Deps) (any, error) {
			node, ok := d.Raw["grafana"]
			if !ok {
				return nil, nil // disabled: no sources.grafana key
			}
			n, err := mergedNode(node)
			if err != nil {
				return nil, fmt.Errorf("sources.grafana: %w", err)
			}
			inner, err := custom.Build(n, d.Cfg)
			if err != nil {
				return nil, fmt.Errorf("sources.grafana: %w", err)
			}
			return &Source{inner: inner}, nil
		},
	})
}
