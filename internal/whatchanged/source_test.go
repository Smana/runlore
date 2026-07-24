// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// tagRepo stamps a lightweight tag on a commit in the buildRepo fixture.
func tagRepo(t *testing.T, dir, name string, h plumbing.Hash) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateTag(name, h, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSourceByTags(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	tagRepo(t, dir, "v1.0.0", v1)
	tagRepo(t, dir, "v1.1.0", v2)
	d := &Differ{}
	sc, err := d.Source(context.Background(), dir, "v1.0.0", "v1.1.0", 50)
	if err != nil {
		t.Fatal(err)
	}
	if sc.FromRef != "v1.0.0" || sc.ToRef != "v1.1.0" {
		t.Fatalf("resolved refs = %q..%q", sc.FromRef, sc.ToRef)
	}
	if len(sc.Commits) != 1 || sc.Commits[0].Subject != "v2" {
		t.Fatalf("commits = %+v, want the single v2 commit", sc.Commits)
	}
	if len(sc.Diff.Files) == 0 {
		t.Fatal("diff is empty")
	}
	// Unscoped: the diff must include files outside any one app dir.
	if got := strings.Join(paths(sc.Diff.Files), ","); !strings.Contains(got, "other/app.yaml") {
		t.Fatalf("diff paths = %s, want other/app.yaml included (scope must be unset)", got)
	}
}

// The v-prefix fallback: image tags are usually bare ("1.1.0") while git tags
// carry a v ("v1.1.0"). The resolved spelling is reported back.
func TestSourceVPrefixFallback(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	tagRepo(t, dir, "v1.0.0", v1)
	tagRepo(t, dir, "v1.1.0", v2)
	d := &Differ{}
	sc, err := d.Source(context.Background(), dir, "1.0.0", "1.1.0", 50)
	if err != nil {
		t.Fatal(err)
	}
	if sc.FromRef != "v1.0.0" || sc.ToRef != "v1.1.0" {
		t.Fatalf("resolved refs = %q..%q, want v-prefixed", sc.FromRef, sc.ToRef)
	}
}

func TestSourceRefNotFoundListsNearbyTags(t *testing.T) {
	dir, v1, v2 := buildRepo(t)
	tagRepo(t, dir, "v1.0.0", v1)
	tagRepo(t, dir, "v1.1.0", v2)
	d := &Differ{}
	_, err := d.Source(context.Background(), dir, "v9.9.9", "v1.1.0", 50)
	var nf *RefNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want *RefNotFoundError", err)
	}
	if len(nf.Tags) == 0 || !strings.Contains(err.Error(), "v1.0.0") {
		t.Fatalf("error must list nearby tags for self-correction, got %v", err)
	}
}

func TestSourceCommitCap(t *testing.T) {
	dir, v1, _ := buildRepo(t)
	h := addUnrelatedCommit(t, dir) // third commit, helper from differ_test.go
	d := &Differ{}
	sc, err := d.Source(context.Background(), dir, v1.String(), h.String(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Commits) != 1 || !sc.CommitsCapped {
		t.Fatalf("commits = %d capped = %v, want 1/true", len(sc.Commits), sc.CommitsCapped)
	}
}
