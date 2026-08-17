// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const noteEntry = "---\ntype: Concept\ntitle: Operator note: OOM\n---\n\n### 📝 Operator note\n\nthe first note\n"

// appendServer stubs the three calls AppendToEntryOnPR makes: the MR (source
// branch + changed paths), the raw entry on that branch, and the commit that
// writes it back. The commit body is captured into commit.
func appendServer(t *testing.T, branch string, changed []string, entry, content string, commit *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		switch {
		case strings.HasSuffix(path, "/merge_requests/84/changes"):
			var b strings.Builder
			b.WriteString(`{"source_branch":"` + branch + `","changes":[`)
			for i, f := range changed {
				if i > 0 {
					b.WriteString(",")
				}
				b.WriteString(`{"new_path":"` + f + `"}`)
			}
			b.WriteString("]}")
			_, _ = w.Write([]byte(b.String()))
		case strings.Contains(path, "/repository/files/"):
			// The file path must arrive URL-encoded as ONE segment and be read at
			// the merge request's own ref — GitLab 404s an unencoded "/" in a way
			// that looks like a permissions problem, so this is asserted rather
			// than assumed.
			want := "/repository/files/" + url.PathEscape(entry) + "/raw"
			if !strings.HasSuffix(path, want) || r.URL.Query().Get("ref") != branch {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(content))
		case strings.HasSuffix(path, "/repository/commits"):
			_ = json.NewDecoder(r.Body).Decode(commit)
			_, _ = w.Write([]byte(`{"id":"abc123"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestAppendToEntryOnPRUpdatesTheEntryOnTheSourceBranch is the GitLab half of
// issue #493: the note reaches the ENTRY FILE — on the merge request's own
// source branch, keeping every note already there — rather than a discussion
// note the catalog never indexes.
func TestAppendToEntryOnPRUpdatesTheEntryOnTheSourceBranch(t *testing.T) {
	var commit map[string]any
	srv := appendServer(t, "runlore/kb-oom-1",
		[]string{"index.md", "log.md", "concepts/oom-1.md"}, "concepts/oom-1.md", noteEntry, &commit)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "### 📝 Operator note\n\nthe second note"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	if commit == nil {
		t.Fatal("nothing was committed")
	}
	if commit["branch"] != "runlore/kb-oom-1" {
		t.Errorf("branch = %v, want the merge request's own source branch", commit["branch"])
	}
	actions, _ := commit["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v, want exactly one (the entry update)", commit["actions"])
	}
	action, _ := actions[0].(map[string]any)
	if action["action"] != "update" {
		t.Errorf("action = %v, want update — a create would 400 on an existing file", action["action"])
	}
	if action["file_path"] != "concepts/oom-1.md" {
		t.Errorf("file_path = %v, want the entry — index.md and log.md are bundle upkeep", action["file_path"])
	}
	body, _ := action["content"].(string)
	if !strings.Contains(body, "the first note") || !strings.Contains(body, "the second note") {
		t.Errorf("the entry must keep every note, got:\n%s", body)
	}
	if strings.Index(body, "the first note") > strings.Index(body, "the second note") {
		t.Errorf("notes must accumulate in order, got:\n%s", body)
	}
}

// TestAppendToEntryOnPRNeutralizesImages: the appended block gets the same image
// defusal renderEntry gives a first draft, so a note written straight into an
// entry cannot carry what a drafted one could not.
func TestAppendToEntryOnPRNeutralizesImages(t *testing.T) {
	var commit map[string]any
	srv := appendServer(t, "b", []string{"concepts/oom-1.md"}, "concepts/oom-1.md", noteEntry, &commit)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "look ![px](https://evil.example/px.gif)"); err != nil {
		t.Fatalf("AppendToEntryOnPR: %v", err)
	}
	actions, _ := commit["actions"].([]any)
	action, _ := actions[0].(map[string]any)
	body, _ := action["content"].(string)
	if strings.Contains(body, "![px](") || strings.Contains(body, "evil.example") {
		t.Errorf("image markdown reached the entry file:\n%s", body)
	}
	if !strings.Contains(body, "`[image: px]`") {
		t.Errorf("want the defused label renderEntry would have produced:\n%s", body)
	}
}

// TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous: the next move is a commit
// into a merge request a human is reviewing, so an unclear target is an error,
// never a guess — the caller degrades to a comment on it.
func TestAppendToEntryOnPRRefusesWhenTheEntryIsAmbiguous(t *testing.T) {
	for name, changed := range map[string][]string{
		"two entries": {"concepts/a.md", "concepts/b.md"},
		"no entry":    {"index.md", "log.md"},
	} {
		t.Run(name, func(t *testing.T) {
			var commit map[string]any
			srv := appendServer(t, "b", changed, "concepts/a.md", noteEntry, &commit)
			defer srv.Close()

			c := New(srv.URL, "o/r", "main", staticToken("tok"))
			if err := c.AppendToEntryOnPR(context.Background(), 84, "note"); err == nil {
				t.Fatal("want an error rather than a guess at which file to rewrite")
			}
			if commit != nil {
				t.Errorf("committed %v despite an ambiguous entry", commit)
			}
		})
	}
}

// TestAppendToEntryOnPRRefusesWithoutASourceBranch: an MR payload with no
// source_branch must not become a commit onto the empty branch name, which
// GitLab would resolve to the project default.
func TestAppendToEntryOnPRRefusesWithoutASourceBranch(t *testing.T) {
	var commit map[string]any
	srv := appendServer(t, "", []string{"concepts/oom-1.md"}, "concepts/oom-1.md", noteEntry, &commit)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.AppendToEntryOnPR(context.Background(), 84, "note"); err == nil {
		t.Fatal("want an error when the merge request names no source branch")
	}
	if commit != nil {
		t.Errorf("committed %v with no source branch", commit)
	}
}
