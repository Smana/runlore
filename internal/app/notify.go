package app

import (
	"log/slog"
	"os"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

// BuildNotifier assembles the configured chat notifiers (best-effort fan-out).
func BuildNotifier(cfg *config.Config, log *slog.Logger) *notify.Multi {
	var ns []providers.Notifier
	// Bot token (chat.postMessage) takes precedence over an incoming webhook.
	if sl := cfg.Notify.Slack; sl.BotTokenEnv != "" && sl.Channel != "" {
		if tok := os.Getenv(sl.BotTokenEnv); tok != "" {
			ns = append(ns, notify.NewSlackBot(tok, sl.Channel))
		}
	} else if env := cfg.Notify.Slack.WebhookURLEnv; env != "" {
		if url := os.Getenv(env); url != "" {
			ns = append(ns, notify.NewSlack(url))
		}
	}
	if mc := cfg.Notify.Matrix; mc.Homeserver != "" && mc.RoomID != "" && mc.AccessTokenEnv != "" {
		if tok := os.Getenv(mc.AccessTokenEnv); tok != "" {
			ns = append(ns, notify.NewMatrix(mc.Homeserver, mc.RoomID, tok))
		}
	}
	return notify.NewMulti(log, ns...)
}
