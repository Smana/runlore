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
	"time"
)

// at is the candidate validation date every case below stamps with.
var at = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

const minGap = 720 * time.Hour // 30d, the shipped default

func TestSetLastValidated(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // "" ⇒ expect skipErr instead
		skipErr error
	}{
		{
			name: "no last_validated and an old timestamp inserts the field",
			in:   "---\ntype: Incident\ntimestamp: 2025-01-02\n---\nbody\n",
			want: "---\nlast_validated: 2026-08-03\ntype: Incident\ntimestamp: 2025-01-02\n---\nbody\n",
		},
		{
			name: "an older last_validated is replaced in place, nothing else moves",
			in:   "---\ntype: Incident\nlast_validated: 2025-01-02\ntitle: t\n---\nbody\n",
			want: "---\ntype: Incident\nlast_validated: 2026-08-03\ntitle: t\n---\nbody\n",
		},
		{
			// The anti-spam rule: a stamp inside minGap is noise, not news.
			name:    "a last_validated inside minGap is a done-skip",
			in:      "---\ntype: Incident\nlast_validated: 2026-07-20\n---\nbody\n",
			skipErr: ErrRecentlyValidated,
		},
		{
			// okf.Render quotes an RFC3339 scalar; the reader must see through it.
			name:    "a quoted RFC3339 last_validated inside minGap is a done-skip",
			in:      "---\ntype: Incident\nlast_validated: \"2026-07-20T08:00:00Z\"\n---\nbody\n",
			skipErr: ErrRecentlyValidated,
		},
		{
			name:    "a future last_validated is never written backwards",
			in:      "---\ntype: Incident\nlast_validated: 2027-01-01\n---\nbody\n",
			skipErr: ErrRecentlyValidated,
		},
		{
			// A freshly drafted entry carries no last_validated but a recent
			// timestamp: recall's own freshness fallback, so no PR is warranted.
			name:    "a recent timestamp with no last_validated is a done-skip",
			in:      "---\ntype: Incident\ntimestamp: \"2026-07-25T08:00:00Z\"\n---\nbody\n",
			skipErr: ErrRecentlyValidated,
		},
		{
			name: "a dateless entry has nothing on record, so it is stamped",
			in:   "---\ntype: Incident\ntitle: t\n---\nbody\n",
			want: "---\nlast_validated: 2026-08-03\ntype: Incident\ntitle: t\n---\nbody\n",
		},
		{
			name: "an unparseable last_validated is repaired, not trusted",
			in:   "---\ntype: Incident\nlast_validated: someday\n---\nbody\n",
			want: "---\ntype: Incident\nlast_validated: 2026-08-03\n---\nbody\n",
		},
		{
			// Recall never fires a retired/draft entry, so it can never earn a
			// confirmation; proposing one would contradict the retirement pass.
			name:    "a retired entry is never revalidated",
			in:      "---\ntype: Incident\nstatus: retired\ntimestamp: 2020-01-02\n---\nbody\n",
			skipErr: ErrEntryInactive,
		},
		{
			name:    "a draft entry is never revalidated",
			in:      "---\ntype: Incident\nstatus: Draft\ntimestamp: 2020-01-02\n---\nbody\n",
			skipErr: ErrEntryInactive,
		},
		{
			name: "an explicitly active entry is stamped like any other",
			in:   "---\ntype: Incident\nstatus: active\ntimestamp: 2020-01-02\n---\nbody\n",
			want: "---\nlast_validated: 2026-08-03\ntype: Incident\nstatus: active\ntimestamp: 2020-01-02\n---\nbody\n",
		},
		{
			// Fence-bounded scanning: prose in the body must never be mistaken
			// for frontmatter (the setStatusRetired property, kept).
			name: "last_validated in the BODY does not fool the fence scan",
			in:   "---\ntype: Incident\n---\nlast_validated: 2026-08-01 appears in prose\n",
			want: "---\nlast_validated: 2026-08-03\ntype: Incident\n---\nlast_validated: 2026-08-01 appears in prose\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := setLastValidated([]byte(tc.in), at, minGap)
			if tc.skipErr != nil {
				if !errors.Is(err, tc.skipErr) {
					t.Fatalf("err=%v, want %v", err, tc.skipErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("setLastValidated: %v", err)
			}
			if string(out) != tc.want {
				t.Errorf("got:\n%q\nwant:\n%q", out, tc.want)
			}
		})
	}

	t.Run("no frontmatter is an error, never a blind write", func(t *testing.T) {
		if _, err := setLastValidated([]byte("just a body\n"), at, minGap); err == nil {
			t.Fatal("want an error on missing frontmatter")
		}
	})
	t.Run("unterminated frontmatter is an error", func(t *testing.T) {
		if _, err := setLastValidated([]byte("---\ntype: Incident\n"), at, minGap); err == nil {
			t.Fatal("want an error on unterminated frontmatter")
		}
	})
}

// TestSetLastValidatedRoundTrips pins the stamped value to the ONE date grammar
// recall's age gate reads: a stamp recall cannot parse would silently leave the
// entry exempt from staleness, which looks like success and is not.
func TestSetLastValidatedRoundTrips(t *testing.T) {
	out, err := setLastValidated([]byte("---\ntype: Incident\n---\nbody\n"), at, minGap)
	if err != nil {
		t.Fatalf("setLastValidated: %v", err)
	}
	// Feed the stamped file straight back in with a candidate one day later: the
	// value we just wrote must be readable, hence a done-skip.
	if _, err := setLastValidated(out, at.Add(24*time.Hour), minGap); !errors.Is(err, ErrRecentlyValidated) {
		t.Fatalf("the stamped date did not parse back: err=%v", err)
	}
}

func TestOpenRevalidatePR(t *testing.T) {
	const entry = "---\ntype: Incident\ntitle: t\ntimestamp: 2020-01-02\n---\nbody\n"

	t.Run("opens a last_validated PR carrying the file sha", func(t *testing.T) {
		var calls []string
		var putBody, prBody, labelBody map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "GET contents "+r.PathValue("path"))
			_ = json.NewEncoder(w).Encode(map[string]any{"content": wrapBase64([]byte(entry)), "sha": "filesha123"})
		})
		mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "GET baseref")
			_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
		})
		mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "POST refs")
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "PUT contents "+r.PathValue("path"))
			_ = json.NewDecoder(r.Body).Decode(&putBody)
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "POST pulls")
			_ = json.NewDecoder(r.Body).Decode(&prBody)
			_, _ = w.Write([]byte(`{"html_url":"https://github.com/o/r/pull/42","number":42}`))
		})
		mux.HandleFunc("POST /repos/o/r/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, "POST labels")
			_ = json.NewDecoder(r.Body).Decode(&labelBody)
			_, _ = w.Write([]byte(`[]`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := New(srv.URL, "o", "r", "main", staticToken("tok"))
		ref, err := c.OpenRevalidatePR(context.Background(), "incidents/t.md", at, minGap, "body with marker")
		if err != nil {
			t.Fatalf("OpenRevalidatePR: %v", err)
		}
		if ref.URL != "https://github.com/o/r/pull/42" {
			t.Fatalf("ref=%s", ref.URL)
		}
		if len(calls) != 6 || calls[0] != "GET contents incidents/t.md" || calls[len(calls)-1] != "POST labels" {
			t.Fatalf("unexpected call sequence: %v", calls)
		}
		raw, _ := base64.StdEncoding.DecodeString(putBody["content"].(string))
		if !strings.HasPrefix(string(raw), "---\nlast_validated: 2026-08-03\n") {
			t.Errorf("PUT content not stamped:\n%s", raw)
		}
		if putBody["sha"] != "filesha123" {
			t.Errorf("PUT sha=%v, want filesha123", putBody["sha"])
		}
		if title, _ := prBody["title"].(string); !strings.Contains(title, "incidents/t.md") {
			t.Errorf("PR title=%q, want it to name the entry", title)
		}
		gotLabels, _ := labelBody["labels"].([]any)
		if len(gotLabels) != 2 || gotLabels[0] != "runlore" || gotLabels[1] != "runlore-revalidate" {
			t.Errorf("labels=%v, want [runlore runlore-revalidate]", labelBody["labels"])
		}
	})

	t.Run("a recently validated entry short-circuits after the first GET", func(t *testing.T) {
		var calls []string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "GET contents")
			fresh := "---\ntype: Incident\nlast_validated: 2026-07-25\n---\nbody\n"
			_ = json.NewEncoder(w).Encode(map[string]any{"content": wrapBase64([]byte(fresh)), "sha": "s"})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := New(srv.URL, "o", "r", "main", staticToken("tok"))
		_, err := c.OpenRevalidatePR(context.Background(), "incidents/t.md", at, minGap, "body")
		if !errors.Is(err, ErrRecentlyValidated) {
			t.Fatalf("err=%v, want ErrRecentlyValidated", err)
		}
		if len(calls) != 1 {
			t.Fatalf("expected only the contents GET, got %v", calls)
		}
	})

	t.Run("404 on the entry file errors with no further calls", func(t *testing.T) {
		var calls []string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "GET contents")
			w.WriteHeader(http.StatusNotFound)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := New(srv.URL, "o", "r", "main", staticToken("tok"))
		if _, err := c.OpenRevalidatePR(context.Background(), "incidents/gone.md", at, minGap, "body"); err == nil {
			t.Fatal("want an error on a 404 contents GET")
		}
		if len(calls) != 1 {
			t.Fatalf("expected only the contents GET, got %v", calls)
		}
	})
}
