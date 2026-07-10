// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"os"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
)

// BuildNotifier assembles the configured chat notifiers (best-effort fan-out)
// via the notifier registry. Slack/Matrix (and any registered sink, e.g. the
// generic webhook) self-register; each Build reads its own config.
func BuildNotifier(cfg *config.Config, log *slog.Logger) (*notify.Multi, error) {
	return notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log})
}

// BuildMatrixFeedback assembles the opt-in Matrix reaction listener
// (notify.matrix.feedback_reactions): nil unless the option is on, the outcome
// ledger persists, and the access token is actually present — a listener that
// could record nowhere or authenticate as no one must not start. Validate has
// already required the notifier fields and the ledger path with the option on;
// the token presence is an env-var runtime fact, checked here like the
// notifier's own builder does.
func BuildMatrixFeedback(cfg *config.Config, ledger *outcome.Ledger, log *slog.Logger) *notify.MatrixFeedback {
	mc := cfg.Notify.Matrix
	if !mc.FeedbackReactions || !ledger.Enabled() {
		return nil
	}
	tok := os.Getenv(mc.AccessTokenEnv)
	if tok == "" {
		log.Warn("matrix feedback_reactions enabled but the access token env is empty; listener disabled", "env", mc.AccessTokenEnv)
		return nil
	}
	return notify.NewMatrixFeedback(mc.Homeserver, mc.RoomID, tok, ledger, log)
}
