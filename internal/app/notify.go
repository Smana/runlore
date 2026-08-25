// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/catalog"
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
//
// TTL and max-live come from notify.thread.registry_ttl/registry_max
// (config.ThreadNotify), each already resolved to thread.DefaultRegistryTTL/
// DefaultRegistryMax when unset — an absent notify.thread block reproduces
// the bounds this used to hardcode.
func BuildThreadRegistry(cfg *config.Config) (*thread.Registry, error) {
	ttl := cfg.Notify.Thread.EffectiveRegistryTTL()
	maxLive := cfg.Notify.Thread.EffectiveRegistryMax()
	captureWanted := cfg.Notify.Slack.ThreadCapture || cfg.Notify.Matrix.ThreadCapture
	if !captureWanted || cfg.Outcome.LedgerPath == "" {
		return thread.NewRegistry("", ttl, maxLive)
	}
	path := filepath.Join(filepath.Dir(cfg.Outcome.LedgerPath), "threads.jsonl")
	return thread.NewRegistry(path, ttl, maxLive)
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
// regardless of how many transports end up using it, so ForgeWrites — the
// global per-hour cap on EVERY forge write this path makes, a CommentOnPR
// exactly like an OpenPR — bounds them system-wide instead of once per
// transport: "one responder, two transports," per the design.
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
//
// MaxNotesPerThread, ForgeWrites and MaxNoteBytes all come from
// notify.thread (config.ThreadNotify), each already resolved to its
// thread.Default* constant when unset — an absent notify.thread block
// reproduces exactly the bounds this used to hardcode.
//
// ForgeRepo comes from forge.* instead, because it must name the repository
// the FORGE CLIENT above writes to and nothing else: it is what stops a
// pull-request URL arriving from anywhere else from selecting which PR a note
// lands on (see thread.Responder.ForgeRepo).
//
// chat (nil when model.chat is not configured) is the conversational layer,
// built once by buildThreadChat and carried on the same single responder for
// the same reason ForgeWrites is: its per-hour Budget must bound token spend
// system-wide, not once per transport.
//
// notifier is the process's fan-out sink, and it is here for the announcement
// half: with notify.thread.announce_kb_updates on, a landed knowledge write is
// broadcast to every configured notifier — its own channel or room by default,
// or into the originating thread when the key selects that (see
// buildKBAnnouncer). It may be nil — the log-only path builds no notifier at all
// — which is one of the two ways announcements end up off.
func buildThreadResponder(cfg *config.Config, threadRegistry *thread.Registry, forge thread.Forge, chat *thread.Chat, notifier *notify.Multi, metrics *telemetry.Metrics, log *slog.Logger) *thread.Responder {
	return &thread.Responder{
		Forge:             forge,
		Registry:          threadRegistry,
		MaxNotesPerThread: cfg.Notify.Thread.EffectiveMaxNotesPerThread(),
		ForgeWrites:       ratelimit.New(cfg.Notify.Thread.EffectiveForgeWritesPerHour(), time.Hour),
		MaxNoteBytes:      cfg.Notify.Thread.EffectiveMaxNoteBytes(),
		ForgeRepo:         forgeRepoRef(cfg),
		Chat:              chat,
		Announcer:         buildKBAnnouncer(cfg, notifier, log),
		Metrics:           metrics,
		Log:               log,
	}
}

// buildKBAnnouncer assembles the knowledge-base announcer, or returns nil when
// announcements are off — either because notify.thread.announce_kb_updates was
// not turned on (the default) or because there is no sink to deliver to.
//
// nil is the feature's off switch (thread.Responder.Announcer is nil-safe), and
// the two conditions collapse into that ONE value deliberately: an announcer
// wired to a notifier holding no sinks would hold a dispatcher, its slots and
// its goroutines to deliver every announcement to nobody, which is a live
// feature by every observable signal and a no-op in fact.
//
// The nil check on notifier before handing it over is not incidental. It is a
// concrete *notify.Multi and thread.NewKBAnnouncer takes an interface: passing a
// nil pointer through would produce a NON-nil providers.KBUpdateNotifier holding
// a nil Multi, so "announcements are off" and "announcements are on, delivering
// nowhere" would become indistinguishable — the same typed-nil trap
// buildThreadChat documents for Chat.Catalog.
//
// The Info line states the capability check rather than claiming delivery:
// notify.Multi announces only to sinks implementing providers.KBUpdateNotifier
// and silently skips the rest, so the number of CONFIGURED notifiers is an upper
// bound on how many will actually render an announcement, not a promise.
func buildKBAnnouncer(cfg *config.Config, notifier *notify.Multi, log *slog.Logger) *thread.KBAnnouncer {
	sinks := 0
	if notifier != nil {
		sinks = notifier.Len()
	}
	if msg := KBAnnounceInertWarning(cfg, sinks); msg != "" {
		log.Warn(msg)
	}
	if !cfg.Notify.Thread.AnnounceKBUpdates.On() || sinks == 0 {
		return nil
	}
	log.Info("thread knowledge-base announcements enabled",
		"notifiers", sinks,
		"delivery", string(cfg.Notify.Thread.AnnounceKBUpdates),
		"note", "each configured notifier that implements the announcement capability receives them; a thread delivery reaches the originating thread on its own transport and the channel on every other sink; the thread reply is unchanged")
	return thread.NewKBAnnouncer(notifier, cfg.Notify.Thread.AnnounceKBUpdates.Delivery(), log)
}

// KBAnnounceInertWarning reports that notify.thread.announce_kb_updates is on
// while nothing can act on it, naming which end is missing; "" when the key is
// off or the feature can actually fire.
//
// Every dependency on this path is nil-safe by design — a nil sink means
// announcements are off, and nothing anywhere errors — so both failures are
// completely silent today. Startup must not stay silent about a feature that was
// explicitly asked for and cannot fire.
//
// The two ends the announcement needs are checked separately because the fix
// differs. Delivery: no notifier was built (none configured, or the only one's
// credential resolved empty at runtime and left Multi with nothing in it), so
// every announcement goes to nobody. Production: no transport captures knowledge
// from a thread, so no write ever happens to announce — the same reachability
// gap ChatWithoutCaptureWarning reports for the chat layer, on the feature that
// sits one hop further along the same path. One message at a time: they have
// different remedies, and a deployment with both is a deployment where the key
// was set before anything else was.
//
// A warning rather than a Validate error, for the same reason
// ChatWithoutCaptureWarning is one: the operator may be staging the rest of the
// configuration, and nothing else is degraded — with capture on, the note is
// still written and the human in the thread is still answered. Only the
// broadcast is missing.
func KBAnnounceInertWarning(cfg *config.Config, sinks int) string {
	if !cfg.Notify.Thread.AnnounceKBUpdates.On() {
		return ""
	}
	if sinks == 0 {
		return "notify.thread.announce_kb_updates is enabled but no notifier is configured, so a knowledge-base " +
			"write is announced to nobody: the note is still written and the thread still gets its reply, but " +
			"nothing is broadcast — configure notify.slack / notify.matrix (or another registered notifier), or " +
			"turn the key off"
	}
	if !cfg.Notify.Slack.ThreadCapture && !cfg.Notify.Matrix.ThreadCapture {
		return "notify.thread.announce_kb_updates is enabled but neither notify.slack.thread_capture nor " +
			"notify.matrix.thread_capture is: no thread can write to the knowledge base, so there is never " +
			"anything to announce — enable capture on a transport, or this is dead config"
	}
	return ""
}

// forgeRepoRef is the WEB identity of the repository RunLore curates into —
// "github.com/acme/kb", "gitlab.example.com/group/sub/proj" — the host and
// path a pull-request / merge-request URL from that repository carries.
//
// Both halves come from the same config the forge client is built from, so
// they cannot name a different repository than the one writes actually land
// in. Empty when forge.kb_repo is unset, which is also when buildForge returns
// no client at all: nothing is written, and thread.Responder.ForgeRepo stays
// unanchored rather than anchored to a half-formed reference.
func forgeRepoRef(cfg *config.Config) string {
	repo := strings.Trim(cfg.Forge.KBRepo, "/")
	if repo == "" {
		return ""
	}
	return forgeWebHost(cfg) + "/" + repo
}

// forgeWebHost is the WEB host of the forge RunLore curates into — the host a
// pull-request / merge-request URL from that forge carries. It follows the
// same provider switch and the same defaults the forge client itself is built
// from (see buildForge / config.Validate), so the two cannot name different
// hosts: GitHub derives it from the API base, exactly as githubGitHost already
// does for source-diff allowlisting; GitLab's base_url IS the instance root,
// which is the host its web_url uses.
//
// forgeGitHost derives the CREDENTIAL BOUNDARY from this function, so the fold
// here is a security decision and not presentation: it goes through
// asciiLowerHost, which refuses to fold a host that Go and IDNA would normalise
// to different names. See asciiLowerHost.
func forgeWebHost(cfg *config.Config) string {
	if cfg.Forge.Provider != "gitlab" {
		return githubGitHost(cfg.Forge.GitHubAPIURL)
	}
	if cfg.Forge.GitLab.BaseURL == "" {
		return "gitlab.com"
	}
	u, err := url.Parse(cfg.Forge.GitLab.BaseURL)
	if err != nil || u.Hostname() == "" {
		return "gitlab.com"
	}
	return asciiLowerHost(u.Hostname())
}

// buildThreadChat assembles the conversational reply layer, or returns nil when
// model.chat is absent — the presence of that block IS the feature switch (see
// BuildChatModel), and a nil *thread.Chat is what makes Responder's freeform
// route behave exactly as it did before this layer existed.
//
// Every field on thread.Chat is nil/zero-safe, which makes this function the
// only place an omission can be caught: an unwired Budget silently removes the
// spend ceiling entirely, an unwired Catalog silently degrades every reply, and
// an unwired MaxOutputTokens silently under-charges the budget against the cap
// the operator actually configured. TestBuildThreadChatWiresEveryField pins each
// one for that reason.
//
// cat is the SAME *catalog.Catalog the investigation path holds — one catalog
// per process, never a second index or a second git-sync goroutine. It is taken
// as a concrete pointer and converted here so a nil catalog reaches Chat.Catalog
// as a nil INTERFACE: a typed-nil would make Chat.Catalog != nil true and turn
// catalogHits' documented no-hits branch into a nil-receiver call. Same idiom,
// same reason, as BuildInvestigator's priorEntryFinder.
//
// The layer is built even when it is degraded — no catalog, an empty credential,
// no transport listening. Chat.Answer's contract is that any failure returns
// false and the deterministic capture path answers instead, so degrading loudly
// beats refusing to start; each condition below warns exactly once, naming what
// the operator has to fix.
func buildThreadChat(cfg *config.Config, cat *catalog.Catalog, metrics *telemetry.Metrics, log *slog.Logger) *thread.Chat {
	model := BuildChatModel(cfg)
	if model == nil {
		return nil
	}
	// model.chat configured with no transport listening for mentions is paid-for
	// dead config. A warning rather than a Validate error: the operator may be
	// staging thread_capture next — see config.ChatWithoutCaptureWarning.
	if msg := config.ChatWithoutCaptureWarning(cfg); msg != "" {
		log.Warn(msg)
	}
	// Configuration is not delivery: an api_key_env present but EMPTY at runtime
	// (an unmounted secret, a blank Helm value) passes Validate and then fails
	// every call. The same runtime gap ThreadCaptureDeliverable and
	// SlackFeedbackDeliverable close for their transports.
	if env := cmp.Or(cfg.Model.Chat.APIKeyEnv, cfg.Model.APIKeyEnv); env != "" && os.Getenv(env) == "" {
		log.Warn("model.chat is configured but its credential env var is empty; every conversational reply "+
			"will fail at call time and fall back to deterministic capture", "api_key_env", env)
	}
	var searcher catalog.Searcher
	if cat != nil {
		searcher = cat
	} else {
		log.Warn("model.chat is configured but no catalog is available; conversational replies are answered " +
			"with no knowledge-base context (configure catalog.dir / catalog.repo)")
	}
	callsPerHour := cfg.Notify.Thread.EffectiveChatCallsPerHour()
	tokensPerHour := cfg.Notify.Thread.EffectiveChatTokensPerHour()
	maxOutput := chatMaxTokens(cfg)
	log.Info("thread conversational replies enabled",
		"model", cfg.Model.Chat.Model, "max_output_tokens", maxOutput,
		"calls_per_hour", callsPerHour, "tokens_per_hour", tokensPerHour)
	return &thread.Chat{
		Model:  model,
		Budget: thread.NewBudget(callsPerHour, tokensPerHour, time.Hour, log),
		// MaxOutputTokens must be the value the provider client above actually
		// runs under, not thread's own default: it sizes the conservative charge
		// applied when a provider reports no usage, so an operator who raised
		// model.chat.max_tokens would otherwise have that spend under-billed
		// against their own ceiling.
		MaxOutputTokens: maxOutput,
		Catalog:         searcher,
		// The same bound the Responder writes under, so one configured value
		// covers the message on the way to the model and on the way to the KB.
		MaxNoteBytes: cfg.Notify.Thread.EffectiveMaxNoteBytes(),
		Metrics:      metrics,
		Log:          log,
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

// BuildMatrixFeedback assembles the opt-in Matrix listener backing THREE
// independent capabilities: notify.matrix.feedback_reactions,
// notify.matrix.silence_reactions and notify.matrix.thread_capture. One
// /sync long-poll loop serves any combination of them, so the listener is
// built whenever ANY option is on. Returns nil when none of the three is on,
// the outcome ledger cannot persist, or the access token is actually empty at
// runtime — a listener that could record nowhere or authenticate as no one
// must not start. Validate has already required the notifier fields and the
// ledger path with any option on; the token presence is an env-var runtime
// fact, checked here like the notifier's own builder does.
//
// notify.WithSilenceReactions type-asserts the ledger it is given against
// notify.SilenceSink itself, so passing ledger unconditionally here (rather
// than only when SilenceReactions is set) is safe either way.
//
// Thread capture (the returned listener's Mentions/Dispatch) is wired on top
// ONLY when notify.matrix.thread_capture is on AND buildMatrixThreadMention
// can resolve a forge, an enabled registry and a Matrix replier — see that
// function for the guard-and-warn per condition. Missing any of the three
// degrades to a listener with thread capture off (feedback and silence
// reactions still work if those options are on); it is never a reason to
// fail startup, and WithThreadCapture is simply not called — see its doc for
// why that leaves Mentions/Dispatch nil rather than partially set.
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
	if !mc.FeedbackReactions && !mc.ThreadCapture && !mc.SilenceReactions {
		return nil
	}
	if !ledger.Enabled() {
		return nil
	}
	tok := os.Getenv(mc.AccessTokenEnv)
	if tok == "" {
		log.Warn("matrix feedback_reactions/silence_reactions/thread_capture enabled but the access token env is empty; listener disabled", "env", mc.AccessTokenEnv)
		return nil
	}
	opts := []notify.MatrixFeedbackOption{notify.WithMetrics(metrics)}
	if mc.FeedbackReactions {
		opts = append(opts, notify.WithFeedbackReactions())
	}
	if mc.SilenceReactions {
		opts = append(opts, notify.WithSilenceReactions(cfg.Notify.Silence.Default(), cfg.Notify.Silence.MaxWindow.Std()))
	}
	if mc.ThreadCapture {
		if mention := buildMatrixThreadMention(cfg, responder, notifier, log); mention != nil {
			opts = append(opts, notify.WithThreadCapture(mention, dispatch, busyDispatch))
		}
	}
	return notify.NewMatrixFeedback(mc.Homeserver, mc.RoomID, tok, ledger, log, opts...)
}
