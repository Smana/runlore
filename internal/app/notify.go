package app

import (
	"log/slog"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
)

// BuildNotifier assembles the configured chat notifiers (best-effort fan-out)
// via the notifier registry. Slack/Matrix (and any registered sink, e.g. the
// generic webhook) self-register; each Build reads its own config.
func BuildNotifier(cfg *config.Config, log *slog.Logger) (*notify.Multi, error) {
	return notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log})
}
