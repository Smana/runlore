// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

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
	token         string // resolved at Build; see auth.go
}

// parseConfig compiles the sources.custom block. Every error aborts startup —
// a bad mapping must never silently drop alerts at ingest.
func parseConfig(node yaml.Node) (map[string]*instance, error) {
	// Loud unknown-key check per instance (node-level, since instanceCfg's plain
	// Decode is not strict).
	var rawRoot struct {
		Instances map[string]map[string]yaml.Node `yaml:"instances"`
	}
	if err := node.Decode(&rawRoot); err != nil {
		return nil, fmt.Errorf("decode sources.custom: %w", err)
	}
	for name, keys := range rawRoot.Instances {
		for k := range keys {
			if !instanceKeys[k] {
				return nil, fmt.Errorf("sources.custom.instances.%s: unknown key %q", name, k)
			}
		}
	}

	var c rootCfg
	if err := node.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode sources.custom: %w", err)
	}
	if len(c.Instances) == 0 {
		return nil, fmt.Errorf("sources.custom: at least one instance is required")
	}
	out := make(map[string]*instance, len(c.Instances))
	names := make([]string, 0, len(c.Instances))
	for n := range c.Instances {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic error order
	for _, name := range names {
		ic := c.Instances[name]
		if ic.Fields["title"] == "" {
			return nil, fmt.Errorf("sources.custom.instances.%s: fields.title is required", name)
		}
		inst := &instance{
			fields:        map[string]path{},
			resolvedValue: ic.ResolvedValue,
			defaults:      ic.Defaults,
			severityMap:   ic.SeverityMap,
			tokenEnv:      ic.TokenEnv,
		}
		if inst.resolvedValue == "" {
			inst.resolvedValue = "resolved"
		}
		for f, p := range ic.Fields {
			if !fieldNames[f] {
				return nil, fmt.Errorf("sources.custom.instances.%s: unknown field %q", name, f)
			}
			cp, err := parsePath(p)
			if err != nil {
				return nil, fmt.Errorf("sources.custom.instances.%s: fields.%s: %w", name, f, err)
			}
			inst.fields[f] = cp
		}
		if ic.Items != "" {
			cp, err := parsePath(ic.Items)
			if err != nil {
				return nil, fmt.Errorf("sources.custom.instances.%s: items: %w", name, err)
			}
			inst.items = cp
		}
		if ic.Labels != "" {
			cp, err := parsePath(ic.Labels)
			if err != nil {
				return nil, fmt.Errorf("sources.custom.instances.%s: labels: %w", name, err)
			}
			inst.labels = cp
		}
		out[name] = inst
	}
	return out, nil
}
