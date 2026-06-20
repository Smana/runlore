package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// WhatChangedTool exposes the GitOps "what changed" lens to the model: the change
// timeline for a namespace/workload, each with its diff.
type WhatChangedTool struct {
	GitOps providers.GitOpsProvider
}

// Name returns the tool name registered with the model.
func (t WhatChangedTool) Name() string { return "what_changed" }

// Description returns the human-readable tool description advertised to the model.
func (t WhatChangedTool) Description() string {
	return "List what changed (GitOps revision history + the actual Git diff) for a namespace, optionally a named workload."
}

// Schema returns the JSON Schema for the tool's arguments.
func (t WhatChangedTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"name":{"type":"string"}},"required":["namespace"]}`
}

// Call lists changes for the selector and renders each with its diff.
func (t WhatChangedTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	changes, err := t.GitOps.Changes(ctx, providers.TimeWindow{}, providers.Selector{Namespace: in.Namespace, Name: in.Name})
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "no changes found for the given selector", nil
	}
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%s %s/%s (%s): %s..%s\n", c.Engine, c.Workload.Kind, c.Workload.Name, c.Type, c.FromRev, c.ToRev)
		d, derr := t.GitOps.Diff(ctx, c)
		if derr != nil {
			fmt.Fprintf(&b, "  (diff error: %v)\n", derr)
			continue
		}
		for _, f := range d.Files {
			fmt.Fprintf(&b, "  --- %s\n%s\n", f.Path, f.Patch)
		}
	}
	return b.String(), nil
}
