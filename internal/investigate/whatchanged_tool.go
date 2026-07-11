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
	// Clone each source repo at most once for this call: several changes on one
	// (mono)repo would otherwise each trigger a full clone. Set up BEFORE Changes()
	// so its enumeration clones (Differ.RevisionsInWindow/CommitTime) share the same
	// cache as the per-change Diff() clones below. The cache owns the clones and
	// removes them when the call returns.
	ctx, done := whatchanged.WithCloneCache(ctx)
	defer done()
	changes, err := t.GitOps.Changes(ctx, providers.TimeWindow{}, providers.Selector{Namespace: in.Namespace, Name: in.Name})
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "no changes found for the given selector", nil
	}
	var b strings.Builder
	// B2: the provider resolves a workload namespace to its OWNING GitOps object,
	// which commonly lives elsewhere (Flux Kustomizations in flux-system, Argo
	// Applications in argocd). Flag that so a match in another namespace is never
	// misread as "the tool ignored my namespace" — and "no changes" stays honest.
	if in.Namespace != "" && !anyInNamespace(changes, in.Namespace) {
		fmt.Fprintf(&b, "note: no GitOps object in namespace %q; matched by name across namespaces (the owning object lives elsewhere, e.g. flux-system/argocd)\n", in.Namespace)
	}
	rendered := 0
	for _, c := range changes {
		// Cap the number of changes rendered so a namespace with dozens of workloads
		// can't blow the tool budget; the tail is summarized, not silently dropped.
		if rendered >= maxChangesRendered {
			fmt.Fprintf(&b, "…and %d more changes (narrow with a workload name)\n", len(changes)-rendered)
			break
		}
		rendered++
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
		renderDiff(&b, d)
	}
	return b.String(), nil
}

const (
	// maxChangesRendered caps how many changes a single what_changed call renders in
	// full before summarizing the tail. A namespace-wide query can return many.
	maxChangesRendered = 20
	// maxFilesRendered caps files rendered per change. A Helm-vendoring commit can
	// touch hundreds of files; the diffstat still lists every file's +/− counts.
	maxFilesRendered = 25
	// maxPatchLines caps lines of an individual file's patch. A vendored chart can be
	// tens of thousands of diff lines; the loop-level byte cap would cut mid-hunk.
	maxPatchLines = 200
)

// renderDiff writes a bounded rendering of a change's diff: a diffstat header (per
// file: +added/−removed) first, then each file's patch capped at maxPatchLines with
// an explicit truncation marker, and at most maxFilesRendered files with a tail note
// (B3). This keeps the actual-diff strength while making the output intelligibly
// bounded rather than relying on the loop-level byte cap to cut mid-hunk.
func renderDiff(b *strings.Builder, d providers.Diff) {
	if len(d.Files) == 0 {
		return
	}
	b.WriteString("  diffstat:\n")
	// Split each file's patch once and reuse the lines for both the diffstat count
	// and the capped render, so a large patch isn't strings.Split twice per file.
	patchLines := make([][]string, len(d.Files))
	for i, f := range d.Files {
		patchLines[i] = strings.Split(strings.TrimRight(f.Patch, "\n"), "\n")
		add, del := countChanges(patchLines[i])
		fmt.Fprintf(b, "    %s (+%d/-%d)\n", f.Path, add, del)
	}
	for i, f := range d.Files {
		if i >= maxFilesRendered {
			fmt.Fprintf(b, "  …and %d more files (see diffstat above)\n", len(d.Files)-i)
			break
		}
		fmt.Fprintf(b, "  --- %s\n", f.Path)
		b.WriteString(capPatch(patchLines[i]))
	}
}

// countChanges counts added/removed lines in a unified-diff patch (lines beginning
// with a single + or -), ignoring the ---/+++ file headers.
func countChanges(lines []string) (added, removed int) {
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
			continue
		case strings.HasPrefix(ln, "+"):
			added++
		case strings.HasPrefix(ln, "-"):
			removed++
		}
	}
	return added, removed
}

// capPatch returns the patch lines trimmed to at most maxPatchLines, appending an
// explicit marker naming how many lines were dropped. The result always ends in a
// newline.
func capPatch(lines []string) string {
	if len(lines) <= maxPatchLines {
		return strings.Join(lines, "\n") + "\n"
	}
	kept := lines[:maxPatchLines]
	return strings.Join(kept, "\n") + fmt.Sprintf("\n  [file diff truncated: %d more lines]\n", len(lines)-maxPatchLines)
}

// anyInNamespace reports whether any change's owning workload is actually in ns.
func anyInNamespace(changes []providers.Change, ns string) bool {
	for _, c := range changes {
		if c.Workload.Namespace == ns {
			return true
		}
	}
	return false
}
