// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"
)

// Source is the generic webhook source adapter. One Source serves every
// configured instance; the core-stamped source.InstanceHeader selects which
// mapping applies to a delivery (see source.Built.Handler).
type Source struct {
	instances map[string]*instance
	shared    string // shared server.webhook_token_env value, resolved at Build
}

// osGetenv is a package-level indirection for os.Getenv (PagerDuty precedent;
// tests set real env vars via t.Setenv).
var osGetenv = os.Getenv

// Decode maps one delivery through the instance's field paths. A single
// non-conforming event is skipped (fail-safe: one junk element must not void a
// batch); an unknown instance is an error (→ 400) — it means a route/config
// mismatch, not vendor noise.
func (s *Source) Decode(body []byte, h http.Header) (source.DecodeResult, error) {
	name := h.Get(source.InstanceHeader)
	inst, ok := s.instances[name]
	if !ok {
		return source.DecodeResult{}, fmt.Errorf("custom: unknown instance %q", name)
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return source.DecodeResult{}, fmt.Errorf("custom/%s: decode body: %w", name, err)
	}

	events := []any{doc}
	if inst.items != nil {
		v, ok := inst.items.lookup(doc)
		if !ok {
			return source.DecodeResult{}, fmt.Errorf("custom/%s: items path yields nothing", name)
		}
		arr, ok := v.([]any)
		if !ok {
			return source.DecodeResult{}, fmt.Errorf("custom/%s: items path is not an array", name)
		}
		events = arr
	}

	var out source.DecodeResult
	for _, ev := range events {
		get := func(field string) string {
			p, ok := inst.fields[field]
			if !ok {
				return inst.defaults[field]
			}
			v, found := p.lookup(ev)
			if !found {
				return inst.defaults[field]
			}
			s, ok := coerce(v)
			if !ok {
				return inst.defaults[field]
			}
			return s
		}

		fingerprint := get("fingerprint")
		if get("resolved") == inst.resolvedValue {
			if fingerprint != "" { // a resolution without identity cannot be attributed
				out.Resolved = append(out.Resolved, source.Resolution{Fingerprint: fingerprint, At: time.Now()})
			}
			continue
		}
		title := get("title")
		if title == "" {
			continue // fail-safe: skip the event, keep the batch
		}
		severity := get("severity")
		if mapped, ok := inst.severityMap[severity]; ok {
			severity = mapped
		}
		labels := map[string]string{"instance": name}
		if inst.labels != nil {
			if v, found := inst.labels.lookup(ev); found {
				if m, ok := v.(map[string]any); ok {
					for k, lv := range m {
						if s, ok := coerce(lv); ok {
							labels[k] = s
						}
					}
				}
			}
		}
		ns, kind, wname := get("namespace"), get("workload_kind"), get("workload_name")
		var fps []string
		if fingerprint != "" {
			fps = []string{fingerprint}
		}
		out.Requests = append(out.Requests, investigate.Request{
			Source:       investigate.SourceCustom,
			Title:        title,
			Severity:     severity,
			Environment:  get("environment"),
			Workload:     providers.Workload{Namespace: ns, Kind: kind, Name: wname},
			Reason:       severity,
			Message:      get("message"),
			Labels:       labels,
			At:           time.Now(),
			Fingerprint:  fingerprint,
			Fingerprints: fps,
			// Instance takes the cluster slot (PagerDuty precedent: its service
			// does) so two vendors reporting the same workload stay distinct.
			TriggerKey: curator.IncidentKey(title, ns, kind, wname, name),
		})
	}
	return out, nil
}

func init() {
	source.Register(source.Descriptor{
		Name: "custom",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/custom/{instance}",
		Build: func(d source.Deps) (any, error) {
			node, ok := d.Raw["custom"]
			if !ok {
				return nil, nil // disabled: no sources.custom key
			}
			insts, err := parseConfig(node)
			if err != nil {
				return nil, err
			}
			shared := ""
			if d.Cfg != nil && d.Cfg.Server.WebhookTokenEnv != "" {
				shared = osGetenv(d.Cfg.Server.WebhookTokenEnv)
			}
			for name, inst := range insts {
				if inst.tokenEnv != "" {
					inst.token = osGetenv(inst.tokenEnv)
					if inst.token == "" {
						return nil, fmt.Errorf("sources.custom.instances.%s: token_env %q is empty", name, inst.tokenEnv)
					}
				}
				// Fail closed under mode=auto: an unattended executor must not
				// accept unauthenticated vendor webhooks (PagerDuty precedent).
				if d.Cfg != nil && d.Cfg.Actions.Mode == config.ActionAuto && inst.token == "" && shared == "" {
					return nil, fmt.Errorf("actions.mode=auto requires a token for sources.custom.instances.%s (token_env or server.webhook_token_env)", name)
				}
			}
			return &Source{instances: insts, shared: shared}, nil
		},
	})
}
