// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
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
