// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
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
