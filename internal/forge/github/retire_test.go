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
)

func TestSetStatusRetired(t *testing.T) {
	t.Run("inserts status after the opening fence", func(t *testing.T) {
		in := "---\ntype: Incident\ntitle: t\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		want := "---\nstatus: retired\ntype: Incident\ntitle: t\n---\nbody\n"
		if string(out) != want {
			t.Errorf("got:\n%s\nwant:\n%s", out, want)
		}
	})
	t.Run("replaces an existing status line in place", func(t *testing.T) {
		in := "---\ntype: Incident\nstatus: active\ntitle: t\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if !strings.Contains(string(out), "\nstatus: retired\n") || strings.Contains(string(out), "active") {
			t.Errorf("status not replaced in place:\n%s", out)
		}
	})
	t.Run("already retired reports already, content unchanged", func(t *testing.T) {
		in := "---\nstatus: retired\ntype: Incident\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || !already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if string(out) != in {
			t.Errorf("content changed on already-retired entry")
		}
	})
	t.Run("no frontmatter is an error, never a blind write", func(t *testing.T) {
		if _, _, err := setStatusRetired([]byte("just a body\n")); err == nil {
			t.Fatal("want error on missing frontmatter")
		}
	})
	t.Run("status in the BODY does not fool the fence scan", func(t *testing.T) {
		in := "---\ntype: Incident\n---\nstatus: retired appears in prose\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if !strings.HasPrefix(string(out), "---\nstatus: retired\n") {
			t.Errorf("status not inserted into frontmatter:\n%s", out)
		}
	})
}

// wrapBase64 mimics GitHub's contents API, which returns base64 wrapped at 60
// chars with newlines — the decode path must strip them before decoding.
func wrapBase64(b []byte) string {
	s := base64.StdEncoding.EncodeToString(b)
	var out strings.Builder
	for i := 0; i < len(s); i += 60 {
		end := min(i+60, len(s))
		out.WriteString(s[i:end])
		out.WriteByte('\n')
	}
	return out.String()
}

func TestOpenRetirePR(t *testing.T) {
	const entry = "---\ntype: Incident\ntitle: t\n---\nbody\n"

	t.Run("opens a status:retired PR carrying the file sha", func(t *testing.T) {
		var calls []string
		var putBody map[string]any
		var labelBody map[string]any
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
		mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "POST pulls")
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
		ref, err := c.OpenRetirePR(context.Background(), "incidents/t.md", "body with marker")
		if err != nil {
			t.Fatalf("OpenRetirePR: %v", err)
		}
		if ref.URL != "https://github.com/o/r/pull/42" {
			t.Fatalf("ref=%s", ref.URL)
		}
		// All six calls, PR opened before labels.
		if len(calls) != 6 || calls[0] != "GET contents incidents/t.md" || calls[len(calls)-1] != "POST labels" {
			t.Fatalf("unexpected call sequence: %v", calls)
		}
		// (a) PUT content is the stamped file.
		raw, _ := base64.StdEncoding.DecodeString(putBody["content"].(string))
		if !strings.HasPrefix(string(raw), "---\nstatus: retired\n") {
			t.Errorf("PUT content not stamped:\n%s", raw)
		}
		// (b) PUT carries the file sha from the first GET (makes it an update).
		if putBody["sha"] != "filesha123" {
			t.Errorf("PUT sha=%v, want filesha123", putBody["sha"])
		}
		// Labels applied.
		gotLabels, _ := labelBody["labels"].([]any)
		if len(gotLabels) != 2 || gotLabels[0] != "runlore" || gotLabels[1] != "runlore-retire" {
			t.Errorf("labels=%v, want [runlore runlore-retire]", labelBody["labels"])
		}
	})

	t.Run("already retired short-circuits after the first GET", func(t *testing.T) {
		var calls []string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, _ *http.Request) {
			calls = append(calls, "GET contents")
			retired := "---\nstatus: retired\ntype: Incident\n---\nbody\n"
			_ = json.NewEncoder(w).Encode(map[string]any{"content": wrapBase64([]byte(retired)), "sha": "s"})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		c := New(srv.URL, "o", "r", "main", staticToken("tok"))
		_, err := c.OpenRetirePR(context.Background(), "incidents/t.md", "body")
		if !errors.Is(err, ErrAlreadyRetired) {
			t.Fatalf("err=%v, want ErrAlreadyRetired", err)
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
		if _, err := c.OpenRetirePR(context.Background(), "incidents/gone.md", "body"); err == nil {
			t.Fatal("want error on 404 contents GET")
		}
		if len(calls) != 1 {
			t.Fatalf("expected only the contents GET, got %v", calls)
		}
	})
}
