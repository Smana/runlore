// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func TestBuildKBUIHandler(t *testing.T) {
	cat := kbTestCatalog(t, map[string]string{
		"incidents/harbor-down.md": "---\ntype: Incident\ntitle: Harbor registry down\n---\n\n## Resolution\n\nRestart Harbor.\n",
	})
	cfg := &config.Config{
		Server: config.ServerConfig{
			KBUI: config.ServerKBUI{Enabled: false},
		},
	}

	if h := BuildKBUIHandler(cfg, cat, nil, slog.Default()); h != nil {
		t.Fatal("disabled server.kb_ui must return nil")
	}

	cfg.Server.KBUI.Enabled = true
	if h := BuildKBUIHandler(cfg, nil, nil, slog.Default()); h != nil {
		t.Fatal("nil catalog must return nil")
	}

	var buf bytes.Buffer
	h := BuildKBUIHandler(cfg, cat, nil, captureLog(&buf))
	if h == nil {
		t.Fatal("enabled server.kb_ui with catalog must return handler")
	}
	if !strings.Contains(buf.String(), "serve port") || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("expected one warning naming the serve port when token_env is unset, got: %s", buf.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/kb", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Harbor registry down") {
		t.Fatalf("GET /kb = %d: %s", rec.Code, rec.Body.String())
	}

	cfg.Server.KBUI.TokenEnv = "KBUI_TEST_TOKEN"
	t.Setenv("KBUI_TEST_TOKEN", "s3cret-ui") // pragma: allowlist secret
	h = BuildKBUIHandler(cfg, cat, nil, slog.Default())
	for auth, want := range map[string]int{
		"":                 http.StatusUnauthorized,
		"Bearer nope":      http.StatusUnauthorized,
		"Bearer s3cret-ui": http.StatusOK, // pragma: allowlist secret
	} {
		req := httptest.NewRequest(http.MethodGet, "/kb", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != want {
			t.Errorf("auth=%q: status = %d, want %d", auth, w.Code, want)
		}
	}

	buf.Reset()
	t.Setenv("KBUI_TEST_TOKEN", "")
	h = BuildKBUIHandler(cfg, cat, nil, captureLog(&buf))
	if !strings.Contains(buf.String(), "KBUI_TEST_TOKEN") {
		t.Errorf("expected warning naming empty token_env variable, got: %s", buf.String())
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/kb", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty token_env must fail closed with 401, got %d", w.Code)
	}
}
