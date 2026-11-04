// SPDX-License-Identifier: Apache-2.0

package kbresolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeForge keeps one PR's entry in memory and records what Resolve asked of it.
type fakeForge struct {
	pr       providers.PREntry
	raw      string
	open     []providers.CuratedIssue // open runlore-resolve PRs
	openErr  error
	edits    int
	opened   []string // entry paths
	openBody string
	labels   []string
}

func (f *fakeForge) PREntry(context.Context, int) (providers.PREntry, error) { return f.pr, nil }

func (f *fakeForge) EditEntryOnPR(_ context.Context, _ int, _ string, edit func([]byte) ([]byte, bool, error)) (bool, error) {
	out, changed, err := edit([]byte(f.raw))
	if err != nil || !changed {
		return false, err
	}
	f.raw, f.edits = string(out), f.edits+1
	return true, nil
}

func (f *fakeForge) ListPRsByLabel(_ context.Context, label string) ([]providers.CuratedIssue, error) {
	f.labels = append(f.labels, label)
	return f.open, nil
}

func (f *fakeForge) OpenResolvePR(_ context.Context, path string, edit func([]byte) ([]byte, bool, error), body string) (providers.Ref, error) {
	if f.openErr != nil {
		return providers.Ref{}, f.openErr
	}
	if _, changed, err := edit([]byte(f.raw)); err != nil || !changed {
		return providers.Ref{}, errors.Join(providers.ErrAlreadyResolved, err)
	}
	f.opened, f.openBody = append(f.opened, path), body
	return providers.Ref{URL: "https://github.com/o/r/pull/9"}, nil
}

func TestResolveOpenPR(t *testing.T) {
	f := &fakeForge{pr: providers.PREntry{Path: "incidents/a.md", State: "open"}, raw: entry}
	got, err := Resolve(context.Background(), f, Target{PR: 157}, res)
	if err != nil || got.Route != "appended" || f.edits != 1 || !strings.Contains(f.raw, "fix_pr: "+res.FixPR) {
		t.Fatal(got, err, f.raw)
	}
	got, err = Resolve(context.Background(), f, Target{PR: 157}, res)
	if err != nil || got.Route != "unchanged" || f.edits != 1 {
		t.Fatal("the same call again changes nothing: ", got, err)
	}
}

func TestResolveMergedPR(t *testing.T) {
	f := &fakeForge{pr: providers.PREntry{Path: "incidents/a.md", State: "merged"}, raw: entry}
	got, err := Resolve(context.Background(), f, Target{PR: 158}, res)
	marker := "<!-- runlore:resolve:" + resolveSum("incidents/a.md", res) + " -->"
	if err != nil || got.Route != "edit-pr" || got.URL != "https://github.com/o/r/pull/9" || len(f.opened) != 1 || f.opened[0] != "incidents/a.md" || !strings.Contains(f.openBody, marker) {
		t.Fatal(got, err, f.opened, f.openBody)
	}
	if len(f.labels) != 1 || f.labels[0] != "runlore-resolve" {
		t.Fatal(f.labels)
	}

	// The PR is still open: its marker is found, no second PR.
	f2 := &fakeForge{pr: f.pr, raw: entry, open: []providers.CuratedIssue{{Number: 9, Body: "text\n" + marker}}}
	if got, err := Resolve(context.Background(), f2, Target{PR: 158}, res); err != nil || got.Route != "unchanged" || len(f2.opened) != 0 {
		t.Fatal(got, err)
	}
	// The edit merged: the entry already carries it.
	f3 := &fakeForge{pr: f.pr, raw: func() string { out, _, _ := Apply([]byte(entry), res); return string(out) }()}
	if got, err := Resolve(context.Background(), f3, Target{PR: 158}, res); err != nil || got.Route != "unchanged" || len(f3.opened) != 0 {
		t.Fatal(got, err)
	}
	// Another fix has another marker.
	other := with(res, func(r *providers.AppliedResolution) { r.FixPR = fixB })
	if resolveSum("incidents/a.md", other) == resolveSum("incidents/a.md", res) {
		t.Fatal("the marker covers path, fix and verdict")
	}
}

func TestResolveByPath(t *testing.T) {
	f := &fakeForge{pr: providers.PREntry{State: "open"}, raw: entry}
	got, err := Resolve(context.Background(), f, Target{Path: "incidents/a.md"}, res)
	if err != nil || got.Route != "edit-pr" || f.edits != 0 {
		t.Fatal("a path goes straight to the merged-entry route: ", got, err)
	}
	if _, err := Resolve(context.Background(), f, Target{}, res); err == nil {
		t.Fatal("a target names a PR or a path")
	}
}

func TestResolveClosedUnmerged(t *testing.T) {
	f := &fakeForge{pr: providers.PREntry{State: "closed"}, raw: entry}
	if _, err := Resolve(context.Background(), f, Target{PR: 1}, res); !errors.Is(err, ErrClosedUnmerged) {
		t.Fatal(err)
	}
}

func TestResolveRefusesAnUnsafeResolution(t *testing.T) {
	f := &fakeForge{pr: providers.PREntry{State: "open"}, raw: entry}
	bad := with(res, func(r *providers.AppliedResolution) { r.FixPR = "not a url" })
	if _, err := Resolve(context.Background(), f, Target{PR: 1}, bad); err == nil || f.edits != 0 {
		t.Fatal(err)
	}
}
