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

func TestUpdateIndexAppendsToTypeSection(t *testing.T) {
	existing := `---
okf_version: "0.1"
type: Index
---
# Catalog

## Playbooks

- [HelmRelease upgrade failure](playbooks/helmrelease.md)

## Incidents

- [Old incident](incidents/old.md)
`
	got := string(updateIndex([]byte(existing), kbEntry(), "incidents/harbor-down-deadbeef.md"))
	want := "- [Harbor down](incidents/harbor-down-deadbeef.md) — valkey down"
	if !strings.Contains(got, want) {
		t.Fatalf("index missing %q:\n%s", want, got)
	}
	// The new line must land inside the Incidents section, not after Playbooks.
	if strings.Index(got, want) < strings.Index(got, "## Incidents") {
		t.Fatalf("entry landed outside its type section:\n%s", got)
	}
	// The existing Playbooks section must be untouched.
	if !strings.Contains(got, "- [HelmRelease upgrade failure](playbooks/helmrelease.md)") {
		t.Fatalf("existing sections must be preserved:\n%s", got)
	}
}

func TestUpdateIndexCreatesMissingSection(t *testing.T) {
	existing := "# Catalog\n\n## Playbooks\n\n- [P](p.md)\n"
	got := string(updateIndex([]byte(existing), kbEntry(), "incidents/h.md"))
	if !strings.Contains(got, "## Incidents") {
		t.Fatalf("missing new ## Incidents section:\n%s", got)
	}
	if !strings.Contains(got, "- [Harbor down](incidents/h.md) — valkey down") {
		t.Fatalf("missing entry line:\n%s", got)
	}
}

func TestUpdateLogCreatesAndPrepends(t *testing.T) {
	// No log yet → a fresh OKF log: H1 title, newest-first date heading, bold
	// action word.
	got := string(updateLog(nil, kbEntry(), "incidents/h.md", "2026-07-03"))
	for _, want := range []string{"# ", "## 2026-07-03", "* **Creation**: Added [Harbor down](incidents/h.md)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("fresh log missing %q:\n%s", want, got)
		}
	}

	// Existing log with an older date → the new date heading goes FIRST (newest
	// first), older entries preserved below.
	existing := "# Catalog update log\n\n## 2026-06-20\n\n* **Creation**: Added [Old](o.md).\n"
	got = string(updateLog([]byte(existing), kbEntry(), "incidents/h.md", "2026-07-03"))
	i, j := strings.Index(got, "## 2026-07-03"), strings.Index(got, "## 2026-06-20")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("dates must be newest-first:\n%s", got)
	}

	// Same-day second entry → reuse the existing date heading, no duplicate.
	got = string(updateLog([]byte(got), kbEntry(), "incidents/h2.md", "2026-07-03"))
	if strings.Count(got, "## 2026-07-03") != 1 {
		t.Fatalf("same-day entries must share one date heading:\n%s", got)
	}
	if !strings.Contains(got, "(incidents/h2.md)") {
		t.Fatalf("second same-day entry missing:\n%s", got)
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
