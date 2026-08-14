// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/thread"
)

// BuildNotifier assembles the configured chat notifiers (best-effort fan-out)
// via the notifier registry. Slack/Matrix (and any registered sink, e.g. the
// generic webhook) self-register; each Build reads its own config.
//
// threads (nil to disable) is handed to notifiers that can capture a thread
// root, so a later reply there can be attributed to this investigation.
func BuildNotifier(cfg *config.Config, threads notify.ThreadSink, log *slog.Logger) (*notify.Multi, error) {
	return notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log, Threads: threads})
}

// Thread-registry bounds. A thread stays answerable for a week — long enough for
// a Monday follow-up on a Friday incident, short enough that the live set stays
// small — and the size cap is the real backstop for a busy channel.
const (
	threadRegistryTTL = 7 * 24 * time.Hour
	threadRegistryMax = 2000
)

// BuildThreadRegistry assembles the thread-context registry backing
// notify.slack.thread_capture: nil-path (disabled) unless the option is on AND
// the outcome ledger has a path.
//
// The ledger path is the dependency because it names the durable state
// directory — the registry has to survive a restart or a leader failover for the
// same reason the ledger does, and inventing a second location for one file
// would be a second thing to mount. Without it, capture degrades to
// unavailable, which the human is told, rather than to silently forgetful.
func BuildThreadRegistry(cfg *config.Config) (*thread.Registry, error) {
	if !cfg.Notify.Slack.ThreadCapture || cfg.Outcome.LedgerPath == "" {
		return thread.NewRegistry("", threadRegistryTTL, threadRegistryMax)
	}
	path := filepath.Join(filepath.Dir(cfg.Outcome.LedgerPath), "threads.jsonl")
	return thread.NewRegistry(path, threadRegistryTTL, threadRegistryMax)
}

// ThreadCaptureDeliverable reports whether thread capture can actually work, and
// warns when it cannot. Call it only with notify.slack.thread_capture on.
//
// Validate already requires the bot-token fields alongside the option, but
// configuration is not delivery: the env var holding the token can be present
// and EMPTY at runtime (an unmounted secret, a blank Helm value), and the Slack
// builder then returns no notifier at all. No message is posted, so no thread
// exists to reply in — while startup announced the feature as enabled. Same
// guard, same reason, as SlackFeedbackDeliverable.
func ThreadCaptureDeliverable(cfg *config.Config, log *slog.Logger) bool {
	sl := cfg.Notify.Slack
	if notify.SlackBotDelivery(sl) {
		return true
	}
	log.Warn("slack thread_capture enabled but no bot-token delivery target resolved (credential env var empty); "+
		"no message is delivered, so no thread exists to capture knowledge in",
		"bot_token_env", sl.BotTokenEnv, "channel", sl.Channel)
	return false
}

// SlackFeedbackDeliverable reports whether the opt-in 👍/👎 buttons can actually
// reach Slack, and warns when they cannot. Call it only with
// notify.slack.feedback_buttons on.
//
// Validate already requires a delivery target to be CONFIGURED alongside the
// option, but configuration is not delivery: the env var holding the webhook URL
// or bot token can be present and EMPTY at runtime (an unmounted secret, a blank
// Helm value), and the Slack builder then returns no notifier at all. Nothing is
// delivered, so no buttons render and no rating can ever be recorded — while
// startup announced the feature as enabled. That is the same runtime gap
// BuildMatrixFeedback closes for an empty Matrix access token, checked the same
// way and in the same place.
func SlackFeedbackDeliverable(cfg *config.Config, log *slog.Logger) bool {
	sl := cfg.Notify.Slack
	if notify.SlackDeliveryTarget(sl) != "" {
		return true
	}
	log.Warn("slack feedback_buttons enabled but no slack delivery target resolved (credential env var empty); no message is delivered, so no buttons render and no feedback can be recorded",
		"webhook_url_env", sl.WebhookURLEnv, "bot_token_env", sl.BotTokenEnv, "channel", sl.Channel)
	return false
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
