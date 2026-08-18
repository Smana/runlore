// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"gopkg.in/yaml.v3"

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
		// One cloud resource must have ONE spelling downstream. A vendor rule
		// templates whichever identifier it has to hand, so the same RDS instance
		// arrives as its bare DBInstanceIdentifier on one firing and as its full ARN
		// on the next — and Workload.Name is what the notification renders, what a
		// curated entry is indexed under, and what the recurrence key is built from.
		// Only an ARN is rewritten; a Kubernetes name passes through untouched, and
		// the raw value still travels on Labels.
		wname = providers.ARNResourceName(wname)
		var fps []string
		if fingerprint != "" {
			fps = []string{fingerprint}
		}
		out.Requests = append(out.Requests, investigate.Request{
			// Per-instance (see WithSource), not a package constant: Source is
			// part of investigate's workqueue coalescing key, so an adapter
			// delegating here must be able to claim its own value.
			Source:       inst.src,
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

// Build compiles a `sources.custom`-shaped yaml.Node (its `instances:` map)
// into a ready Source: parses and validates every instance's field mapping,
// resolves each instance's token (and the shared server.webhook_token_env
// fallback), and enforces actions.mode=auto's fail-closed token requirement.
// Exported so an adapter that delegates to this mapper with baked-in
// defaults (internal/source/grafana) reuses the same validation and
// token-resolution path instead of re-implementing it. Such an adapter passes
// WithConfigPath/WithSource so startup errors name the key the operator really
// wrote and its investigations stay distinct from a custom instance's; with no
// options this is exactly the plain `sources.custom` build.
func Build(node yaml.Node, cfg *config.Config, opts ...Option) (*Source, error) {
	o := resolveOptions(opts)
	insts, err := parseConfig(node, opts...)
	if err != nil {
		return nil, err
	}
	shared := ""
	if cfg != nil && cfg.Server.WebhookTokenEnv != "" {
		shared = osGetenv(cfg.Server.WebhookTokenEnv)
	}
	for name, inst := range insts {
		where := o.instancePath(name)
		if inst.tokenEnv != "" {
			inst.token = osGetenv(inst.tokenEnv)
			if inst.token == "" {
				return nil, fmt.Errorf("%s: token_env %q is empty", where, inst.tokenEnv)
			}
		}
		// Fail closed under mode=auto: an unattended executor must not
		// accept unauthenticated vendor webhooks (PagerDuty precedent).
		if cfg != nil && cfg.Actions.Mode == config.ActionAuto && inst.token == "" && shared == "" {
			return nil, fmt.Errorf("actions.mode=auto requires a token for %s (token_env or server.webhook_token_env)", where)
		}
	}
	return &Source{instances: insts, shared: shared}, nil
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
			return Build(node, d.Cfg)
		},
	})
}
