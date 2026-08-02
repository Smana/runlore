// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/investigate"
)

// Option tunes Build/parseConfig for an adapter that delegates to this mapper
// but exposes its OWN config surface (internal/source/grafana). Passing no
// options yields plain `sources.custom` behaviour, byte for byte — the custom
// source's own registration passes none.
type Option func(*options)

// options is the resolved form of the Option list.
type options struct {
	// block names the config key the whole mapping block was read from, for
	// errors that are not about one instance ("at least one instance is
	// required", decode failures).
	block string
	// instancePath renders the operator-facing config path of ONE instance's
	// keys. It is a function rather than a prefix string because the two
	// callers disagree on shape: `custom` multiplexes named instances
	// (sources.custom.instances.<name>) while an adapter compiles a single
	// synthetic instance under a flat key (sources.grafana) whose name the
	// operator never typed and must therefore never see in an error.
	instancePath func(name string) string
	// src is stamped on every investigate.Request this mapping produces. It is
	// part of investigate's workqueue coalescing key, so an adapter MUST claim
	// its own value or its incidents silently coalesce with same-titled ones
	// from a plain custom instance.
	src investigate.Source
}

// resolveOptions applies opts over the `sources.custom` defaults.
func resolveOptions(opts []Option) options {
	o := options{
		block:        "sources.custom",
		instancePath: func(name string) string { return "sources.custom.instances." + name },
		src:          investigate.SourceCustom,
	}
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// WithConfigPath makes every startup error name path — the key the OPERATOR
// actually wrote — instead of the synthetic
// `sources.custom.instances.<name>` this mapper compiles the mapping under.
// Without it a typo under `sources.grafana` fails closed loudly but points at
// a path that does not exist in the operator's file.
func WithConfigPath(path string) Option {
	return func(o *options) {
		o.block = path
		o.instancePath = func(string) string { return path }
	}
}

// WithSource stamps s on every investigate.Request instead of
// investigate.SourceCustom, so an adapter's investigations stay distinct from
// a custom instance's in the workqueue's coalescing key (and in logs).
func WithSource(s investigate.Source) Option {
	return func(o *options) { o.src = s }
}

// instanceCfg is the raw yaml shape of one sources.custom.instances entry.
type instanceCfg struct {
	TokenEnv      string            `yaml:"token_env"`
	Items         string            `yaml:"items"`
	Fields        map[string]string `yaml:"fields"`
	ResolvedValue string            `yaml:"resolved_value"`
	Labels        string            `yaml:"labels"`
	Defaults      map[string]string `yaml:"defaults"`
	SeverityMap   map[string]string `yaml:"severity_map"`
}

type rootCfg struct {
	Instances map[string]instanceCfg `yaml:"instances"`
}

// fieldNames is the closed set of extractable Request fields.
var fieldNames = map[string]bool{
	"title": true, "message": true, "severity": true, "namespace": true,
	"workload_kind": true, "workload_name": true, "environment": true,
	"fingerprint": true, "resolved": true,
}

// instanceKeys is the closed set of per-instance config keys, for the loud
// unknown-key check (mirrors source.BuildEnabled's typo philosophy: a typo'd
// key must abort startup, not silently disable a mapping).
var instanceKeys = map[string]bool{
	"token_env": true, "items": true, "fields": true, "resolved_value": true,
	"labels": true, "defaults": true, "severity_map": true,
}

// instance is the compiled (validated, path-parsed) form used at decode time.
type instance struct {
	fields        map[string]path
	items         path // nil = single event at body root
	resolvedValue string
	labels        path // nil = none
	defaults      map[string]string
	severityMap   map[string]string
	tokenEnv      string
	token         string             // resolved at Build; see auth.go
	src           investigate.Source // stamped on every Request; see options.src
}

// parseConfig compiles the sources.custom block. Every error aborts startup —
// a bad mapping must never silently drop alerts at ingest. opts (see Option)
// only change how errors NAME the config and which source the compiled
// mapping claims; the mapping semantics are identical either way.
func parseConfig(node yaml.Node, opts ...Option) (map[string]*instance, error) {
	o := resolveOptions(opts)
	// Loud unknown-key check per instance (node-level, since instanceCfg's plain
	// Decode is not strict).
	var rawRoot struct {
		Instances map[string]map[string]yaml.Node `yaml:"instances"`
	}
	if err := node.Decode(&rawRoot); err != nil {
		return nil, fmt.Errorf("decode %s: %w", o.block, err)
	}
	for name, keys := range rawRoot.Instances {
		for k := range keys {
			if !instanceKeys[k] {
				return nil, fmt.Errorf("%s: unknown key %q", o.instancePath(name), k)
			}
		}
	}

	var c rootCfg
	if err := node.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", o.block, err)
	}
	if len(c.Instances) == 0 {
		return nil, fmt.Errorf("%s: at least one instance is required", o.block)
	}
	out := make(map[string]*instance, len(c.Instances))
	names := make([]string, 0, len(c.Instances))
	for n := range c.Instances {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic error order
	for _, name := range names {
		ic := c.Instances[name]
		where := o.instancePath(name)
		if ic.Fields["title"] == "" {
			return nil, fmt.Errorf("%s: fields.title is required", where)
		}
		inst := &instance{
			fields:        map[string]path{},
			resolvedValue: ic.ResolvedValue,
			defaults:      ic.Defaults,
			severityMap:   ic.SeverityMap,
			tokenEnv:      ic.TokenEnv,
			src:           o.src,
		}
		if inst.resolvedValue == "" {
			inst.resolvedValue = "resolved"
		}
		for f, p := range ic.Fields {
			if !fieldNames[f] {
				return nil, fmt.Errorf("%s: unknown field %q", where, f)
			}
			cp, err := parsePath(p)
			if err != nil {
				return nil, fmt.Errorf("%s: fields.%s: %w", where, f, err)
			}
			inst.fields[f] = cp
		}
		if ic.Items != "" {
			cp, err := parsePath(ic.Items)
			if err != nil {
				return nil, fmt.Errorf("%s: items: %w", where, err)
			}
			inst.items = cp
		}
		if ic.Labels != "" {
			cp, err := parsePath(ic.Labels)
			if err != nil {
				return nil, fmt.Errorf("%s: labels: %w", where, err)
			}
			inst.labels = cp
		}
		out[name] = inst
	}
	return out, nil
}
