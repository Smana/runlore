package investigate

import (
	"context"
	"strings"
	"testing"

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
