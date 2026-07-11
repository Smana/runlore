// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// fakeGitOps returns canned changes/diffs.
type fakeGitOps struct {
	changes []providers.Change
	diff    providers.Diff
}

func (f fakeGitOps) Changes(context.Context, providers.TimeWindow, providers.Selector) ([]providers.Change, error) {
	return f.changes, nil
}
func (f fakeGitOps) Diff(context.Context, providers.Change) (providers.Diff, error) {
	return f.diff, nil
}
func (f fakeGitOps) WatchFailures(context.Context) (<-chan providers.FailureEvent, error) {
	ch := make(chan providers.FailureEvent)
	close(ch)
	return ch, nil
}

func TestWhatChangedTool(t *testing.T) {
	gp := fakeGitOps{
		changes: []providers.Change{{
			Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
			Engine:   providers.EngineFlux, Type: providers.ChangeSync, FromRev: "aaa", ToRev: "bbb",
		}},
		diff: providers.Diff{Files: []providers.FileDiff{{Path: "apps/harbor/values.yaml", Patch: "+version: 1.15.0"}}},
	}
	tool := WhatChangedTool{GitOps: gp}
	out, err := tool.Call(context.Background(), `{"namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "apps") || !strings.Contains(out, "bbb") || !strings.Contains(out, "version: 1.15.0") {
		t.Fatalf("tool output missing expected content:\n%s", out)
	}
	if tool.Name() != "what_changed" {
		t.Fatalf("unexpected name %q", tool.Name())
	}
}

// TestWhatChangedToolRendersWhen asserts a change's timestamp reaches the model
// when the engine knows it — "deploy at 14:02, first crash at 14:03" is the core
// change↔symptom correlation — and that a zero When (Flux today) renders nothing.
func TestWhatChangedToolRendersWhen(t *testing.T) {
	when := time.Date(2026, 7, 1, 14, 2, 0, 0, time.UTC)
	gp := fakeGitOps{changes: []providers.Change{
		{
			Workload: providers.Workload{Kind: "Application", Name: "web", Namespace: "argocd"},
			Engine:   providers.EngineArgoCD, Type: providers.ChangeSync, ToRev: "ccc", When: when,
		},
		{
			Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
			Engine:   providers.EngineFlux, Type: providers.ChangeSync, ToRev: "bbb",
		},
	}}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(), `{"namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "at 2026-07-01T14:02:00Z") {
		t.Fatalf("change timestamp missing:\n%s", out)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Fatalf("zero When must not render:\n%s", out)
	}
}

// TestWhatChangedToolCrossNamespaceNote asserts B2: when the resolved change's
// owning object lives in a DIFFERENT namespace than the one queried (the flux-system
// / argocd bootstrap layout), the tool prefixes an explicit note so a cross-namespace
// match is never misread — and "no changes" can never be a silent false negative.
func TestWhatChangedToolCrossNamespaceNote(t *testing.T) {
	gp := fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "harbor", Namespace: "flux-system"},
		Engine:   providers.EngineFlux, Type: providers.ChangeSync, ToRev: "bbb",
	}}}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(), `{"namespace":"harbor","name":"harbor"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "note:") || !strings.Contains(out, `namespace "harbor"`) {
		t.Fatalf("expected a cross-namespace resolution note:\n%s", out)
	}
}

// TestWhatChangedToolBoundsDiff asserts B3: a big multi-file diff renders a
// diffstat, caps each file's patch with an explicit truncation marker, and caps the
// number of files with a tail note — the actual-diff strength stays, bounded.
func TestWhatChangedToolBoundsDiff(t *testing.T) {
	// One huge file (well over the per-file line cap) + more files than the file cap.
	var huge strings.Builder
	for i := 0; i < maxPatchLines+500; i++ {
		fmt.Fprintf(&huge, "+line %d\n", i)
	}
	files := []providers.FileDiff{{Path: "big/values.yaml", Patch: huge.String()}}
	for i := 0; i < maxFilesRendered+10; i++ {
		files = append(files, providers.FileDiff{Path: fmt.Sprintf("vendor/chart-%d.yaml", i), Patch: "+a\n-b\n"})
	}
	gp := fakeGitOps{
		changes: []providers.Change{{
			Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
			Engine:   providers.EngineFlux, Type: providers.ChangeSync, ToRev: "bbb",
		}},
		diff: providers.Diff{Files: files},
	}
	out, err := WhatChangedTool{GitOps: gp}.Call(context.Background(), `{"namespace":"flux-system"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "diffstat:") {
		t.Fatalf("expected a diffstat header:\n%s", out[:min(len(out), 500)])
	}
	// Diffstat lists every file's counts (even beyond the render cap).
	if !strings.Contains(out, "big/values.yaml (+") || !strings.Contains(out, fmt.Sprintf("vendor/chart-%d.yaml", maxFilesRendered+5)) {
		t.Fatal("diffstat must enumerate every file with +/- counts")
	}
	if !strings.Contains(out, "[file diff truncated:") {
		t.Fatalf("per-file patch cap marker missing:\n%s", out)
	}
	if !strings.Contains(out, "more files") {
		t.Fatalf("file-count cap tail note missing:\n%s", out)
	}
	// The huge patch's late lines must not appear in full (only its first cap lines).
	if strings.Contains(out, "+line 690") {
		t.Fatalf("per-file patch was not capped — late lines present")
	}
}
