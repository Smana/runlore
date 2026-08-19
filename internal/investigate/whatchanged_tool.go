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
//
// "optionally a named workload" is what shipped, and it invited the call that cannot
// work: `name` is matched against the GitOps OBJECT's name, so the failing pod's name —
// which carries a ReplicaSet hash or a StatefulSet ordinal — can never match. A model
// reading "named workload" passes exactly that, gets an empty result, and the empty
// result used to read as "Git shows no change".
//
// The correction has a floor, though: a BARE workload name frequently DOES match, because
// Kustomizations and Applications are routinely named after the app they render, and both
// providers deliberately retry a name across all namespaces for that case. Telling the
// model a Deployment name never matches would lose that narrowing — the accurate claim is
// about the suffix, not about workload names.
func (t WhatChangedTool) Description() string {
	return "List what changed (GitOps revision history + the actual Git diff) for a namespace, " +
		"optionally narrowed to one GitOps object by name. name is matched against the Argo CD " +
		"Application / Flux Kustomization name — often the bare app name (e.g. \"vmagent\"), but " +
		"NEVER a pod or replicaset name carrying a hash or ordinal suffix (e.g. " +
		"\"vmagent-vmagent-0\"), which cannot match. Prefer the namespace alone; an unmatched " +
		"name lists the objects that do exist there."
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
	sel := providers.Selector{Namespace: in.Namespace, Name: in.Name}
	changes, err := t.GitOps.Changes(ctx, providers.TimeWindow{}, sel)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return t.unresolvedSelector(ctx, providers.TimeWindow{}, sel), nil
	}
	var b strings.Builder
	// B2: the provider resolves a workload namespace to its OWNING GitOps object,
	// which commonly lives elsewhere (Flux Kustomizations in flux-system, Argo
	// Applications in argocd). Flag that so a match in another namespace is never
	// misread as "the tool ignored my namespace". (This used to end "and 'no changes' stays
	// honest", naming a return string unresolvedSelector below has since replaced.)
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

// unresolvedSelector reports what the enumeration ESTABLISHED when it yielded no change,
// rather than asserting what Git does or does not contain.
//
// The shipped wording was "no changes found for the given selector" — a claim about Git
// history in reply to a question the tool may never have asked. Observed live: asked about a
// pod running on a cluster this Argo CD does not manage, the tool returned that string, and
// the model used it to rule out a GitOps cause, then recorded it as the finding's provenance
// and the entry's ONLY citation. The same finding's data-gaps section said the cluster was
// invisible — the investigation contradicted itself, and this sentence is what let it.
//
// THE ANSWER COMES FROM THE PROVIDER, the only place that can give it. An empty list means
// nothing matched, or something matched but was undiffable, or a source read was REFUSED —
// the first is about the request, the second about an object that exists, the third not about
// the cluster at all. providers.ChangesLookupReporter exists for exactly this.
//
// An earlier version of this fix guessed instead: it re-called Changes with a namespace-only
// selector and inferred from whether THAT was empty. Recording why, because the shape is
// tempting — it could not see a denial at all (so it filed one under "no resolvable source",
// reintroducing #503 inside the fix for #503), it asserted "this namespace IS managed" from a
// namespace match that ignores the destination CLUSTER, and it cost a second cluster-wide
// List plus a cold clone per source repo against the 60s tool timeout, to render names only.
//
// A provider without the capability gets the neutral answer, claiming no reason because none
// was established. Both real providers implement it.
func (t WhatChangedTool) unresolvedSelector(ctx context.Context, w providers.TimeWindow, sel providers.Selector) string {
	id := fmt.Sprintf("namespace %q", sel.Namespace)
	if sel.Name != "" {
		id += fmt.Sprintf(", name %q", sel.Name)
	}
	// changesWithLookup is shared with incident_timeline, the other Changes consumer that
	// rendered an empty list as a statement about Git. The enumeration already ran in Call;
	// this re-reads it to recover the reason rather than threading a second return value
	// through every GitOpsProvider caller. It repeats the List, not the resolution work: an
	// empty result means no object reached the revision/clone path, so there is nothing for
	// the repeat to redo.
	_, lk, err := changesWithLookup(ctx, t.GitOps, w, sel)
	if err != nil {
		return gitopsChangesAnswer(id, providers.Lookup{Reason: providers.LookupFailed})
	}
	return gitopsChangesAnswer(id, lk)
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
