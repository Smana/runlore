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
