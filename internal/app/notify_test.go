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
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
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
func (fakeThreadForge) IsPROpen(context.Context, int) (bool, error) { return true, nil }

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

	m := BuildThreadMention(cfg, reg, fakeThreadForge{}, notifier, nil, log)
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
	m := BuildThreadMention(cfg, reg, fakeThreadForge{}, nil, nil, log)
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

	m := BuildThreadMention(cfg, reg, fakeThreadForge{}, notifier, nil, log)
	if m == nil {
		t.Fatal("must wire the handler when forge, bot-token delivery and the notifier are all reachable")
	}
	if !strings.Contains(buf.String(), "slack thread capture enabled") {
		t.Fatalf("must log that thread capture is enabled; got: %s", buf.String())
	}
}

// TestBuildThreadMentionWiresForgeWritesAndMetrics pins wiring that none of
// the other TestBuildThreadMention* tests check: with everything reachable,
// the returned Responder must actually carry the global rate limit and the
// metrics instrument set — the same idiom TestWireRecallSetsEveryRuntimeDependency
// uses for investigate.Recall (see wire_recall_test.go).
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

	m := BuildThreadMention(cfg, reg, fakeThreadForge{}, notifier, metrics, log)
	if m == nil {
		t.Fatal("must wire the handler when everything is reachable")
	}
	if m.Responder.ForgeWrites == nil {
		t.Fatal("Responder.ForgeWrites is nil — the global forge-write rate limit is unenforced in production")
	}
	if m.Responder.Metrics != metrics {
		t.Fatal("Responder.Metrics is not the *telemetry.Metrics passed to BuildThreadMention — " +
			"ThreadWritesThrottled/ThreadNotesWritten go dead")
	}
}

// TestBuildThreadMentionReportsMissingForge pins the unchanged forge==nil case.
func TestBuildThreadMentionReportsMissingForge(t *testing.T) {
	cfg := &config.Config{}
	cfg.Notify.Slack.ThreadCapture = true
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	m := BuildThreadMention(cfg, &thread.Registry{}, nil, nil, nil, log)
	if m != nil {
		t.Fatal("must not wire the handler when no forge is configured")
	}
	if !strings.Contains(buf.String(), "no forge is configured") {
		t.Fatalf("must name the missing forge; got: %s", buf.String())
	}
}
