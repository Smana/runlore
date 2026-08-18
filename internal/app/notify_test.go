// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/thread"
)

// TestSlackFeedbackDeliverable covers the runtime half of the feedback-buttons
// contract, the half Validate cannot see: an env var that is SET BUT EMPTY (an
// unmounted secret, a Helm value left blank) passes config validation and still
// leaves the Slack notifier skipped — no message, no buttons, no feedback — while
// startup used to log "slack feedback buttons enabled" regardless.
//
// So the guard reports whether a message can actually carry the buttons and warns
// when it cannot, exactly as BuildMatrixFeedback does for an empty Matrix access
// token. The warning must name the env var the operator has to fix.
func TestSlackFeedbackDeliverable(t *testing.T) {
	tests := []struct {
		name    string
		slack   config.SlackNotify
		env     map[string]string
		want    bool
		wantLog string // substring the warning must carry; "" ⇒ nothing may be logged
	}{
		{
			name:  "incoming webhook present",
			slack: config.SlackNotify{WebhookURLEnv: "TEST_SLACK_WEBHOOK_URL"},
			env:   map[string]string{"TEST_SLACK_WEBHOOK_URL": "https://hooks.slack.com/services/x"},
			want:  true,
		},
		{
			name:  "bot token present",
			slack: config.SlackNotify{BotTokenEnv: "TEST_SLACK_BOT_TOKEN", Channel: "C123"},
			env:   map[string]string{"TEST_SLACK_BOT_TOKEN": "xoxb-test"},
			want:  true,
		},
		{
			name:    "webhook env set but empty",
			slack:   config.SlackNotify{WebhookURLEnv: "TEST_SLACK_WEBHOOK_URL"},
			env:     map[string]string{"TEST_SLACK_WEBHOOK_URL": ""},
			want:    false,
			wantLog: "TEST_SLACK_WEBHOOK_URL",
		},
		{
			name:    "bot token env set but empty",
			slack:   config.SlackNotify{BotTokenEnv: "TEST_SLACK_BOT_TOKEN", Channel: "C123"},
			env:     map[string]string{"TEST_SLACK_BOT_TOKEN": ""},
			want:    false,
			wantLog: "TEST_SLACK_BOT_TOKEN",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			cfg := &config.Config{}
			cfg.Notify.Slack = tc.slack
			cfg.Notify.Slack.FeedbackButtons = true

			got := SlackFeedbackDeliverable(cfg, log)
			if got != tc.want {
				t.Fatalf("SlackFeedbackDeliverable = %v, want %v", got, tc.want)
			}
			if tc.wantLog == "" {
				if buf.Len() > 0 {
					t.Fatalf("a deliverable configuration must warn about nothing, got: %s", buf.String())
				}
				return
			}
			out := buf.String()
			if !strings.Contains(out, "feedback_buttons") || !strings.Contains(out, tc.wantLog) {
				t.Fatalf("the warning must name the feature and the env var %q, got: %s", tc.wantLog, out)
			}
			// Pinned phrase: configuration.md and integrations/notifications/slack.md
			// both tell operators to grep the startup logs for it, so rewording the
			// warning without updating those pages breaks the documented procedure.
			const documentedGrep = "no slack delivery target resolved"
			if !strings.Contains(out, documentedGrep) {
				t.Fatalf("the warning must contain the documented grep phrase %q, got: %s", documentedGrep, out)
			}
		})
	}
}

func TestBuildThreadRegistryDisabledWithoutCapture(t *testing.T) {
	cfg := &config.Config{}
	cfg.Outcome.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if reg.Enabled() {
		t.Fatal("thread capture off must yield a disabled registry")
	}
}

func TestBuildThreadRegistryUsesTheLedgerDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Outcome.LedgerPath = filepath.Join(dir, "ledger.jsonl")
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "T", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}

	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if !reg.Enabled() {
		t.Fatal("thread capture on with a ledger path must yield an enabled registry")
	}
	if err := reg.Put(thread.Context{Root: "1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "threads.jsonl")); err != nil {
		t.Fatalf("registry must persist beside the ledger: %v", err)
	}
}

// TestBuildThreadRegistryEnabledForMatrixOnly pins the Task 8 fix: the
// registry must persist for a deployment that only turns on
// notify.matrix.thread_capture (Slack's flag left off). Before this fix,
// BuildThreadRegistry's gate checked ONLY notify.slack.thread_capture, so a
// Matrix-only deployment always got a disabled (no-op) registry no matter how
// notify.matrix.thread_capture and outcome.ledger_path were set — Matrix
// thread capture could never work.
func TestBuildThreadRegistryEnabledForMatrixOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Outcome.LedgerPath = filepath.Join(dir, "ledger.jsonl")
	cfg.Notify.Matrix = config.MatrixNotify{
		Homeserver: "https://matrix.example.org", RoomID: "!room:example.org",
		AccessTokenEnv: "T", ThreadCapture: true,
	}

	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if !reg.Enabled() {
		t.Fatal("matrix thread_capture alone (slack's flag off) must yield an enabled registry")
	}
}

func TestBuildThreadRegistryDisabledWithoutLedgerPath(t *testing.T) {
	// The registry needs somewhere durable to live. Without a ledger path there is
	// no state directory, so capture degrades to unavailable rather than to
	// silently-forgetful.
	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "T", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if reg.Enabled() {
		t.Fatal("no ledger path must yield a disabled registry")
	}
}

func TestThreadCaptureDeliverable(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN_PRESENT", "xoxb-real")

	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "SLACK_BOT_TOKEN_PRESENT", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	if !ThreadCaptureDeliverable(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("a present bot token must be deliverable")
	}

	cfg.Notify.Slack.BotTokenEnv = "SLACK_BOT_TOKEN_ABSENT"
	if ThreadCaptureDeliverable(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("an empty bot-token env means no message is delivered, so no thread exists to reply in")
	}
}

// fakeThreadForge is a minimal thread.Forge stub — TestBuildThreadMention*
// only needs a non-nil forge to get past the "no forge configured" case; none
// of these tests exercise an actual write.
type fakeThreadForge struct{}

func (fakeThreadForge) CommentOnPR(context.Context, int, string) error { return nil }
func (fakeThreadForge) OpenPR(context.Context, providers.KBEntry) (providers.Ref, error) {
	return providers.Ref{}, nil
}
func (fakeThreadForge) IsPROpen(context.Context, int) (bool, error)                  { return true, nil }
func (fakeThreadForge) AppendToEntryOnPR(context.Context, int, string, string) error { return nil }

// TestBuildThreadMentionNamesTheBotTokenCause pins the fix for the dead-code
// warning: ThreadCaptureDeliverable's "no bot-token delivery target resolved"
// message names the SPECIFIC, actionable cause (an env var that resolves empty
// at runtime) and configuration.md / slack.md both tell operators to grep the
// logs for it. But reaching that warning's call site used to require
// replier != nil, which (since only *notify.SlackBot implements
// providers.ThreadNotifier, and it is only ever built when SlackBotDelivery
// resolves true) meant the "false" branch inside ThreadCaptureDeliverable
// could never execute in production — the generic "no thread-capable notifier
// resolved" message fired instead, every time, masking the real cause.
//
// This test builds a REAL *notify.Multi the way serve.go does (via
// notify.BuildEnabled against a config with the bot-token env present but
// empty at runtime — a mounted-but-blank secret) and asserts the specific
// message is what actually reaches the log.
func TestBuildThreadMentionNamesTheBotTokenCause(t *testing.T) {
	t.Setenv("TEST_THREAD_BOT_TOKEN_EMPTY", "")
	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "TEST_THREAD_BOT_TOKEN_EMPTY", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	notifier, err := notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}

	m := BuildThreadMention(cfg, &thread.Responder{Forge: fakeThreadForge{}, Registry: reg}, notifier, log)
	if m != nil {
		t.Fatal("must not wire the handler when the bot token cannot actually deliver")
	}
	out := buf.String()
	const documentedGrep = "no bot-token delivery target resolved"
	if !strings.Contains(out, documentedGrep) {
		t.Fatalf("must log the specific, documented cause %q; got: %s", documentedGrep, out)
	}
	if strings.Contains(out, "no thread-capable notifier resolved") {
		t.Fatalf("must not ALSO log the generic message once the specific cause is known; got: %s", out)
	}
}

// TestBuildThreadMentionStillReportsNoNotifierWhenNoneWasBuilt is the other
// half: when replier is nil for a reason ThreadCaptureDeliverable cannot see
// (no notifier built at all — the log-only, no-model path), the generic
// message must still fire.
func TestBuildThreadMentionStillReportsNoNotifierWhenNoneWasBuilt(t *testing.T) {
	t.Setenv("TEST_THREAD_BOT_TOKEN_PRESENT", "xoxb-real")
	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "TEST_THREAD_BOT_TOKEN_PRESENT", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// notifier is nil, exactly as serve.go passes it on the log-only (no model
	// configured) startup path.
	m := BuildThreadMention(cfg, &thread.Responder{Forge: fakeThreadForge{}, Registry: reg}, nil, log)
	if m != nil {
		t.Fatal("must not wire the handler when no notifier was built at all")
	}
	out := buf.String()
	if !strings.Contains(out, "no thread-capable notifier resolved") {
		t.Fatalf("must fall back to the generic message when the cause is not a bot-token one; got: %s", out)
	}
}

// TestBuildThreadMentionWiresWhenEverythingIsReachable is the success path:
// forge reachable, bot-token delivery resolves, so the handler is wired and
// the "enabled" line is logged.
func TestBuildThreadMentionWiresWhenEverythingIsReachable(t *testing.T) {
	t.Setenv("TEST_THREAD_BOT_TOKEN_OK", "xoxb-real")
	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "TEST_THREAD_BOT_TOKEN_OK", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	notifier, err := notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}

	m := BuildThreadMention(cfg, &thread.Responder{Forge: fakeThreadForge{}, Registry: reg}, notifier, log)
	if m == nil {
		t.Fatal("must wire the handler when forge, bot-token delivery and the notifier are all reachable")
	}
	if !strings.Contains(buf.String(), "slack thread capture enabled") {
		t.Fatalf("must log that thread capture is enabled; got: %s", buf.String())
	}
}

// TestBuildThreadMentionWiresForgeWritesAndMetrics pins wiring that none of
// the other TestBuildThreadMention* tests check: the shared Responder built by
// buildThreadResponder (see serve.go) and threaded through BuildThreadMention
// must actually carry the global rate limit and the metrics instrument set —
// the same idiom TestWireRecallSetsEveryRuntimeDependency uses for
// investigate.Recall (see wire_recall_test.go).
//
// Both assignments are load-bearing but invisible to the rest of the suite if
// dropped: Responder.write nil-checks ForgeWrites before calling it
// (`if r.ForgeWrites != nil && !r.ForgeWrites.Allow()`), so a missing
// ForgeWrites does not panic or fail any existing test — it just silently
// makes the one global write budget this feature has unlimited. Metrics is
// nil-safe the same way throughout Responder, so a missing Metrics silently
// drops ThreadWritesThrottled and ThreadNotesWritten to zero in production
// with nothing else noticing.
func TestBuildThreadMentionWiresForgeWritesAndMetrics(t *testing.T) {
	t.Setenv("TEST_THREAD_WIRING_BOT_TOKEN", "xoxb-real")
	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "TEST_THREAD_WIRING_BOT_TOKEN", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	notifier, err := notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}
	metrics := telemetry.NewMetrics()
	responder := buildThreadResponder(cfg, reg, fakeThreadForge{}, nil, nil, metrics, log)

	m := BuildThreadMention(cfg, responder, notifier, log)
	if m == nil {
		t.Fatal("must wire the handler when everything is reachable")
	}
	if m.Responder.ForgeWrites == nil {
		t.Fatal("Responder.ForgeWrites is nil — the global forge-write rate limit is unenforced in production")
	}
	if m.Responder.Metrics != metrics {
		t.Fatal("Responder.Metrics is not the *telemetry.Metrics passed to buildThreadResponder — " +
			"ThreadWritesThrottled/ThreadNotesWritten go dead")
	}
}

// TestBuildThreadResponderWiresTheForgeRepoRef pins that ForgeRepo is actually
// wired, and to the same host AND repository the forge client is built from.
// An unwired ForgeRepo is silent: routing simply stays unanchored, exactly as
// it was before the field existed, so no other test in the suite would notice
// — while a pull-request URL from any repository could again pick which PR a
// note lands on inside the operator's own knowledge base.
func TestBuildThreadResponderWiresTheForgeRepoRef(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := thread.NewRegistry("", time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, tt := range []struct {
		name string
		mut  func(*config.Config)
		want string
	}{
		{"github default", func(*config.Config) {}, "github.com/acme/kb"},
		{"github explicit api", func(c *config.Config) { c.Forge.GitHubAPIURL = "https://api.github.com" }, "github.com/acme/kb"},
		{"github enterprise", func(c *config.Config) { c.Forge.GitHubAPIURL = "https://ghe.example.com/api/v3" }, "ghe.example.com/acme/kb"},
		{"gitlab.com", func(c *config.Config) { c.Forge.Provider = "gitlab" }, "gitlab.com/acme/kb"},
		{"gitlab self-managed", func(c *config.Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.GitLab.BaseURL = "https://GitLab.Example.com"
		}, "gitlab.example.com/acme/kb"},
		{"gitlab nested group", func(c *config.Config) {
			c.Forge.Provider = "gitlab"
			c.Forge.KBRepo = "grp/sub/proj"
		}, "gitlab.com/grp/sub/proj"},
		// No kb_repo means no forge client either, so there is nothing to anchor
		// to — and anchoring to a bare host would be exactly the weak check
		// ForgeRepo replaced.
		{"no kb_repo", func(c *config.Config) { c.Forge.KBRepo = "" }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Forge.KBRepo = "acme/kb"
			tt.mut(cfg)
			if got := buildThreadResponder(cfg, reg, fakeThreadForge{}, nil, nil, nil, log).ForgeRepo; got != tt.want {
				t.Fatalf("ForgeRepo = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBuildThreadMentionReportsMissingForge pins the unchanged
// responder==nil/Forge==nil case.
func TestBuildThreadMentionReportsMissingForge(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Slack.ThreadCapture = true
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	m := BuildThreadMention(cfg, nil, nil, log)
	if m != nil {
		t.Fatal("must not wire the handler when no forge is configured")
	}
	if !strings.Contains(buf.String(), "no forge is configured") {
		t.Fatalf("must name the missing forge; got: %s", buf.String())
	}
}

// fakeMatrixReplier is a minimal providers.ThreadNotifier stub identifying as
// the "matrix" transport — enough for BuildMatrixFeedback's / buildMatrixThreadMention's
// tests to resolve a replier via Multi.ThreadRepliers() without a live homeserver.
type fakeMatrixReplier struct{}

func (fakeMatrixReplier) Deliver(context.Context, providers.Investigation) error { return nil }
func (fakeMatrixReplier) ReplyInThread(context.Context, string, string, string) error {
	return nil
}
func (fakeMatrixReplier) Transport() string { return "matrix" }

// readyMatrixResponder builds a *thread.Responder with a real, enabled
// registry (backed by a temp file) and a fake forge — everything
// buildMatrixThreadMention needs to succeed, for the test cases that want it
// reachable.
func readyMatrixResponder(t *testing.T) *thread.Responder {
	t.Helper()
	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return &thread.Responder{Forge: fakeThreadForge{}, Registry: reg}
}

// TestBuildMatrixThreadMentionReportsMissingForge mirrors
// TestBuildThreadMentionReportsMissingForge for the Matrix analogue.
func TestBuildMatrixThreadMentionReportsMissingForge(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Matrix.ThreadCapture = true
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	m := buildMatrixThreadMention(cfg, nil, nil, log)
	if m != nil {
		t.Fatal("must not wire the handler when no forge is configured")
	}
	if !strings.Contains(buf.String(), "no forge is configured") {
		t.Fatalf("must name the missing forge; got: %s", buf.String())
	}
}

// TestBuildMatrixThreadMentionReportsDisabledRegistry pins the registry guard:
// a responder whose Registry is disabled (no path — e.g. outcome.ledger_path
// unset) must degrade rather than wire a Mention no reply could ever be
// attributed through.
func TestBuildMatrixThreadMentionReportsDisabledRegistry(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Matrix.ThreadCapture = true
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	responder := &thread.Responder{Forge: fakeThreadForge{}, Registry: &thread.Registry{}} // disabled: empty path
	m := buildMatrixThreadMention(cfg, responder, nil, log)
	if m != nil {
		t.Fatal("must not wire the handler when the thread registry is disabled")
	}
	if !strings.Contains(buf.String(), "thread registry is unavailable") {
		t.Fatalf("must name the disabled registry; got: %s", buf.String())
	}
}

// TestBuildMatrixFeedbackNeitherOptionOnIsNil pins the compound top gate: with
// BOTH notify.matrix.feedback_reactions and notify.matrix.thread_capture off,
// no listener is built at all — same as before Task 8.
func TestBuildMatrixFeedbackNeitherOptionOnIsNil(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Matrix = config.MatrixNotify{Homeserver: "https://matrix.example.org", RoomID: "!room:x", AccessTokenEnv: "T"}
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	mfb := BuildMatrixFeedback(cfg, ledger, nil, nil, nil, nil, nil, log)
	if mfb != nil {
		t.Fatal("neither option on must yield no listener")
	}
}

// TestBuildMatrixFeedbackTokenEmptyDisables pins the unchanged runtime-empty
// access-token guard, now covering both options (message must not go stale
// once thread_capture can also request the listener).
func TestBuildMatrixFeedbackTokenEmptyDisables(t *testing.T) {
	t.Setenv("TEST_MATRIX_TOKEN_EMPTY", "")
	cfg := &config.Config{}
	cfg.Notify.Matrix = config.MatrixNotify{
		Homeserver: "https://matrix.example.org", RoomID: "!room:x",
		AccessTokenEnv: "TEST_MATRIX_TOKEN_EMPTY", FeedbackReactions: true,
	}
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	mfb := BuildMatrixFeedback(cfg, ledger, nil, nil, nil, nil, nil, log)
	if mfb != nil {
		t.Fatal("an empty access token must disable the listener")
	}
	if !strings.Contains(buf.String(), "TEST_MATRIX_TOKEN_EMPTY") {
		t.Fatalf("must name the empty env var; got: %s", buf.String())
	}
}

// TestBuildMatrixFeedbackThreadCaptureWiresStandalone pins the Task 8 fix at
// the top gate: the listener must build — and thread capture must wire — from
// notify.matrix.thread_capture ALONE, with feedback_reactions off. Before this
// wiring, BuildMatrixFeedback's top gate checked ONLY FeedbackReactions, so
// thread_capture had no observable effect no matter what Task 6/7 built (see
// task-6-report.md's "Concerns").
func TestBuildMatrixFeedbackThreadCaptureWiresStandalone(t *testing.T) {
	t.Setenv("TEST_MATRIX_TOKEN_STANDALONE", "matrix-token")
	cfg := &config.Config{}
	cfg.Notify.Matrix = config.MatrixNotify{
		Homeserver: "https://matrix.example.org", RoomID: "!room:x",
		AccessTokenEnv: "TEST_MATRIX_TOKEN_STANDALONE", ThreadCapture: true, // FeedbackReactions left false
	}
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	responder := readyMatrixResponder(t)
	dispatch := thread.NewDispatcher(4, time.Minute, log)
	busyDispatch := thread.NewDispatcher(4, time.Minute, log)
	notifier := notify.NewMulti(log, fakeMatrixReplier{})

	mfb := BuildMatrixFeedback(cfg, ledger, responder, dispatch, busyDispatch, notifier, nil, log)
	if mfb == nil {
		t.Fatal("thread_capture alone (feedback_reactions off) must still build the listener")
	}
	if mfb.Mentions == nil || mfb.Dispatch == nil || mfb.BusyDispatch == nil {
		t.Fatal("thread_capture on with a reachable registry, forge and replier must wire Mentions, Dispatch and BusyDispatch")
	}
	if !strings.Contains(buf.String(), "matrix thread capture enabled") {
		t.Fatalf("must log that thread capture is enabled; got: %s", buf.String())
	}
}

// TestBuildMatrixFeedbackThreadCaptureOffLeavesNeitherWired pins "supplying no
// option must leave thread capture off": with thread_capture off (even with
// everything else reachable), the built listener carries neither Mentions nor
// Dispatch, exactly reproducing the pre-thread-capture listener.
func TestBuildMatrixFeedbackThreadCaptureOffLeavesNeitherWired(t *testing.T) {
	t.Setenv("TEST_MATRIX_TOKEN_OFF", "matrix-token")
	cfg := &config.Config{}
	cfg.Notify.Matrix = config.MatrixNotify{
		Homeserver: "https://matrix.example.org", RoomID: "!room:x",
		AccessTokenEnv: "TEST_MATRIX_TOKEN_OFF", FeedbackReactions: true, // thread_capture left false
	}
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	responder := readyMatrixResponder(t)
	dispatch := thread.NewDispatcher(4, time.Minute, log)
	busyDispatch := thread.NewDispatcher(4, time.Minute, log)
	notifier := notify.NewMulti(log, fakeMatrixReplier{})

	mfb := BuildMatrixFeedback(cfg, ledger, responder, dispatch, busyDispatch, notifier, nil, log)
	if mfb == nil {
		t.Fatal("feedback_reactions on must still build the listener")
	}
	if mfb.Mentions != nil || mfb.Dispatch != nil || mfb.BusyDispatch != nil {
		t.Fatal("thread_capture off must leave Mentions, Dispatch and BusyDispatch nil")
	}
}

// TestBuildMatrixFeedbackDegradesWithoutReplier pins the no-panic degrade:
// with thread_capture on but no notifier built at all (so no Matrix replier is
// resolvable), the listener still builds (feedback_reactions covers that) but
// leaves thread capture off and logs the specific cause — never panics.
func TestBuildMatrixFeedbackDegradesWithoutReplier(t *testing.T) {
	t.Setenv("TEST_MATRIX_TOKEN_NOREPLIER", "matrix-token")
	cfg := &config.Config{}
	cfg.Notify.Matrix = config.MatrixNotify{
		Homeserver: "https://matrix.example.org", RoomID: "!room:x",
		AccessTokenEnv: "TEST_MATRIX_TOKEN_NOREPLIER", FeedbackReactions: true, ThreadCapture: true,
	}
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	responder := readyMatrixResponder(t)
	dispatch := thread.NewDispatcher(4, time.Minute, log)
	busyDispatch := thread.NewDispatcher(4, time.Minute, log)

	// notifier is nil: no notifier built at all — the same "no replier resolvable"
	// case BuildThreadMention's Slack analogue covers.
	mfb := BuildMatrixFeedback(cfg, ledger, responder, dispatch, busyDispatch, nil, nil, log)
	if mfb == nil {
		t.Fatal("feedback_reactions on must still build the listener even when thread capture cannot wire")
	}
	if mfb.Mentions != nil || mfb.Dispatch != nil || mfb.BusyDispatch != nil {
		t.Fatal("no resolvable Matrix replier must leave thread capture off")
	}
	if !strings.Contains(buf.String(), "no Matrix thread-capable notifier resolved") {
		t.Fatalf("must log the specific cause; got: %s", buf.String())
	}
}

// chatCfg returns a config with model.chat configured and thread capture on,
// the shape every TestBuildThreadChat* case starts from.
func chatCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Model.Provider = "anthropic"
	cfg.Model.Model = "big-expensive-model"
	cfg.Model.APIKeyEnv = "TEST_CHAT_PARENT_KEY"
	cfg.Notify.Slack.ThreadCapture = true
	return cfg
}

// TestBuildThreadChatOffWithoutModelChat pins the feature switch: the presence
// of the model.chat block IS the switch (BuildChatModel's contract), so an
// absent block must yield no Chat at all — not an inert one. A nil *thread.Chat
// on the Responder is what makes the freeform route behave exactly as it did in
// PR2, which is the contract the responder's own tests assert.
func TestBuildThreadChatOffWithoutModelChat(t *testing.T) {
	cfg := chatCfg()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if got := buildThreadChat(cfg, nil, nil, log); got != nil {
		t.Fatalf("buildThreadChat with no model.chat = %+v, want nil (feature off)", got)
	}
	if buf.Len() != 0 {
		t.Fatalf("an absent model.chat is the default, not a problem; must log nothing, got: %s", buf.String())
	}
}

// TestBuildThreadChatWiresEveryField is the test this whole task exists for.
//
// Every field on thread.Chat is nil/zero-safe by design, so an UNWIRED field
// degrades silently instead of failing: a missing Budget allows every call with
// no ceiling at all, a missing Catalog quietly drops KB context from the
// prompt, a missing MaxOutputTokens under-charges the budget against the real
// cap an operator configured. None of that fails any other test in this suite —
// which is exactly why this one enumerates the fields explicitly rather than
// asserting the Chat is merely non-nil.
func TestBuildThreadChatWiresEveryField(t *testing.T) {
	t.Setenv("TEST_CHAT_PARENT_KEY", "sk-parent")
	cfg := chatCfg()
	cfg.Model.Chat = &config.ModelOverride{Model: "small-cheap-model", MaxTokens: 4096}
	cfg.Notify.Thread.MaxNoteBytes = 4096
	cfg.Notify.Thread.ChatCallsPerHour = 7
	cfg.Notify.Thread.ChatTokensPerHour = 12345

	cat := &catalog.Catalog{}
	metrics := telemetry.NewMetrics()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	chat := buildThreadChat(cfg, cat, metrics, log)
	if chat == nil {
		t.Fatal("model.chat configured must build the chat layer")
	}
	if chat.Model == nil {
		t.Fatal("Chat.Model is nil — Answer returns false on every message and the paid feature is silently off")
	}
	if chat.Budget == nil {
		t.Fatal("Chat.Budget is nil — every call is allowed, the spend has no ceiling at all")
	}
	calls, tokens := chat.Budget.Remaining()
	if calls != 7 || tokens != 12345 {
		t.Fatalf("Budget carries (%d calls, %d tokens), want the configured (7, 12345) from "+
			"notify.thread.chat_calls_per_hour/chat_tokens_per_hour", calls, tokens)
	}
	if chat.Catalog != catalog.Searcher(cat) {
		t.Fatalf("Chat.Catalog = %v, want the shared catalog — without it replies lose KB context silently", chat.Catalog)
	}
	if chat.MaxNoteBytes != cfg.Notify.Thread.EffectiveMaxNoteBytes() {
		t.Fatalf("Chat.MaxNoteBytes = %d, want notify.thread.max_note_bytes (%d) — the same bound the "+
			"Responder writes under", chat.MaxNoteBytes, cfg.Notify.Thread.EffectiveMaxNoteBytes())
	}
	if chat.MaxOutputTokens != chatMaxTokens(cfg) {
		t.Fatalf("Chat.MaxOutputTokens = %d, want model.chat.max_tokens (%d) — otherwise the conservative "+
			"charge applied when a provider reports no usage under-bills against the real cap",
			chat.MaxOutputTokens, chatMaxTokens(cfg))
	}
	if chat.Metrics != metrics {
		t.Fatal("Chat.Metrics is not the process instrument set — thread_chat_* telemetry goes dead")
	}
	if chat.Log == nil {
		t.Fatal("Chat.Log is nil — every degraded path logs to the package default instead of the process logger")
	}
}

// TestBuildThreadChatDegradesWithoutCatalog covers the no-catalog case: the
// layer still builds and still answers, only without KB excerpts. The nil must
// reach Chat.Catalog as a nil INTERFACE — assigning a typed-nil *catalog.Catalog
// would make Chat.Catalog != nil true, and catalogHits would call Search on a nil
// receiver instead of taking its documented no-hits branch.
func TestBuildThreadChatDegradesWithoutCatalog(t *testing.T) {
	t.Setenv("TEST_CHAT_PARENT_KEY", "sk-parent")
	cfg := chatCfg()
	cfg.Model.Chat = &config.ModelOverride{Model: "small-cheap-model"}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	chat := buildThreadChat(cfg, nil, nil, log)
	if chat == nil {
		t.Fatal("no catalog must degrade the reply quality, never disable the layer")
	}
	if chat.Catalog != nil {
		t.Fatalf("Chat.Catalog = %v, want a nil interface (a typed-nil would panic in catalogHits)", chat.Catalog)
	}
	if !strings.Contains(buf.String(), "no catalog") {
		t.Fatalf("must warn that replies carry no knowledge-base context; got: %s", buf.String())
	}
}

// TestBuildThreadChatWarnsWhenTheCredentialIsEmpty covers the runtime half of
// the contract Validate cannot see, the same gap ThreadCaptureDeliverable and
// SlackFeedbackDeliverable close for their transports: an api_key_env that is
// set but EMPTY (an unmounted secret, a blank Helm value) builds a client that
// fails on every call. The layer still builds — Answer degrades to deterministic
// capture on a provider error, which is the designed behaviour — but startup
// must name the env var rather than announce a working feature.
func TestBuildThreadChatWarnsWhenTheCredentialIsEmpty(t *testing.T) {
	t.Setenv("TEST_CHAT_PARENT_KEY", "")
	cfg := chatCfg()
	cfg.Model.Chat = &config.ModelOverride{Model: "small-cheap-model"}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	chat := buildThreadChat(cfg, nil, nil, log)
	if chat == nil {
		t.Fatal("an empty credential must degrade at call time, not disable the layer at startup")
	}
	if !strings.Contains(buf.String(), "TEST_CHAT_PARENT_KEY") {
		t.Fatalf("the warning must name the env var an operator has to fix; got: %s", buf.String())
	}
}

// TestBuildThreadChatWarnsWhenNoTransportListens pins that
// config.ChatWithoutCaptureWarning is actually EMITTED in production. The
// warning catches an operator who configured a paid model for a feature no
// transport can ever trigger; a guard with unit coverage and no production
// caller is green and inert, which is the failure this test exists to prevent.
func TestBuildThreadChatWarnsWhenNoTransportListens(t *testing.T) {
	t.Setenv("TEST_CHAT_PARENT_KEY", "sk-parent")
	cfg := chatCfg()
	cfg.Notify.Slack.ThreadCapture = false
	cfg.Model.Chat = &config.ModelOverride{Model: "small-cheap-model"}
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	buildThreadChat(cfg, nil, nil, log)
	if want := config.ChatWithoutCaptureWarning(cfg); want == "" {
		t.Fatal("test setup is wrong: this config must warrant the warning")
	} else if !strings.Contains(buf.String(), "dead config") {
		t.Fatalf("must emit ChatWithoutCaptureWarning at startup; got: %s", buf.String())
	}
}

// TestBuildThreadResponderCarriesTheChatLayer pins the last link in the chain:
// the Chat must reach the ONE shared *thread.Responder both transports use.
// Responder.freeform nil-checks Chat before every use, so a Chat built and then
// dropped on the floor fails nothing — it just leaves the feature off in
// production while every unit test of Chat itself stays green.
func TestBuildThreadResponderCarriesTheChatLayer(t *testing.T) {
	t.Setenv("TEST_CHAT_PARENT_KEY", "sk-parent")
	cfg := chatCfg()
	cfg.Model.Chat = &config.ModelOverride{Model: "small-cheap-model"}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	chat := buildThreadChat(cfg, nil, nil, log)
	if chat == nil {
		t.Fatal("test setup is wrong: model.chat is configured")
	}

	r := buildThreadResponder(cfg, reg, fakeThreadForge{}, chat, nil, nil, log)
	if r.Chat != chat {
		t.Fatal("Responder.Chat is not the shared chat layer — freeform stays on the PR2 deterministic path in production")
	}
	// And the PR2 shape survives: no model.chat means no Chat on the responder.
	off := buildThreadResponder(chatCfg(), reg, fakeThreadForge{}, buildThreadChat(chatCfg(), nil, nil, log), nil, nil, log)
	if off.Chat != nil {
		t.Fatalf("no model.chat must leave Responder.Chat nil, got %+v", off.Chat)
	}
}

// TestKBAnnounceInertWarning covers the guard on its own: the operator asked for
// knowledge-base announcements and one of the two ends the feature needs is
// missing. Every dependency on this path is nil-safe — a nil *notify.Multi means
// "announcements are off" and nothing anywhere fails — so without this the
// startup log would be silent about a feature the operator explicitly turned on
// and will never see fire.
//
// The two causes are asserted by their remedy, not merely by "something was
// logged": an operator told to configure a notifier when what they are missing
// is thread_capture is worse off than one told nothing, because they will go and
// check the notifier.
func TestKBAnnounceInertWarning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		announce config.AnnounceMode
		capture  bool
		sinks    int
		wantCue  string // substring the message must carry; "" ⇒ no warning at all
	}{
		{name: "off, with nothing else configured either: the default warrants nothing"},
		{name: "off, with everything else configured", sinks: 2, capture: true},
		{name: "explicitly off, with everything else configured", announce: config.AnnounceOff, sinks: 2, capture: true},
		{name: "on, with a notifier and capture", announce: config.AnnounceChannel, sinks: 1, capture: true, wantCue: ""},
		{name: "on, with capture but no notifier", announce: config.AnnounceChannel, capture: true, wantCue: "no notifier is configured"},
		{name: "on, with a notifier but nothing capturing", announce: config.AnnounceChannel, sinks: 1, wantCue: "thread_capture"},
		{
			// Both ends missing: one message, and it must be the delivery one —
			// reporting them in a fixed order keeps the line deterministic.
			name: "on, with neither end", announce: config.AnnounceChannel, wantCue: "no notifier is configured",
		},
		// Routing does not change reachability: a thread-routed announcement still
		// needs a sink and still needs a transport capturing, and an operator who
		// wrote the new value must get the same diagnostic as one who wrote true.
		{name: "thread-routed, with capture but no notifier", announce: config.AnnounceThread, capture: true, wantCue: "no notifier is configured"},
		{name: "both, with a notifier but nothing capturing", announce: config.AnnounceBoth, sinks: 1, wantCue: "thread_capture"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Notify.Thread.AnnounceKBUpdates = tc.announce
			cfg.Notify.Slack.ThreadCapture = tc.capture
			got := KBAnnounceInertWarning(cfg, tc.sinks)
			if tc.wantCue == "" {
				if got != "" {
					t.Fatalf("KBAnnounceInertWarning = %q, want no warning", got)
				}
				return
			}
			// The message has to name the key the operator wrote, so the line is
			// actionable from the log alone rather than requiring them to guess
			// which of several notification options is inert.
			if !strings.Contains(got, "announce_kb_updates") || !strings.Contains(got, tc.wantCue) {
				t.Fatalf("the warning must name notify.thread.announce_kb_updates and the missing end %q, got: %q",
					tc.wantCue, got)
			}
		})
	}
}

// TestKBAnnounceInertWarningSeesMatrixCaptureToo pins the capture check against
// the same Matrix-only deployment BuildThreadRegistry once got wrong: a
// transport is a transport, and reading only notify.slack.thread_capture would
// call a working Matrix-only setup dead config.
func TestKBAnnounceInertWarningSeesMatrixCaptureToo(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Thread.AnnounceKBUpdates = config.AnnounceChannel
	cfg.Notify.Matrix.ThreadCapture = true
	if got := KBAnnounceInertWarning(cfg, 1); got != "" {
		t.Fatalf("matrix thread_capture alone must satisfy the capture end, got: %q", got)
	}
}

// TestBuildThreadResponderAnnouncerIsOptIn pins the switch END TO END through
// the builder RunServe actually calls, not through a hand-built Responder.
//
// Responder.Announcer is nil-safe by contract (nil means announcements are
// off), so every wrong answer here is silent: an announcer built when nobody
// asked fans note content out to channels the operator never opted in, and one
// NOT built when they did asked leaves the feature dead with no diagnostic.
// Neither fails anything on its own.
//
// The two "on but nowhere to deliver" rows are the reason the builder consults
// the notifier at all rather than only the config bool: a *notify.Multi with no
// sinks in it delivers every announcement to nobody, so wiring an announcer to
// it would spend a dispatcher and its goroutines to produce nothing, while
// startup implied the feature was live.
func TestBuildThreadResponderAnnouncerIsOptIn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		announce config.AnnounceMode
		notifier func() *notify.Multi
		want     bool // want an announcer on the responder
		wantWarn bool
	}{
		{
			name:     "off, with a notifier configured",
			notifier: func() *notify.Multi { return notify.NewMulti(discardLog(), &captureNotifier{}) },
		},
		{
			name: "off, with no notifier at all: nothing was asked for, so nothing is said",
		},
		{
			name:     "explicitly off, with a notifier configured",
			announce: config.AnnounceOff,
			notifier: func() *notify.Multi { return notify.NewMulti(discardLog(), &captureNotifier{}) },
		},
		{
			name:     "on, with a notifier configured",
			announce: config.AnnounceChannel,
			notifier: func() *notify.Multi { return notify.NewMulti(discardLog(), &captureNotifier{}) },
			want:     true,
		},
		{
			// Every destination is the same switch: routing decides WHERE, never
			// whether. A mode that built no announcer would be a key that reads as
			// enabled and does nothing.
			name:     "thread-routed, with a notifier configured",
			announce: config.AnnounceThread,
			notifier: func() *notify.Multi { return notify.NewMulti(discardLog(), &captureNotifier{}) },
			want:     true,
		},
		{
			name:     "both, with a notifier configured",
			announce: config.AnnounceBoth,
			notifier: func() *notify.Multi { return notify.NewMulti(discardLog(), &captureNotifier{}) },
			want:     true,
		},
		{
			name:     "on, but no notifier was built at all",
			announce: config.AnnounceChannel,
			wantWarn: true,
		},
		{
			name:     "on, but the notifier holds no sinks",
			announce: config.AnnounceChannel,
			notifier: func() *notify.Multi { return notify.NewMulti(discardLog()) },
			wantWarn: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			cfg := &config.Config{}
			cfg.Notify.Thread.AnnounceKBUpdates = tc.announce
			// Capture on, so the only thing varying across the table is the
			// sink: with no transport capturing, KBAnnounceInertWarning would
			// fire for a second reason and every row would look the same.
			cfg.Notify.Slack.ThreadCapture = true
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			var notifier *notify.Multi
			if tc.notifier != nil {
				notifier = tc.notifier()
			}

			r := buildThreadResponder(cfg, reg, fakeThreadForge{}, nil, notifier, nil, log)
			if got := r.Announcer != nil; got != tc.want {
				t.Fatalf("Responder.Announcer != nil = %v, want %v — announcements are opt-in and must be "+
					"wired exactly when notify.thread.announce_kb_updates is on AND a sink exists", got, tc.want)
			}
			if warned := strings.Contains(buf.String(), "announce_kb_updates"); warned != tc.wantWarn {
				t.Fatalf("warned about an inert announce_kb_updates = %v, want %v; log: %s",
					warned, tc.wantWarn, buf.String())
			}
		})
	}
}

// kbSinkNotifier is a Notifier that also accepts announcements, so the fan-out
// the builder wires actually reaches something a test can read.
type kbSinkNotifier struct {
	mu  sync.Mutex
	got []providers.KBUpdate
}

func (*kbSinkNotifier) Deliver(context.Context, providers.Investigation) error { return nil }

func (s *kbSinkNotifier) DeliverKBUpdate(_ context.Context, up providers.KBUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, up)
	return nil
}

func (s *kbSinkNotifier) updates() []providers.KBUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]providers.KBUpdate(nil), s.got...)
}

// TestAnnounceDestinationReachesTheSink closes the wiring gap between the key an
// operator writes and the routing decision a notifier acts on.
//
// Every piece of this is unit-tested on its own: config resolves the mode to a
// providers.KBDelivery, the announcer stamps its delivery onto the event, and
// the notifiers route on that stamp. None of that is worth anything if the
// builder passes a literal instead of the configured value — `thread` in the
// file would read as enabled, log as enabled, and still post to the channel,
// which is precisely the complaint the destination exists to answer. The only
// thing that catches it is driving the real builder from a real config and
// reading what arrives at the sink.
//
// The thread handles are asserted alongside the destination because a routing
// instruction with no address is inert: a sink told to reply in a thread, given
// no channel, falls back — silently, and correctly — so the destination alone
// proves nothing about whether the announcement can ever reach a thread.
func TestAnnounceDestinationReachesTheSink(t *testing.T) {
	for _, tc := range []struct {
		mode config.AnnounceMode
		want providers.KBDelivery
	}{
		{mode: config.AnnounceChannel, want: providers.KBDeliverChannel},
		{mode: config.AnnounceThread, want: providers.KBDeliverThread},
		{mode: config.AnnounceBoth, want: providers.KBDeliverBoth},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			origin := thread.Context{Transport: "slack", Root: "111.222", Channel: "C-ORIGIN", Title: "OOM"}
			if err := reg.Put(origin); err != nil {
				t.Fatalf("Put: %v", err)
			}

			cfg := &config.Config{}
			cfg.Notify.Thread.AnnounceKBUpdates = tc.mode
			cfg.Notify.Slack.ThreadCapture = true
			sink := &kbSinkNotifier{}
			r := buildThreadResponder(cfg, reg, fakeThreadForge{}, nil, notify.NewMulti(discardLog(), sink), nil, discardLog())
			if r.Announcer == nil {
				t.Fatal("no announcer was wired; every destination is the same switch")
			}

			if _, err := r.Handle(context.Background(), origin, "alice", "<@U0BOT> note: spot reclaim, not OOM"); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r.Announcer.Drain(ctx)

			got := sink.updates()
			if len(got) != 1 {
				t.Fatalf("the sink received %d announcements, want exactly 1 per landed write", len(got))
			}
			if got[0].Delivery != tc.want {
				t.Errorf("announce_kb_updates: %s reached the sink as Delivery %q, want %q — the builder is not "+
					"passing the configured destination through", tc.mode, string(got[0].Delivery), string(tc.want))
			}
			if got[0].Root != "111.222" || got[0].Channel != "C-ORIGIN" {
				t.Errorf("the announcement carries root=%q channel=%q, want both handles from the thread context — "+
					"without them every thread-routed delivery silently falls back to the channel",
					got[0].Root, got[0].Channel)
			}
		})
	}
}

// threadBoundsCfg returns a config with EVERY notify.thread BOUND set to a
// distinctive non-default value, so a bound that arrives with its default
// cannot be mistaken for one that was delivered. Chat capture and a ledger path
// are on because BuildThreadRegistry needs both to build an enabled registry.
//
// announce_kb_updates is deliberately not here: it is the one key in the block
// that is a switch rather than a bound, so "the value that arrived is not the
// default" is not a question that can be asked of it — a bool has no third
// state. TestBuildThreadResponderAnnouncerIsOptIn covers its delivery instead,
// through the same builder.
func threadBoundsCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg := chatCfg()
	cfg.Model.Chat = &config.ModelOverride{Model: "small-cheap-model"}
	cfg.Outcome.LedgerPath = filepath.Join(t.TempDir(), "outcome.jsonl")
	cfg.Notify.Thread = config.ThreadNotify{
		MaxNotesPerThread:  7,
		ForgeWritesPerHour: 3,
		RegistryTTL:        config.Duration(90 * time.Minute),
		RegistryMax:        2,
		MaxNoteBytes:       4321,
		ChatCallsPerHour:   11,
		ChatTokensPerHour:  4242,
	}
	return cfg
}

// TestNotifyThreadBoundsReachTheObjectsTheyConfigure pins DELIVERY for every
// notify.thread key — the half the config tests cannot see.
//
// config_test.go covers parsing and defaulting thoroughly: given YAML, the
// right number comes out of the Effective* resolver. What nothing covered is
// whether that number reaches the object it configures. Replacing any of these
// five with a hardcoded default in this file left the whole suite green:
//
//	MaxNoteBytes:      cfg.Notify.Thread.EffectiveMaxNoteBytes()      → 0
//	MaxNotesPerThread: cfg.Notify.Thread.EffectiveMaxNotesPerThread() → 0
//	ForgeWrites:       …EffectiveForgeWritesPerHour()                 → the default
//	ttl :=             …EffectiveRegistryTTL()                        → the default
//	maxLive :=         …EffectiveRegistryMax()                        → the default
//
// Each is silent in production: the operator who raised a bound sees the
// documented behaviour of the value they did NOT configure, with no error and
// no log line saying so. The two chat bounds are here too, so the table is the
// whole block rather than "the ones that broke" — and max_note_bytes gets two
// rows, because "one configured value covers the message on the way to the
// model AND on the way to the knowledge base" is a claim this code makes and
// only one of the two surfaces was pinned.
//
// The bounds that have no getter are asserted through BEHAVIOUR (a fourth forge
// write is refused, a thread past the TTL stops being answerable, the oldest
// thread is evicted at the cap) rather than through reflection: what an operator
// is promised is the behaviour, and it is what a hardcoded default changes.
func TestNotifyThreadBoundsReachTheObjectsTheyConfigure(t *testing.T) {
	t.Setenv("TEST_CHAT_PARENT_KEY", "sk-parent")
	for _, tc := range []struct {
		key   string
		check func(t *testing.T, reg *thread.Registry, r *thread.Responder, c *thread.Chat)
	}{
		{
			key: "max_notes_per_thread",
			check: func(t *testing.T, _ *thread.Registry, r *thread.Responder, _ *thread.Chat) {
				if r.MaxNotesPerThread != 7 {
					t.Errorf("Responder.MaxNotesPerThread = %d, want the configured 7 — a thread keeps "+
						"writing knowledge past the cap the operator set", r.MaxNotesPerThread)
				}
			},
		},
		{
			key: "forge_writes_per_hour",
			check: func(t *testing.T, _ *thread.Registry, r *thread.Responder, _ *thread.Chat) {
				if r.ForgeWrites == nil {
					t.Fatal("Responder.ForgeWrites is nil — thread capture's one global cap on forge writes is absent entirely")
				}
				if got := r.ForgeWrites.Window(); got != time.Hour {
					t.Errorf("ForgeWrites window = %s, want 1h — the key is per HOUR", got)
				}
				for i := 1; i <= 3; i++ {
					if !r.ForgeWrites.Allow() {
						t.Fatalf("forge write %d of the configured 3 was refused", i)
					}
				}
				if r.ForgeWrites.Allow() {
					t.Error("a 4th forge write was allowed against the configured budget of 3 — the " +
						"default (20) is in force and the operator's ceiling is 6.7x what they asked for")
				}
			},
		},
		{
			key: "max_note_bytes → the write path",
			check: func(t *testing.T, _ *thread.Registry, r *thread.Responder, _ *thread.Chat) {
				if r.MaxNoteBytes != 4321 {
					t.Errorf("Responder.MaxNoteBytes = %d, want the configured 4321 — the message written "+
						"to the knowledge base is bounded by the default instead", r.MaxNoteBytes)
				}
			},
		},
		{
			key: "max_note_bytes → the prompt path",
			check: func(t *testing.T, _ *thread.Registry, _ *thread.Responder, c *thread.Chat) {
				if c.MaxNoteBytes != 4321 {
					t.Errorf("Chat.MaxNoteBytes = %d, want the configured 4321 — the two surfaces have "+
						"drifted, so one configured value no longer covers both", c.MaxNoteBytes)
				}
			},
		},
		{
			key: "registry_ttl",
			check: func(t *testing.T, reg *thread.Registry, _ *thread.Responder, _ *thread.Chat) {
				mustPut(t, reg, thread.Context{Root: "stale", At: time.Now().Add(-2 * time.Hour)})
				mustPut(t, reg, thread.Context{Root: "fresh"})
				if _, ok := reg.Get("stale"); ok {
					t.Error("a thread last touched 2h ago is still answerable under the configured 90m " +
						"registry_ttl — the default (168h) is in force, so threads stay live for a week")
				}
				if _, ok := reg.Get("fresh"); !ok {
					t.Error("a thread touched just now is not answerable — the TTL is far shorter than configured")
				}
			},
		},
		{
			key: "registry_max",
			check: func(t *testing.T, reg *thread.Registry, _ *thread.Responder, _ *thread.Chat) {
				for _, root := range []string{"first", "second", "third"} {
					mustPut(t, reg, thread.Context{Root: root})
				}
				if _, ok := reg.Get("first"); ok {
					t.Error("the oldest of 3 threads survived a configured registry_max of 2 — the " +
						"default (2000) is in force and the live set is unbounded for any real channel")
				}
				if _, ok := reg.Get("third"); !ok {
					t.Error("the newest thread was evicted — eviction is not keeping the most recent entries")
				}
			},
		},
		{
			key: "chat_calls_per_hour / chat_tokens_per_hour",
			check: func(t *testing.T, _ *thread.Registry, _ *thread.Responder, c *thread.Chat) {
				calls, tokens := c.Budget.Remaining()
				if calls != 11 || tokens != 4242 {
					t.Errorf("Chat.Budget allows (%d calls, %d tokens), want the configured (11, 4242) — "+
						"the hourly spend ceiling is not the one the operator set", calls, tokens)
				}
			},
		},
	} {
		t.Run(tc.key, func(t *testing.T) {
			cfg := threadBoundsCfg(t)
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			reg, err := BuildThreadRegistry(cfg)
			if err != nil {
				t.Fatalf("BuildThreadRegistry: %v", err)
			}
			if !reg.Enabled() {
				t.Fatal("test setup is wrong: the registry must persist for these bounds to be observable")
			}
			chat := buildThreadChat(cfg, nil, nil, log)
			if chat == nil {
				t.Fatal("test setup is wrong: model.chat is configured")
			}
			tc.check(t, reg, buildThreadResponder(cfg, reg, fakeThreadForge{}, chat, nil, nil, log), chat)
		})
	}
}

// mustPut records a thread context or fails the test — a Put that errored would
// make an eviction/expiry assertion below pass for the wrong reason.
func mustPut(t *testing.T, reg *thread.Registry, tc thread.Context) {
	t.Helper()
	if err := reg.Put(tc); err != nil {
		t.Fatalf("Registry.Put(%q): %v", tc.Root, err)
	}
}
