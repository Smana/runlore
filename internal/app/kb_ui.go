// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"net/http"
	"os"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbui"
	"github.com/Smana/runlore/internal/outcome"
)

// BuildKBUIHandler builds the read-only /kb HTML explorer handler from cat and
// optional ledger; returns nil when server.kb_ui is off, or (with a warning)
// when there is no catalog. With token_env set the handler requires that bearer
// token (read from the environment on each request); set but empty, every
// request gets 401. Without token_env one warning states that any caller
// admitted on the serve port reaches /kb, read-only.
func BuildKBUIHandler(cfg *config.Config, cat *catalog.Catalog, ledger *outcome.Ledger, log *slog.Logger) http.Handler {
	u := cfg.Server.KBUI
	if !u.Enabled {
		return nil
	}
	if cat == nil {
		log.Warn("server.kb_ui.enabled is set but there is no catalog (no model configured): /kb is off")
		return nil
	}
	if u.TokenEnv == "" {
		log.Warn("server.kb_ui.token_env is not set: any caller allowed on the serve port reaches /kb, which is read-only and serves catalog entries")
	} else if os.Getenv(u.TokenEnv) == "" {
		log.Warn("server.kb_ui.token_env names an empty variable: every request to /kb gets 401", "env", u.TokenEnv)
	}
	if ledger != nil && !ledger.Enabled() {
		ledger = nil
	}
	return kbui.New(kbui.Options{
		Catalog:  cat,
		Ledger:   ledger,
		BasePath: "/kb",
		TokenEnv: u.TokenEnv,
	})
}
