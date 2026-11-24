// SPDX-License-Identifier: Apache-2.0

package kbui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
)

func setupTestCatalogAndLedger(t *testing.T) (*catalog.Catalog, *outcome.Ledger, time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	localDir := t.TempDir()
	commonsDir := t.TempDir()

	writeFile := func(root, rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(localDir, "incidents/oom-worker.md", `---
type: Incident
title: OOM worker limits too low
description: Worker pods OOMKilled under batch load
resource: apps/worker
tags: [oom, memory]
status: active
last_validated: "2026-11-20"
---
## Cause

Memory limit was 256MiB while peak RSS reached 512MiB.

## Resolution

Bump limit to 1GiB. See [PR #42](https://github.com/org/kb/pull/42).
<script>alert("xss")</script>
`)

	writeFile(localDir, "playbooks/stale-cert.md", `---
type: Playbook
title: Renew expired ingress TLS cert
description: Cert-manager issuer stuck
resource: ingress/api
status: draft
last_validated: "2025-01-01"
---
## Resolution

Trigger cert-manager challenge retry.
`)

	writeFile(commonsDir, "incidents/dns-timeout.md", `---
type: Incident
title: CoreDNS upstream timeout
description: DNS lookup failures across cluster
resource: kube-system/coredns
status: active
last_validated: "2026-11-01"
---
## Resolution

Check upstream resolvers.
`)

	cat, err := catalog.NewWithCommons(t.Context(), localDir, commonsDir)
	if err != nil {
		t.Fatalf("NewWithCommons: %v", err)
	}

	ledgerPath := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(ledgerPath)
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	// Record 2 recalls for oom-worker: 1 resolved, 1 unresolved, plus 1 👍 and 1 👎.
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fp-1",
		Kind:        "recall",
		Entry:       "incidents/oom-worker.md",
		TriggerKey:  "tk-1",
		At:          now.Add(-48 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Resolve("fp-1", now.Add(-47*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fp-2",
		Kind:        "recall",
		Entry:       "incidents/oom-worker.md",
		TriggerKey:  "tk-2",
		At:          now.Add(-24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Feedback("tk-1", "up", "U1", now.Add(-20*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Feedback("tk-2", "down", "U2", now.Add(-10*time.Hour)); err != nil {
		t.Fatal(err)
	}

	return cat, ledger, now
}

func assertSecurityHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	wantCSP := "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("CSP = %q, want %q", got, wantCSP)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if strings.Contains(strings.ToLower(rec.Body.String()), "<script") {
		t.Errorf("response body contains <script tag:\n%s", rec.Body.String())
	}
}

func TestHandlerIndexAndFilters(t *testing.T) {
	cat, ledger, now := setupTestCatalogAndLedger(t)
	h := New(Options{
		Catalog: cat,
		Ledger:  ledger,
		Now:     func() time.Time { return now },
	})

	t.Run("index lists all entries with outcome stats and staleness", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kb", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		assertSecurityHeaders(t, rec)
		body := rec.Body.String()
		for _, want := range []string{
			"OOM worker limits too low",
			"Renew expired ingress TLS cert",
			"CoreDNS upstream timeout",
			"1/2",   // resolve rate
			"0.50",  // Beta factor: (1+1+1)/(2+1+1+2) = 3/6 = 0.50
			"stale", // stale-cert is from 2025-01-01
			"commons",
			"local",
		} {
			if !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
				t.Errorf("index missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("search and filters narrow results", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kb?q=oom&type=Incident&origin=local", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		assertSecurityHeaders(t, rec)
		body := rec.Body.String()
		if !strings.Contains(body, "OOM worker limits too low") {
			t.Errorf("expected OOM entry in filtered results:\n%s", body)
		}
		if strings.Contains(body, "CoreDNS upstream timeout") || strings.Contains(body, "Renew expired ingress TLS cert") {
			t.Errorf("unexpected entry in filtered results:\n%s", body)
		}
	})

	t.Run("status filter matches draft", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kb?status=draft", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "Renew expired ingress TLS cert") {
			t.Errorf("expected draft entry:\n%s", body)
		}
		if strings.Contains(body, "OOM worker limits too low") {
			t.Errorf("active entry should be filtered out:\n%s", body)
		}
	})
}

func TestHandlerEntryDetailAndSecurity(t *testing.T) {
	cat, ledger, now := setupTestCatalogAndLedger(t)
	h := New(Options{
		Catalog: cat,
		Ledger:  ledger,
		Now:     func() time.Time { return now },
	})

	t.Run("entry detail renders frontmatter, outcome stats, and safe markdown", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kb/entry/incidents/oom-worker.md", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		assertSecurityHeaders(t, rec)
		body := rec.Body.String()
		for _, want := range []string{
			"OOM worker limits too low",
			"apps/worker",
			`<h2 id="resolution">Resolution</h2>`,
			`<a href="https://github.com/org/kb/pull/42" rel="noopener noreferrer">PR #42</a>`,
			"1/2",
			"0.50",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("entry page missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("path traversal returns 404", func(t *testing.T) {
		for _, p := range []string{
			"/kb/entry/../../etc/passwd",
			"/kb/entry/incidents/../../etc/passwd",
			"/kb/entry/does-not-exist.md",
		} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s: status = %d, want 404", p, rec.Code)
			}
			assertSecurityHeaders(t, rec)
		}
	})

	t.Run("non-GET/HEAD returns 405", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/kb", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /kb: status = %d, want 405", method, rec.Code)
			}
			assertSecurityHeaders(t, rec)
		}
	})

	t.Run("HEAD /kb succeeds", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/kb", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("HEAD /kb: status = %d, want 200", rec.Code)
		}
		assertSecurityHeaders(t, rec)
	})
}

func TestHandlerBearerAuth(t *testing.T) {
	cat, _, _ := setupTestCatalogAndLedger(t)
	t.Setenv("TEST_KBUI_TOKEN", "top-secret-token") // pragma: allowlist secret
	h := New(Options{
		Catalog:  cat,
		TokenEnv: "TEST_KBUI_TOKEN",
	})

	cases := []struct {
		name string
		auth string
		want int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "Bearer wrong", http.StatusUnauthorized},
		{"wrong scheme", "Basic dXNlcjpwYXNz", http.StatusUnauthorized},
		{"valid token", "Bearer top-secret-token", http.StatusOK}, // pragma: allowlist secret
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/kb", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			assertSecurityHeaders(t, rec)
		})
	}
}
