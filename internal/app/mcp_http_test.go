// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbmcp"
)

func kbTestCatalog(t *testing.T, files map[string]string) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := catalog.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func rpc(t *testing.T, h http.Handler, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func listedTools(t *testing.T, h http.Handler) []string {
	t.Helper()
	w := rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var out struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	var names []string
	for _, tl := range out.Result.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func callText(t *testing.T, h http.Handler, tool, args string) string {
	t.Helper()
	w := rpc(t, h, "", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+args+`}}`)
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Result.Content) == 0 {
		t.Fatalf("%v: %s", err, w.Body)
	}
	return out.Result.Content[0].Text
}

func status(t *testing.T, h http.Handler, auth string) int {
	t.Helper()
	return rpc(t, h, auth, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`).Code
}

func TestKBMCPHandler(t *testing.T) {
	cat := kbTestCatalog(t, map[string]string{"incidents/harbor-down.md": "---\ntype: Incident\ntitle: Harbor registry down\n---\n\n## Symptom\n\nharbor 503\n"})
	cfg := &config.Config{Server: config.ServerConfig{MCP: config.ServerMCP{Enabled: true}}, GitOps: config.GitOps{Engine: "flux"}}
	var buf bytes.Buffer
	h := BuildKBMCPHandler(cfg, cat, "test", captureLog(&buf))
	if !strings.Contains(buf.String(), "serve port") || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("one warning naming the serve port: %s", buf.String())
	}
	want, _ := kbmcp.Tools(cat)[0].Handler(context.Background(), json.RawMessage(`{"query":"harbor"}`))
	if names := listedTools(t, h); !slices.Equal(names, []string{"kb_search", "kb_get"}) || callText(t, h, "kb_search", `{"query":"harbor"}`) != want {
		t.Fatalf("exactly the KB tools, answered as lore mcp does: %v", names)
	}
	if BuildKBMCPHandler(cfg, nil, "test", slog.Default()) != nil {
		t.Fatal("no catalog (no model) builds nil, with a warning, instead of a panic on every call")
	}
	cfg.Server.MCP = config.ServerMCP{Enabled: true, TokenEnv: "KB_TOKEN"}
	t.Setenv("KB_TOKEN", "s3cret")                                                              // pragma: allowlist secret
	for auth, code := range map[string]int{"": 401, "Bearer nope": 401, "Bearer s3cret": 200} { // pragma: allowlist secret
		if got := status(t, BuildKBMCPHandler(cfg, cat, "test", slog.Default()), auth); got != code {
			t.Errorf("%q: %d", auth, got)
		}
	}
	buf.Reset()
	t.Setenv("KB_TOKEN", "")
	h = BuildKBMCPHandler(cfg, cat, "test", captureLog(&buf))
	if !strings.Contains(buf.String(), "KB_TOKEN") {
		t.Errorf("an empty token variable is warned about by name: %s", buf.String())
	}
	for _, auth := range []string{"", "Bearer ", "Bearer x"} {
		if got := status(t, h, auth); got != 401 {
			t.Errorf("empty token_env variable, %q: %d", auth, got)
		}
	}
	cfg.Server.MCP.Enabled = false
	if BuildKBMCPHandler(cfg, cat, "test", slog.Default()) != nil {
		t.Fatal("disabled builds nil")
	}
}
