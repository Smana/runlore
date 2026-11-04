// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func addLine(line string) func([]byte) ([]byte, bool, error) {
	return func(raw []byte) ([]byte, bool, error) {
		if strings.Contains(string(raw), line) {
			return raw, false, nil
		}
		return append(append([]byte(nil), raw...), []byte(line+"\n")...), true, nil
	}
}

func TestEditEntryOnPR(t *testing.T) {
	ctx := context.Background()

	t.Run("an edit that changes the entry is one PUT, a repeat makes none", func(t *testing.T) {
		s := &stubPR{}
		c := s.start(t)
		changed, err := c.EditEntryOnPR(ctx, 84, "runlore: resolve", addLine("fixed"))
		if err != nil || !changed || len(s.writes()) != 1 || !strings.HasSuffix(s.entryContent(), "fixed\n") {
			t.Fatal(changed, err, s.writes())
		}
		if got := s.writes()[0]; got.message != "runlore: resolve" || got.path != "concepts/oom-1.md" {
			t.Fatalf("commit message and path: %+v", got)
		}
		changed, err = c.EditEntryOnPR(ctx, 84, "runlore: resolve", addLine("fixed"))
		if err != nil || changed || len(s.writes()) != 1 {
			t.Fatal("no change, no PUT: ", changed, err, s.writes())
		}
	})

	t.Run("ready-to-merge comes off before the push (KP20)", func(t *testing.T) {
		s := &stubPR{labels: []string{"runlore", "ready-to-merge"}}
		c := s.start(t)
		if _, err := c.EditEntryOnPR(ctx, 84, "m", addLine("x")); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(s.calls, ","); got != "DELETE ready-to-merge,PUT" {
			t.Fatal(got)
		}
	})

	t.Run("an edit that changes nothing leaves the label alone", func(t *testing.T) {
		s := &stubPR{labels: []string{"runlore", "ready-to-merge"}, content: noteEntry + "x\n"}
		c := s.start(t)
		if changed, err := c.EditEntryOnPR(ctx, 84, "m", addLine("x")); err != nil || changed {
			t.Fatal(changed, err)
		}
		if len(s.calls) != 0 {
			t.Fatal(s.calls)
		}
	})

	t.Run("a failed DELETE stops the push, a 404 does not", func(t *testing.T) {
		s := &stubPR{labels: []string{"runlore", "ready-to-merge"}, deleteStatus: http.StatusInternalServerError}
		c := s.start(t)
		if _, err := c.EditEntryOnPR(ctx, 84, "m", addLine("x")); err == nil || len(s.writes()) != 0 {
			t.Fatal("a 500 on the DELETE is an error and no PUT: ", err, s.writes())
		}
		s = &stubPR{labels: []string{"runlore", "ready-to-merge"}, deleteStatus: http.StatusNotFound}
		c = s.start(t)
		if changed, err := c.EditEntryOnPR(ctx, 84, "m", addLine("x")); err != nil || !changed || len(s.writes()) != 1 {
			t.Fatal("the label is already gone: still pushes: ", changed, err)
		}
	})

	t.Run("a PUT that landed but whose response did not is success", func(t *testing.T) {
		s := &stubPR{putLands: true}
		c := s.start(t)
		if changed, err := c.EditEntryOnPR(ctx, 84, "m", addLine("x")); err != nil || !changed {
			t.Fatal(changed, err)
		}
	})

	t.Run("an edit error is returned and nothing is pushed", func(t *testing.T) {
		s := &stubPR{}
		c := s.start(t)
		boom := errors.New("boom")
		if _, err := c.EditEntryOnPR(ctx, 84, "m", func([]byte) ([]byte, bool, error) { return nil, false, boom }); !errors.Is(err, boom) || len(s.writes()) != 0 {
			t.Fatal(err)
		}
	})
}

func TestAppendToEntryOnPRRemovesReadyToMergeFirst(t *testing.T) {
	s := &stubPR{labels: []string{"runlore", "ready-to-merge"}}
	c := s.start(t)
	if err := c.AppendToEntryOnPR(context.Background(), 84, "### note\n\nhi", "k1"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.calls, ","); got != "DELETE ready-to-merge,PUT" {
		t.Fatal("a thread note is a push too: ", got)
	}
}

func TestReplaceLabelRemovalOnly(t *testing.T) {
	for status, wantErr := range map[int]bool{http.StatusInternalServerError: true, http.StatusNotFound: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		c := New(srv.URL, "o", "r", "main", staticToken("tok"))
		if err := c.ReplaceLabel(context.Background(), 1, "x", ""); (err != nil) != wantErr {
			t.Errorf("DELETE %d, no add: err=%v", status, err)
		}
		// A swap keeps its best-effort removal: the failed DELETE is ignored.
		swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}))
		if err := New(swap.URL, "o", "r", "main", staticToken("tok")).ReplaceLabel(context.Background(), 1, "x", "y"); err != nil {
			t.Errorf("a swap ignores a failed DELETE: %v", err)
		}
		srv.Close()
		swap.Close()
	}
}

func TestPREntry(t *testing.T) {
	for name, tc := range map[string]struct {
		state, mergedAt, want string
	}{
		"open":   {"open", "", "open"},
		"merged": {"closed", "2026-11-03T10:00:00Z", "merged"},
		"closed": {"closed", "", "closed"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &stubPR{state: tc.state, mergedAt: tc.mergedAt}
			c := s.start(t)
			got, err := c.PREntry(context.Background(), 84)
			if err != nil || got.State != tc.want || got.Path != "concepts/oom-1.md" {
				t.Fatal(got, err)
			}
		})
	}
	s := &stubPR{changed: []string{"a.md", "b.md"}}
	if _, err := s.start(t).PREntry(context.Background(), 84); err == nil {
		t.Fatal("two entries among the changed files: refuse to pick")
	}
}

func TestPullSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/Smana/cnref/pulls/2638":
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/Smana/cnref/pull/2638","merge_commit_sha":"3f5a133c9d","additions":2,"deletions":2}`))
		case "/repos/Smana/cnref/pulls/2638/files":
			if r.URL.Query().Get("per_page") != "5" {
				t.Errorf("first 5 files only: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`[{"filename":"a.yaml"},{"filename":"b.yaml"}]`))
		case "/repos/Smana/cnref/pulls/500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	got, err := c.PullSummary(context.Background(), "Smana/cnref", 2638)
	want := providers.PullSummary{URL: "https://github.com/Smana/cnref/pull/2638", MergeSHA: "3f5a133c9d", Files: []string{"a.yaml", "b.yaml"}, Additions: 2, Deletions: 2}
	if err != nil || got.URL != want.URL || got.MergeSHA != want.MergeSHA || got.Additions != 2 || got.Deletions != 2 || strings.Join(got.Files, ",") != "a.yaml,b.yaml" {
		t.Fatal(got, err)
	}
	if _, err := c.PullSummary(context.Background(), "Smana/cnref", 9); !errors.Is(err, providers.ErrPullNotFound) {
		t.Fatal("a 404 is ErrPullNotFound: ", err)
	}
	if _, err := c.PullSummary(context.Background(), "Smana/cnref", 500); err == nil || errors.Is(err, providers.ErrPullNotFound) {
		t.Fatal("a 500 is not: ", err)
	}
}

func TestOpenResolvePR(t *testing.T) {
	const entry = "---\ntype: Incident\ntitle: t\n---\nbody\n"
	var calls []string
	var putBody, prBody map[string]any
	var labels []any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "GET contents "+r.PathValue("path"))
		_ = json.NewEncoder(w).Encode(map[string]any{"content": wrapBase64([]byte(entry)), "sha": "filesha"})
	})
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"object":{"sha":"b"}}`)) })
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		calls = append(calls, "POST refs "+b["ref"].(string))
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&putBody)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&prBody)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/o/r/pull/9","number":9}`))
	})
	mux.HandleFunc("POST /repos/o/r/issues/9/labels", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		labels, _ = b["labels"].([]any)
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))

	ref, err := c.OpenResolvePR(context.Background(), "incidents/t.md", addLine("fixed"), "the body <!-- runlore:resolve:abcd1234 -->")
	if err != nil || ref.URL != "https://github.com/o/r/pull/9" {
		t.Fatal(ref, err)
	}
	if len(labels) != 2 || labels[0] != "runlore" || labels[1] != "runlore-resolve" {
		t.Fatal("labels: ", labels)
	}
	if !strings.HasPrefix(calls[1], "POST refs refs/heads/runlore/resolve-") || !strings.Contains(prBody["body"].(string), "runlore:resolve:abcd1234") {
		t.Fatal(calls, prBody)
	}
	if putBody["sha"] != "filesha" {
		t.Fatal(putBody)
	}

	before := len(calls)
	_, err = c.OpenResolvePR(context.Background(), "incidents/t.md", func(raw []byte) ([]byte, bool, error) { return raw, false, nil }, "b")
	if !errors.Is(err, providers.ErrAlreadyResolved) || len(calls) != before+1 {
		t.Fatal("an edit that changes nothing: ErrAlreadyResolved, no branch: ", err, calls)
	}
}

// TestRefusesAPRThatIsNotRunLores: the number comes from an operator, so the PR
// must itself carry the runlore label and a runlore/ head branch.
func TestRefusesAPRThatIsNotRunLores(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]*stubPR{
		"unlabelled open PR":        {labels: []string{"bug"}},
		"no labels at all":          {labels: []string{}},
		"labelled PR, human branch": {branch: "alice/fix-oom"},
		"unlabelled merged PR":      {state: "closed", mergedAt: "2026-11-03T10:00:00Z", labels: []string{}},
		"merged, human branch":      {state: "closed", mergedAt: "2026-11-03T10:00:00Z", branch: "alice/x"},
	} {
		t.Run(name, func(t *testing.T) {
			c := s.start(t)
			if _, err := c.PREntry(ctx, 84); !errors.Is(err, providers.ErrNotRunLorePR) {
				t.Fatal("PREntry: ", err)
			}
			if s.state != "closed" {
				if _, err := c.EditEntryOnPR(ctx, 84, "m", addLine("x")); !errors.Is(err, providers.ErrNotRunLorePR) || len(s.calls) != 0 {
					t.Fatal("EditEntryOnPR: ", err, s.calls)
				}
			}
		})
	}
	// A thread note keeps its old contract: an unlabelled PR on a RunLore branch
	// (the best-effort label failed) still takes the note.
	u := &stubPR{labels: []string{}}
	if err := u.start(t).AppendToEntryOnPR(ctx, 84, "### note\n\nhi", "k"); err != nil || len(u.writes()) != 1 {
		t.Fatal("a thread note on an unlabelled RunLore-branch PR appends: ", err)
	}
	// The showcase shape: label runlore, head runlore/kb-...
	s := &stubPR{labels: []string{"runlore", "solved"}, branch: "runlore/kb-oom-1"}
	if pe, err := s.start(t).PREntry(ctx, 84); err != nil || pe.State != "open" {
		t.Fatal(pe, err)
	}
}

func TestPREntryText(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("the entry at the head"))
	serve := func(t *testing.T, files, contents string) *Client {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/o/r/pulls/84":
				_, _ = w.Write([]byte(`{"state":"open","head":{"ref":"someone/branch","sha":"headsha"}}`))
			case "/repos/o/r/pulls/84/files":
				_, _ = w.Write([]byte(files))
			case "/repos/o/r/contents/incidents/a.md":
				if r.URL.Query().Get("ref") != "headsha" || contents == "" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte(contents))
			default:
				w.WriteHeader(http.StatusInternalServerError)
			}
		}))
		t.Cleanup(srv.Close)
		return New(srv.URL, "o", "r", "main", staticToken("tok"))
	}
	one := `[{"filename":"index.md"},{"filename":"log.md"},{"filename":"incidents/a.md"}]`
	file := `{"sha":"blob","encoding":"base64","content":"` + encoded + `"}`

	t.Run("the entry, at the PR's head, whoever opened it", func(t *testing.T) {
		got, err := serve(t, one, file).PREntryText(context.Background(), 84)
		if err != nil || string(got) != "the entry at the head" {
			t.Fatal(string(got), err)
		}
	})
	for name, tc := range map[string]struct{ files, contents string }{
		"two entries":        {`[{"filename":"incidents/a.md"},{"filename":"b.md"}]`, file},
		"no entry":           {`[{"filename":"index.md"}]`, file},
		"gone from the head": {one, ""},
		"too large to read":  {one, `{"sha":"blob","encoding":"none","content":""}`},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := serve(t, tc.files, tc.contents).PREntryText(context.Background(), 84); err == nil {
				t.Fatal("a read that says nothing about the entry must fail: ", string(got))
			}
		})
	}
}
