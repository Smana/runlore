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

	"github.com/Smana/runlore/internal/providers"
)

func kbEntry() providers.KBEntry {
	return providers.KBEntry{
		Type: "Incident", Title: "Harbor down",
		Description: "valkey down", Fingerprint: "deadbeefcafebabe",
	}
}

// TestOpenPRMaintainsBundleFiles: the PR keeps the OKF bundle self-describing —
// index.md gains the new entry's link line (when an index exists) and log.md
// gains a chronological record, both committed on the PR branch.
func TestOpenPRMaintainsBundleFiles(t *testing.T) {
	puts := map[string]string{} // path -> decoded content
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
	})
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /repos/o/r/contents/index.md", func(w http.ResponseWriter, _ *http.Request) {
		content := base64.StdEncoding.EncodeToString([]byte("# Catalog\n\n## Incidents\n"))
		_, _ = w.Write([]byte(`{"sha":"idxsha","content":"` + content + `"}`))
	})
	mux.HandleFunc("GET /repos/o/r/contents/log.md", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content string `json:"content"`
			SHA     string `json:"sha"`
			Branch  string `json:"branch"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := base64.StdEncoding.DecodeString(body.Content)
		puts[r.PathValue("path")] = string(raw)
		if r.PathValue("path") == "index.md" && body.SHA != "idxsha" {
			t.Errorf("index.md update must carry the blob sha, got %q", body.SHA)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"html_url":"u"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if _, err := c.OpenPR(context.Background(), kbEntry()); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}

	idx, ok := puts["index.md"]
	if !ok || !strings.Contains(idx, "- [Harbor down](incidents/harbor-down-deadbeef.md) — valkey down") {
		t.Fatalf("index.md not maintained: %q", idx)
	}
	logMD, ok := puts["log.md"]
	if !ok || !strings.Contains(logMD, "* **Creation**: Added [Harbor down](incidents/harbor-down-deadbeef.md).") {
		t.Fatalf("log.md not maintained: %q", logMD)
	}
}

// TestOpenPRSkipsIndexWhenAbsent: no index.md in the bundle → RunLore does not
// impose one (its structure is the owner's choice); log.md is still created
// (its shape is fully specified by OKF §7).
func TestOpenPRSkipsIndexWhenAbsent(t *testing.T) {
	puts := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
	})
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		puts[r.PathValue("path")] = true
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"html_url":"u"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if _, err := c.OpenPR(context.Background(), kbEntry()); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if puts["index.md"] {
		t.Fatal("absent index.md must not be created")
	}
	if !puts["log.md"] {
		t.Fatal("log.md must be created even when absent")
	}
}
