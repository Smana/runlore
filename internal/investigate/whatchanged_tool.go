// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/whatchanged"
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
	// Clone each source repo at most once for this call: several changes on one
	// (mono)repo would otherwise each trigger a full clone. The cache owns the
	// clones and removes them when the call returns.
	ctx, done := whatchanged.WithCloneCache(ctx)
	defer done()
	var b strings.Builder
	for _, c := range changes {
		// F2: these workloads were DETECTED server-side (Flux/Argo + git) — record them
		// as observed so an action may legitimately target them.
		recordObserved(ctx, c.Workload)
		recordObserved(ctx, c.BlastRadius...)
		fmt.Fprintf(&b, "%s %s/%s (%s): %s..%s", c.Engine, c.Workload.Kind, c.Workload.Name, c.Type, c.FromRev, c.ToRev)
		// When the engine knows WHEN the change landed, say so — "deploy at 14:02,
		// first crash at 14:03" is the core change↔symptom correlation.
		if !c.When.IsZero() {
			fmt.Fprintf(&b, " at %s", c.When.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
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
