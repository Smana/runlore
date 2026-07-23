// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/sourcerepo"
	"github.com/Smana/runlore/internal/whatchanged"
)

type fakeSourceDiffer struct {
	sc      whatchanged.SourceChanges
	err     error
	gotURL  string
	gotFrom string
	gotTo   string
}

func (f *fakeSourceDiffer) Source(_ context.Context, url, from, to string, _ int) (whatchanged.SourceChanges, error) {
	f.gotURL, f.gotFrom, f.gotTo = url, from, to
	return f.sc, f.err
}

func mustAllow(t *testing.T, patterns ...string) *sourcerepo.Allowlist {
	t.Helper()
	a, err := sourcerepo.New(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func fixtureChanges() whatchanged.SourceChanges {
	return whatchanged.SourceChanges{
		FromRef: "v1.2.2", ToRef: "v1.2.3",
		Commits: []whatchanged.SourceCommit{
			{SHA: "a1b2c3d4e5f60000000000000000000000000000", Subject: "fix: raise DB pool 10→50", When: time.Unix(1000, 0)},
		},
		Diff: providers.Diff{Files: []providers.FileDiff{
			{Path: "config/database.yml", Patch: "--- a/config/database.yml\n+++ b/config/database.yml\n-pool: 10\n+pool: 50\n"},
			{Path: "go.sum", Patch: "--- a/go.sum\n+++ b/go.sum\n" + strings.Repeat("+x\n", 200)},
		}},
	}
}

func TestSourceDiffRejectsNonAllowlistedRepo(t *testing.T) {
	tool := SourceDiffTool{Source: &fakeSourceDiffer{}, Allow: mustAllow(t, "github.com/acme/*")}
	_, err := tool.Call(context.Background(), `{"repo":"github.com/evil/x","from":"1","to":"2"}`)
	if err == nil || !strings.Contains(err.Error(), "github.com/acme/*") {
		t.Fatalf("want allowlist rejection naming the allowed patterns, got %v", err)
	}
}

func TestSourceDiffSummary(t *testing.T) {
	f := &fakeSourceDiffer{sc: fixtureChanges()}
	tool := SourceDiffTool{Source: f, Allow: mustAllow(t, "github.com/acme/*")}
	out, err := tool.Call(context.Background(), `{"repo":"github.com/acme/checkout","from":"1.2.2","to":"1.2.3"}`)
	if err != nil {
		t.Fatal(err)
	}
	if f.gotURL != "https://github.com/acme/checkout" {
		t.Fatalf("clone URL = %q, want the normalized allowlisted form", f.gotURL)
	}
	for _, want := range []string{
		"a1b2c3d", "fix: raise DB pool 10→50", // commit line
		"config/database.yml +1 -1", // diffstat
		"go.sum",                    // generated file still listed…
		"generated",                 // …and annotated
		"+pool: 50",                 // real hunk included
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "+x") {
		t.Fatalf("generated go.sum hunks leaked into the summary:\n%s", out)
	}
}

func TestSourceDiffZoom(t *testing.T) {
	f := &fakeSourceDiffer{sc: fixtureChanges()}
	tool := SourceDiffTool{Source: f, Allow: mustAllow(t, "github.com/acme/*")}
	out, err := tool.Call(context.Background(),
		`{"repo":"github.com/acme/checkout","from":"1.2.2","to":"1.2.3","paths":["go.sum","nope.txt"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "+x") {
		t.Fatal("zoom must return the requested file's hunks even for a generated file")
	}
	if !strings.Contains(out, "nope.txt") {
		t.Fatal("zoom must note a requested path that is not in the diff")
	}
}

func TestSourceDiffSummaryBudgetTruncates(t *testing.T) {
	sc := fixtureChanges()
	sc.Diff.Files = append(sc.Diff.Files, providers.FileDiff{
		Path: "big/real_file.go", Patch: "+++ b/big/real_file.go\n" + strings.Repeat("+padding line\n", 2000),
	})
	f := &fakeSourceDiffer{sc: sc}
	tool := SourceDiffTool{Source: f, Allow: mustAllow(t, "github.com/acme/*")}
	out, err := tool.Call(context.Background(), `{"repo":"github.com/acme/checkout","from":"1.2.2","to":"1.2.3"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "paths") || !strings.Contains(out, "zoom") {
		t.Fatalf("a budget-cut summary must tell the model paths-zoom is available:\n%.400s", out)
	}
}
