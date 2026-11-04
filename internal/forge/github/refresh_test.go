// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The commit's identity is read from where GitHub puts it. The JSON is a real commit's, from
// GET /repos/Smana/runlore-kb/pulls/163/commits, cut to the fields that are read.
func TestEntryPRStateCommitIdentity(t *testing.T) {
	const realCommit = `{"author":{"login":"runlore[bot]","type":"Bot"},"committer":{"login":"web-flow","type":"User"},
		"commit":{"committer":{"email":"noreply@github.com"},"verification":{"verified":true,"reason":"valid"}}}`
	cases := []struct {
		name, commit, want string
	}{
		{"a Contents API commit with the App's token", realCommit, "runlore[bot]"},
		{"a human committer", `{"author":{"login":"runlore[bot]"},"committer":{"login":"Smana"},"commit":{"verification":{"verified":true}}}`, ""},
		{"no signature", `{"author":{"login":"runlore[bot]"},"committer":{"login":"web-flow"},"commit":{"verification":{"verified":false}}}`, ""},
		{"no verification block", `{"author":{"login":"runlore[bot]"},"committer":{"login":"web-flow"},"commit":{}}`, ""},
		{"no linked committer", `{"author":{"login":"runlore[bot]"},"committer":null,"commit":{"verification":{"verified":true}}}`, ""},
		{"no linked author", `{"author":null,"committer":{"login":"web-flow"},"commit":{"verification":{"verified":true}}}`, ""},
		{"a human's web edit keeps their login", `{"author":{"login":"Smana"},"committer":{"login":"web-flow"},"commit":{"verification":{"verified":true}}}`, "Smana"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/5"):
					_, _ = fmt.Fprint(w, `{"state":"open","mergeable":false,"head":{"ref":"runlore/kb-a","sha":"abc","repo":{"full_name":"o/r"}}}`)
				case strings.HasSuffix(r.URL.Path, "/commits"):
					_, _ = fmt.Fprint(w, "["+tc.commit+"]")
				default:
					_, _ = fmt.Fprint(w, `[]`)
				}
			}))
			defer srv.Close()
			c := New(srv.URL, "o", "r", "main", func(context.Context) (string, error) { return "t", nil })
			st, err := c.EntryPRState(context.Background(), 5)
			if err != nil || len(st.CommitAuthors) != 1 || st.CommitAuthors[0] != tc.want {
				t.Fatalf("%+v %v", st, err)
			}
		})
	}
}

// Real head refs of Smana/runlore-kb #157 and #163 fit, and so does what OpenPR makes of any title.
func TestKBBranchPattern(t *testing.T) {
	for _, ok := range []string{
		"runlore/kb-appwizardunavailable-1793255309",
		"runlore/kb-agentfleettokenspendhigh-xplane-run-nnhub3cz-is-the-sole-active-run-2-3-over-budget-and-still-spending-no-1793182052",
		kbBranchPrefix + slugify("Côté — 2.3× over budget; \"no…\" #1 a/b?c") + "-1793255309",
		kbBranchPrefix + slugify("…") + "-1793255309",
	} {
		if !kbBranchPattern.MatchString(ok) {
			t.Errorf("%q must fit", ok)
		}
	}
	for _, no := range []string{"runlore/kb-", "runlore/kb-a#b", "runlore/kb-a?b", "runlore/kb-a%23b", "runlore/kb-a b", "runlore/kb-a/b", "runlore/kb-a\n", "main", "x/runlore/kb-a", "runlore/revalidate-a"} {
		if kbBranchPattern.MatchString(no) {
			t.Errorf("%q must not fit", no)
		}
	}
}

// The rest of the refresh is proved against the fake repository in internal/curate; mergeableRetry
// is this package's own, so what null does is proved here.
func TestEntryPRStateMergeableRetry(t *testing.T) {
	oldRetry := mergeableRetry
	t.Cleanup(func() { mergeableRetry = oldRetry })
	mergeableRetry = 0

	cases := []struct {
		name    string
		answers []string // what each read of the PR says about mergeable
		want    string   // "", "true" or "false": nil is ""
		reads   int32
	}{
		{"known at once", []string{"false"}, "false", 1},
		{"null, then known", []string{"null", "false"}, "false", 2},
		{"null twice stays nil", []string{"null", "null", "false"}, "", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/5"):
					i := int(reads.Add(1)) - 1
					_, _ = fmt.Fprintf(w, `{"state":"open","mergeable":%s,"head":{"ref":"runlore/kb-a","sha":"abc","repo":{"full_name":"o/r"}}}`, tc.answers[i])
				default:
					_, _ = fmt.Fprint(w, `[]`)
				}
			}))
			defer srv.Close()
			c := New(srv.URL, "o", "r", "main", func(context.Context) (string, error) { return "t", nil })
			st, err := c.EntryPRState(context.Background(), 5)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if st.Mergeable != nil {
				got = fmt.Sprint(*st.Mergeable)
			}
			if got != tc.want || reads.Load() != tc.reads || st.HeadSHA != "abc" {
				t.Fatalf("mergeable=%q reads=%d state=%+v", got, reads.Load(), st)
			}
		})
	}

	t.Run("a cancelled wait is an error, not a state", func(t *testing.T) {
		mergeableRetry = time.Hour
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"state":"open","mergeable":null,"head":{"ref":"runlore/kb-a","sha":"abc","repo":{"full_name":"o/r"}}}`)
		}))
		defer srv.Close()
		c := New(srv.URL, "o", "r", "main", func(context.Context) (string, error) { return "t", nil })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if _, err := c.EntryPRState(ctx, 5); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
}
