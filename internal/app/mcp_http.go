// SPDX-License-Identifier: Apache-2.0

package app

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbmcp"
	"github.com/Smana/runlore/internal/mcp"
)

// BuildKBMCPHandler builds the POST /mcp handler serving exactly kb_search and kb_get from cat;
// nil when server.mcp is off, or (with a warning) when there is no catalog, which would panic in
// the search on every call. With token_env set the handler requires that bearer token, read from
// the environment at each request; set but empty, every request gets 401. Without token_env one
// warning says any caller the serve port admits reaches it, read-only.
func BuildKBMCPHandler(cfg *config.Config, cat *catalog.Catalog, version string, log *slog.Logger) http.Handler {
	m := cfg.Server.MCP
	if !m.Enabled {
		return nil
	}
	if cat == nil {
		log.Warn("server.mcp.enabled is set but there is no catalog (no model configured): /mcp is off")
		return nil
	}
	srv := mcp.NewServer("runlore", version, log)
	for _, t := range kbmcp.Tools(cat) {
		srv.AddTool(t)
	}
	if m.TokenEnv == "" {
		log.Warn("server.mcp.token_env is not set: any caller allowed on the serve port reaches /mcp, which is read-only and serves public text")
		return srv
	}
	if os.Getenv(m.TokenEnv) == "" {
		log.Warn("server.mcp.token_env names an empty variable: every request to /mcp gets 401", "env", m.TokenEnv)
	}
	return bearerGuard(m.TokenEnv, srv)
}

// bearerGuard answers 401 unless the request carries "Authorization: Bearer <value of env>".
// An empty variable never matches, so it denies everything.
func bearerGuard(env string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := os.Getenv(env)
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if want == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
