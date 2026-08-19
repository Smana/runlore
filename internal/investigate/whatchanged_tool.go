// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
	changes, err := t.GitOps.Changes(ctx, providers.TimeWindow{}, providers.Selector{Namespace: in.Namespace, Name: in.Name})
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return t.unresolvedSelector(ctx, in.Namespace, in.Name), nil
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

// unresolvedSelector reports what the lookup ESTABLISHED when the engine yielded no
// change, rather than asserting what Git does or does not contain.
//
// The shipped wording was "no changes found for the given selector" — a claim about
// Git history in answer to a question this tool may never have asked. Observed live:
// asked about a pod running on a cluster this Argo CD does not manage, the tool
// returned that string, and the model put it in the finding's `provenance`, made it
// the entry's ONLY citation, and ruled out a config change on it. The same finding's
// data-gaps section stated correctly that the cluster was invisible to the cluster
// tools — the investigation contradicted itself, and this sentence is what let it.
//
// An empty result has three causes and none of them is "the repository shows no change":
//
//  1. Nothing matched the selector. No GitOps object carries that name/namespace, so no
//     repoURL was ever resolved and no clone happened. This is the ORDINARY case for a
//     workload-level name: Applications and Kustomizations are named after apps, not
//     after pods, so `name: vmagent-vmagent-0` cannot match one.
//  2. Objects matched but none was diffable — no applied revision, no sourceRef, or an
//     unresolvable source. Both providers `continue` past those.
//  3. Matched, diffable, and genuinely nothing in the window. Unreachable from here: a
//     zero TimeWindow makes both providers fall back to emitting the current revision
//     for every object they keep, so a match always produces at least one Change.
//
// Cases 1 and 2 are not separable from outside the provider, and picking one would be
// the same over-claim in a new direction. What IS establishable is that the engine
// produced no diffable object, so no repository was searched for that selector.
//
// When a name narrowed the lookup, one namespace-only probe separates "this engine
// manages nothing in that namespace" from "it does, and your name is not one of its
// objects" — and the second can then list the names that do exist, the same
// recover-don't-guess shape alert_rule uses for an unmatched alertname.
//
// THE PROBE IS NOT FREE, and an earlier version of this comment claimed the opposite. It
// said the caller's clone cache made any source resolution shared — but flux's changesFor
// sets `c.When = changeTime(...)` immediately before appending, with no `continue` between
// them, so an empty result PROVES changeTime never ran and the cache is cold. Every clone
// the probe triggers is therefore a fresh one, against the 60s ToolTimeout, and on flux it
// costs a cluster-wide List plus a GetGitRepository and a CommitTime clone per source repo
// — to render names only. On Argo CD it is a duplicate List and no git I/O at all, so the
// cost is asymmetric between the two engines. The durable fix is for the provider to report
// what it established, which removes this call entirely.
func (t WhatChangedTool) unresolvedSelector(ctx context.Context, namespace, name string) string {
	// Hoisted before sel is built: with no name the original lookup was ALREADY
	// namespace-only, so the probe would re-ask the identical question for nothing, and
	// sel's name half can never apply.
	if name == "" {
		return fmt.Sprintf("no GitOps object resolved for namespace %q: this engine reports no "+
			"diffable object there%s", namespace, unresolvedCauses+notAConfigStatement+gitopsNextStep)
	}
	sel := fmt.Sprintf("namespace %q, name %q", namespace, name)
	peers, err := t.GitOps.Changes(ctx, providers.TimeWindow{}, providers.Selector{Namespace: namespace})
	switch {
	case err != nil:
		// The probe is an aid, never a gate: its failure must not fail the investigation,
		// and must not fall back to the absence claim this function exists to remove. Say
		// which half is unestablished instead of guessing at it — so this branch alone does
		// NOT carry unresolvedCauses, because whether the namespace is managed is unknown.
		return fmt.Sprintf("no GitOps object resolved for %s, and no repository was searched. "+
			"The follow-up namespace-only probe FAILED (%v), so whether this engine manages "+
			"anything there is unestablished.%s%s", sel, err, notAConfigStatement, gitopsNextStep)
	case len(peers) == 0:
		return fmt.Sprintf("no GitOps object resolved for %s, and a namespace-only retry found "+
			"none either: this engine reports no diffable object there%s", sel,
			unresolvedCauses+notAConfigStatement+gitopsNextStep)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the name %q did not resolve to a GitOps object, so no repository was "+
		"searched for it — but namespace %q IS managed by this engine, so re-call with the "+
		"namespace alone, or with one of the bare object names below.%s\n",
		name, namespace, notAConfigStatement)
	names := peerNames(peers)
	renderRows(&b, len(names), "more", func(i int) { fmt.Fprintf(&b, "  %s\n", names[i]) })
	return b.String()
}

// The three fragments every unresolved-selector answer is composed from. They were four
// hand-written variants of the same sentences, which had already drifted before review —
// one pair differed only in "unchanged — do not cite" versus "unchanged; do not cite", and
// two of the four silently dropped the recovery advice entirely, on exactly the branches
// reached when a name was given and the model most needs a next step.
const (
	// unresolvedCauses names the causes an empty result CAN have, as possibilities. It is
	// omitted on the probe-error branch, where the namespace question is unestablished.
	//
	// It is deliberately not exhaustive in one respect worth knowing: flux's changesFor
	// swallows a GetGitRepository error with a bare `continue`, so an RBAC Forbidden on the
	// source reaches here indistinguishable from a benign skip and is filed under "no
	// applied revision or resolvable source". That is #503's mechanism — a denial collapsed
	// into an absence — surviving one layer down, and it cannot be fixed from the tool:
	// providers.LookupDenied exists for it and only the provider can set it.
	unresolvedCauses = " — nothing managed there, nothing with an applied revision and a " +
		"resolvable source, or a source read this engine was refused and skipped. A resource " +
		"on a cluster this engine does not manage reads exactly like this too."

	// notAConfigStatement is the load-bearing caveat, in the package's established shape:
	// "says NOTHING about whether X" (gitops_kinds.go, resource_spec_tool.go), which the
	// guard tests assert the PRESENCE of. A blocklist of forbidden phrasings is the retired
	// style — gitops_kinds.go records that "no such object exists" slipped straight past one.
	notAConfigStatement = " No repository was searched, so this says NOTHING about whether " +
		"the configuration changed — do not cite it as evidence against a config cause."

	// gitopsNextStep is the recovery half. It names a LISTING call rather than asking the
	// model to invent an object name: gitops_resource_status needs kind+name+namespace, so
	// pointing at it here would invite a guess, and a guessed resource that later shows up
	// as an action target is what observedresources.go flags as possibly hallucinated.
	gitopsNextStep = " To establish anything about the config, re-call with the namespace " +
		"alone to list what this engine does manage, or confirm the resource is managed by a " +
		"GitOps engine this deployment can see at all."
)

// peerNames renders each change's owning object as "engine Kind namespace/name" via
// Workload.Ref, the canonical identity form, deduplicated and sorted so the list is stable
// across calls: several in-window revisions of one object arrive as several Changes, and
// the model is being offered names to re-call with, not a change count.
func peerNames(changes []providers.Change) []string {
	seen := make(map[string]struct{}, len(changes))
	for _, c := range changes {
		seen[fmt.Sprintf("%s %s %s", c.Engine, c.Workload.Kind, c.Workload.Ref())] = struct{}{}
	}
	return slices.Sorted(maps.Keys(seen))
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
