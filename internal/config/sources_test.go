// SPDX-License-Identifier: Apache-2.0

package config_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"

	// Blank-import the adapters so they self-register and their Build funcs run.
	_ "github.com/Smana/runlore/internal/source/alertmanager"
	_ "github.com/Smana/runlore/internal/source/gitops"
	_ "github.com/Smana/runlore/internal/source/pagerduty"
)

// fakeGitOps is a minimal GitOpsProvider so the gitops source Build (which requires
// a non-nil provider) can construct a watcher.
type fakeGitOps struct{}

func (fakeGitOps) WatchFailures(context.Context) (<-chan providers.FailureEvent, error) {
	return nil, nil
}
func (fakeGitOps) Changes(context.Context, providers.TimeWindow, providers.Selector) ([]providers.Change, error) {
	return nil, nil
}
func (fakeGitOps) Diff(context.Context, providers.Change) (providers.Diff, error) {
	return providers.Diff{}, nil
}

func loadSources(t *testing.T, doc string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return c
}

// TestSourcesMapEnablesAdapters verifies that sources.<name> drives adapter
// enablement: presence of `alertmanager` and `gitops.enabled` builds both, and an
// empty sources map builds neither.
func TestSourcesMapEnablesAdapters(t *testing.T) {
	c := loadSources(t, `
sources:
  alertmanager: {}
  gitops: { enabled: true }
  pagerduty: {}
`)
	built, err := source.BuildEnabled(source.Deps{Cfg: c, GitOps: fakeGitOps{}, Raw: c.Sources})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	names := map[string]bool{}
	for _, b := range built {
		names[b.Desc.Name] = true
	}
	if !names["alertmanager"] || !names["gitops"] || !names["pagerduty"] {
		t.Fatalf("want alertmanager+gitops+pagerduty built, got %v", names)
	}
}

func TestEmptySourcesBuildsNeither(t *testing.T) {
	c := loadSources(t, "actions: { mode: off }\n")
	built, err := source.BuildEnabled(source.Deps{Cfg: c, GitOps: fakeGitOps{}, Raw: c.Sources})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, b := range built {
		if b.Desc.Name == "alertmanager" || b.Desc.Name == "gitops" {
			t.Fatalf("no sources configured, but %q was built", b.Desc.Name)
		}
	}
}
