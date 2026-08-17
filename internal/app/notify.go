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
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
	"github.com/Smana/runlore/internal/telemetry"
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
// notify.slack.thread_capture AND notify.matrix.thread_capture: nil-path
// (disabled) unless EITHER option is on AND the outcome ledger has a path.
//
// The ledger path is the dependency because it names the durable state
// directory — the registry has to survive a restart or a leader failover for the
// same reason the ledger does, and inventing a second location for one file
// would be a second thing to mount. Without it, capture degrades to
// unavailable, which the human is told, rather than to silently forgetful.
//
// One registry backs both transports: a thread opened over Slack is exactly
// as answerable as one opened over Matrix, so there is exactly one durable
// file and one in-memory set, never one per transport — see
// buildThreadResponder for the matching invariant on the write side.
func BuildThreadRegistry(cfg *config.Config) (*thread.Registry, error) {
	captureWanted := cfg.Notify.Slack.ThreadCapture || cfg.Notify.Matrix.ThreadCapture
	if !captureWanted || cfg.Outcome.LedgerPath == "" {
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

// buildThreadResponder assembles the knowledge-write responder shared by
// every transport's thread capture. It is built exactly once (see serve.go)
// regardless of how many transports end up using it, so OpenPRs — the global
// per-hour cap on standalone note PRs — bounds forge writes system-wide
// instead of once per transport: "one responder, two transports," per the
// design.
//
// forge may be nil (no forge.kb_repo configured, or no usable credential): the
// responder is still constructed, always the same instance, so callers check
// Forge == nil themselves and report their own transport-flavoured warning
// rather than this helper guessing which transport's operator to address.
//
// metrics is always non-nil in production (bound to the global no-op provider
// when telemetry is disabled — see serve.go) but the parameter itself stays
// nil-safe: thread.Responder guards it before every use, same as every other
// optional *telemetry.Metrics field in RunLore.
func buildThreadResponder(threadRegistry *thread.Registry, forge thread.Forge, metrics *telemetry.Metrics, log *slog.Logger) *thread.Responder {
	return &thread.Responder{
		Forge:             forge,
		Registry:          threadRegistry,
		MaxNotesPerThread: thread.DefaultMaxNotesPerThread,
		ForgeWrites:       ratelimit.New(20, time.Hour),
		Metrics:           metrics,
		Log:               log,
	}
}

// BuildThreadMention assembles the Slack thread-capture handler when
// notify.slack.thread_capture can actually work end to end, or returns nil
// (warning exactly once, naming the specific reason) when it cannot. Called
// only with the option on and threadRegistry persisted — see serve.go.
//
// responder is the shared knowledge-write responder built once by
// buildThreadResponder (see BuildMatrixFeedback for the Matrix side reusing
// the identical instance); a nil responder, or one with a nil Forge, means no
// forge is configured.
//
// ThreadCaptureDeliverable is checked BEFORE asking notifier for a replier —
// not after, as an earlier version of this wiring did. replier == nil is
// ambiguous on its own: it is nil both when the bot-token env resolves empty
// at runtime (the specific, actionable cause ThreadCaptureDeliverable
// diagnoses and names) AND when no notifier was built at all (the log-only,
// no-model path — see notifier's nil check below). Checking deliverability
// first means the specific cause is reported whenever it is the true one,
// instead of being masked every time by the generic "no thread-capable
// notifier resolved" message — which is what happened before: by the time the
// switch reached its replier check, replier != nil already implied a
// *notify.SlackBot existed in Multi, which is only ever built when
// SlackBotDelivery(sl) had already returned true, so ThreadCaptureDeliverable's
// own "no bot-token delivery target resolved" warning could never fire in
// production. This ordering is what makes it reachable.
func BuildThreadMention(cfg *config.Config, responder *thread.Responder, notifier *notify.Multi, log *slog.Logger) *thread.Mention {
	if responder == nil || responder.Forge == nil {
		log.Warn("slack thread_capture enabled but no forge is configured (forge.kb_repo / credentials); knowledge cannot be written")
		return nil
	}
	if !ThreadCaptureDeliverable(cfg, log) {
		return nil
	}
	// notifier is nil on the log-only path (no model configured); ThreadRepliers
	// is a pointer-receiver method that dereferences it, so a nil check here
	// avoids a startup panic on that otherwise-valid configuration. This wiring
	// is Slack-specific, so it always takes the "slack" entry from the
	// transport-keyed map — a deployment also running Matrix does not change
	// which replier answers a Slack thread.
	var replier providers.ThreadNotifier
	if notifier != nil {
		replier = notifier.ThreadRepliers()["slack"]
	}
	if replier == nil {
		log.Warn("slack thread_capture enabled but no thread-capable notifier resolved; replies cannot be posted")
		return nil
	}
	log.Info("slack thread capture enabled", "endpoint", "/slack/events")
	return &thread.Mention{
		Responder: responder,
		Registry:  responder.Registry,
		Replier:   replier,
		Log:       log,
	}
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

// Matrix thread-capture dispatcher bounds. matrixMentionConcurrency is sized
// the same as the Slack server's eventDispatcher (internal/server): each slot
// holds one forge round-trip, not a heavy computation, so a wide pool stays
// cheap, and the timeout guarantees the pool drains through a degraded forge
// in bounded time instead of being held hostage indefinitely. Matrix mirrors
// Slack's separate, smaller "busy notice" pool too (matrixBusyConcurrency
// below): even under a burst of addressed messages while the main pool is
// saturated, telling humans about it stays detached from the /sync goroutine
// — see MatrixFeedback.BusyDispatch.
const (
	matrixMentionConcurrency = 16
	matrixMentionTimeout     = 2 * time.Minute
	// matrixBusyConcurrency bounds detached "I'm too busy, try again" Matrix
	// replies (MatrixFeedback.BusyDispatch), sized the same as the Slack
	// server's busyDispatcher (internal/server.maxConcurrentBusyNotices):
	// deliberately separate and small from matrixMentionConcurrency, since
	// telling humans about an overload is itself a network round-trip and must
	// never become the next unbounded liability.
	matrixBusyConcurrency = 4
)

// buildMatrixThreadMention assembles the Matrix thread-capture handler when
// notify.matrix.thread_capture can actually work end to end, or returns nil
// (warning exactly once, naming the specific reason) when it cannot. Mirrors
// BuildThreadMention's guard order (forge, then registry, then replier).
// Matrix has no bot-token-style runtime deliverability gap to check
// separately here: by the time BuildMatrixFeedback calls this, the access
// token has already been confirmed present, which is everything Matrix
// delivery needs.
func buildMatrixThreadMention(cfg *config.Config, responder *thread.Responder, notifier *notify.Multi, log *slog.Logger) *thread.Mention {
	if responder == nil || responder.Forge == nil {
		log.Warn("matrix thread_capture enabled but no forge is configured (forge.kb_repo / credentials); knowledge cannot be written")
		return nil
	}
	if !responder.Registry.Enabled() {
		log.Warn("matrix thread_capture enabled but the thread registry is unavailable (outcome.ledger_path not set); knowledge cannot be attributed to a thread")
		return nil
	}
	// notifier is nil on the log-only path (no model configured); ThreadRepliers
	// is a pointer-receiver method that dereferences it, so a nil check here
	// avoids a startup panic on that otherwise-valid configuration.
	var replier providers.ThreadNotifier
	if notifier != nil {
		replier = notifier.ThreadRepliers()["matrix"]
	}
	if replier == nil {
		log.Warn("matrix thread_capture enabled but no Matrix thread-capable notifier resolved; replies cannot be posted")
		return nil
	}
	log.Info("matrix thread capture enabled", "room", cfg.Notify.Matrix.RoomID)
	return &thread.Mention{
		Responder: responder,
		Registry:  responder.Registry,
		Replier:   replier,
		Log:       log,
	}
}

// BuildMatrixFeedback assembles the opt-in Matrix listener backing BOTH
// notify.matrix.feedback_reactions and notify.matrix.thread_capture: one
// /sync long-poll loop serves either or both, so the listener is built
// whenever EITHER option is on. Returns nil when neither option is on, the
// outcome ledger cannot persist, or the access token is actually empty at
// runtime — a listener that could record nowhere or authenticate as no one
// must not start. Validate has already required the notifier fields and the
// ledger path with either option on; the token presence is an env-var runtime
// fact, checked here like the notifier's own builder does.
//
// Thread capture (the returned listener's Mentions/Dispatch) is wired on top
// ONLY when notify.matrix.thread_capture is on AND buildMatrixThreadMention
// can resolve a forge, an enabled registry and a Matrix replier — see that
// function for the guard-and-warn per condition. Missing any of the three
// degrades to a listener with thread capture off (feedback reactions still
// work if that option is on); it is never a reason to fail startup, and
// WithThreadCapture is simply not called — see its doc for why that leaves
// Mentions/Dispatch nil rather than partially set.
//
// responder, dispatch and busyDispatch are the SAME instances the Slack path
// builds and drains (see serve.go): exactly one *thread.Responder and two
// *thread.Dispatcher (mentions, busy notices) exist for the process
// regardless of how many transports use them — "one responder, two
// transports." metrics is the process's shared instrument set (nil-safe
// throughout notify.MatrixFeedback, same as internal/server.Server's own
// telemetryMetrics field) — wired unconditionally so a dropped mention is
// counted (runlore_mentions_dropped_on_saturation_total) exactly like a
// dropped Slack mention already is.
func BuildMatrixFeedback(cfg *config.Config, ledger *outcome.Ledger, responder *thread.Responder, dispatch, busyDispatch *thread.Dispatcher, notifier *notify.Multi, metrics *telemetry.Metrics, log *slog.Logger) *notify.MatrixFeedback {
	mc := cfg.Notify.Matrix
	if !mc.FeedbackReactions && !mc.ThreadCapture {
		return nil
	}
	if !ledger.Enabled() {
		return nil
	}
	tok := os.Getenv(mc.AccessTokenEnv)
	if tok == "" {
		log.Warn("matrix feedback_reactions/thread_capture enabled but the access token env is empty; listener disabled", "env", mc.AccessTokenEnv)
		return nil
	}
	opts := []notify.MatrixFeedbackOption{notify.WithMetrics(metrics)}
	if mc.FeedbackReactions {
		opts = append(opts, notify.WithFeedbackReactions())
	}
	if mc.ThreadCapture {
		if mention := buildMatrixThreadMention(cfg, responder, notifier, log); mention != nil {
			opts = append(opts, notify.WithThreadCapture(mention, dispatch, busyDispatch))
		}
	}
	return notify.NewMatrixFeedback(mc.Homeserver, mc.RoomID, tok, ledger, log, opts...)
}
