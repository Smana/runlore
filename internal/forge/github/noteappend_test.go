// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// appendMux builds a GitHub stub serving one note PR: head branch `branch`,
// changed files `changed`, and `entry` holding `content` on that branch. It
// returns the mux and a pointer to the PUT the client makes, so a test can
// assert what was committed and where.
type recordedPut struct {
	path, branch, sha, content string
	made                       bool
}

func appendMux(t *testing.T, branch string, changed []string, entry, content string, put *recordedPut) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls/84", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"head":{"ref":"` + branch + `"}}`))
	})
	mux.HandleFunc("GET /repos/o/r/pulls/84/files", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString("[")
		for i, f := range changed {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"filename":"` + f + `"}`)
		}
		b.WriteString("]")
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("path") != entry || r.URL.Query().Get("ref") != branch {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"sha":"entrysha","content":"` + base64.StdEncoding.EncodeToString([]byte(content)) + `"}`))
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
			SHA     string `json:"sha"`
			Branch  string `json:"branch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.StdEncoding.DecodeString(body.Content)
		*put = recordedPut{path: r.PathValue("path"), branch: body.Branch, sha: body.SHA, content: string(raw), made: true}
		_, _ = w.Write([]byte(`{}`))
	})
	return mux
}

const noteEntry = "---\ntype: Concept\ntitle: Operator note: OOM\n---\n\n### 📝 Operator note\n\nthe first note\n"

// TestAppendToEntryOnPRCommitsOntoThePRsOwnBranch is the forge half of issue
// #493: the note has to end up in the ENTRY FILE the pull request carries, on
// that pull request's OWN branch, keeping everything already there.
//
// Four separate facts, because getting any one of them wrong loses or corrupts
// a human's note: the right file (not index.md or log.md), the right branch
// (base has no such file until merge), the blob sha (so a racing writer loses
// the PUT instead of silently clobbering), and the original content preserved
// ahead of the new block.
func TestAppendToEntryOnPRCommitsOntoThePRsOwnBranch(t *testing.T) {
	var put recordedPut
	srv := httptest.NewServer(appendMux(t, "runlore/kb-oom-1",
		[]string{"index.md", "log.md", "concepts/oom-1.md"}, "concepts/oom-1.md", noteEntry, &put))
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "### 📝 Operator note\n\nthe second note"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	if !put.made {
		t.Fatal("nothing was committed")
	}
	if put.path != "concepts/oom-1.md" {
		t.Errorf("committed to %q, want the entry file — index.md and log.md are bundle upkeep, not the entry", put.path)
	}
	if put.branch != "runlore/kb-oom-1" {
		t.Errorf("committed onto %q, want the pull request's own head branch", put.branch)
	}
	if put.sha != "entrysha" {
		t.Errorf("sha = %q, want the blob sha just read — without it a racing writer is silently overwritten", put.sha)
	}
	if !strings.Contains(put.content, "the first note") || !strings.Contains(put.content, "the second note") {
		t.Errorf("the entry must keep every note, got:\n%s", put.content)
	}
	if strings.Index(put.content, "the first note") > strings.Index(put.content, "the second note") {
		t.Errorf("notes must accumulate in order, got:\n%s", put.content)
	}
}

// TestAppendToEntryOnPRNeutralizesImagesInTheBlock: an appended note lands in a
// file a reviewer opens on the forge, so it gets the SAME image defusal
// renderEntry applies to a first draft — this client's own neutralizeImages, the
// one that rewrites the whole `![alt](url)` to a labelled code span. Without it
// the second note into an entry could be a tracking pixel that the first note,
// which went through renderEntry, could not have been.
func TestAppendToEntryOnPRNeutralizesImagesInTheBlock(t *testing.T) {
	var put recordedPut
	srv := httptest.NewServer(appendMux(t, "b", []string{"concepts/oom-1.md"}, "concepts/oom-1.md", noteEntry, &put))
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "look ![px](https://evil.example/px.gif)"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	if strings.Contains(put.content, "![px](") || strings.Contains(put.content, "evil.example") {
		t.Errorf("image markdown reached the entry file:\n%s", put.content)
	}
	if !strings.Contains(put.content, "`[image: px]`") {
		t.Errorf("want the defused label renderEntry would have produced:\n%s", put.content)
	}
}

// TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous: the next move is a commit
// into a pull request a human is reviewing, so an unclear target is an error,
// never a guess. The caller degrades to a comment on this error (see
// thread.Responder.addToPR); a wrong write would have no such recovery.
func TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous(t *testing.T) {
	for name, changed := range map[string][]string{
		"two entries": {"concepts/a.md", "concepts/b.md"},
		"no entry":    {"index.md", "log.md"},
	} {
		t.Run(name, func(t *testing.T) {
			var put recordedPut
			srv := httptest.NewServer(appendMux(t, "b", changed, "concepts/a.md", noteEntry, &put))
			defer srv.Close()

			c := New(srv.URL, "o", "r", "main", staticToken("tok"))
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note"); err == nil {
				t.Fatal("want an error rather than a guess at which file to rewrite")
			}
			if put.made {
				t.Errorf("committed to %q despite an ambiguous entry", put.path)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesWithoutAHeadBranch: a PR payload with no head ref
// must not become a commit onto the empty branch name (or onto the base).
func TestAppendToEntryOnPRRefusesWithoutAHeadBranch(t *testing.T) {
	var put recordedPut
	mux := appendMux(t, "", []string{"concepts/oom-1.md"}, "concepts/oom-1.md", noteEntry, &put)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note"); err == nil {
		t.Fatal("want an error when the pull request names no head branch")
	}
	if put.made {
		t.Errorf("committed onto %q with no head branch", put.branch)
	}
}
