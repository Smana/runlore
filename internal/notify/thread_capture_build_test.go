// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// TestBuildEnabledWiresThreadCaptureForEveryTransport is the regression test
// for the defect where notify.matrix's Build closure constructed a *Matrix but
// never assigned its Threads sink: every test that exercised registration
// built Matrix/SlackBot directly and set Threads by hand, bypassing Build
// entirely, so the miswiring shipped green. This test goes through the REAL
// notify.BuildEnabled → Descriptor.Build registry path — the same path
// app.BuildNotifier uses in production — for BOTH thread-capable transports,
// deliberately side by side: asserting only one (e.g. "at least one notifier
// has Threads set") would let a broken second transport hide behind a
// passing first one. This does NOT generalise to a hypothetical third
// thread-capable transport, though: the assertions below look up
// repliers["matrix"] and repliers["slack"] by name, so a third transport that
// forgets to wire its sink would simply never be checked here and would pass
// silently. Extending this test to cover one would mean iterating
// m.ThreadRepliers() itself rather than two hardcoded keys.
func TestBuildEnabledWiresThreadCaptureForEveryTransport(t *testing.T) {
	var matrixGotBody bool
	matrixSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		matrixGotBody = true
		_, _ = w.Write([]byte(`{"event_id":"$matrix-root"}`))
	}))
	defer matrixSrv.Close()
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
	}))
	defer slackSrv.Close()

	t.Setenv("TC_MATRIX_TOK", "syt_x")
	t.Setenv("TC_SLACK_BOT", "xoxb-test")

	cfg := &config.Config{}
	cfg.Notify.Matrix = config.MatrixNotify{
		Homeserver:     matrixSrv.URL,
		RoomID:         "!r:example.org",
		AccessTokenEnv: "TC_MATRIX_TOK",
		ThreadCapture:  true,
	}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv:   "TC_SLACK_BOT",
		Channel:       "C1",
		ThreadCapture: true,
	}

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, err := BuildEnabled(Deps{Cfg: cfg, Log: slog.New(slog.DiscardHandler), Threads: reg})
	if err != nil {
		t.Fatalf("BuildEnabled: %v", err)
	}
	repliers := m.ThreadRepliers()

	matrixNotifier, ok := repliers["matrix"].(*Matrix)
	if !ok {
		t.Fatalf("no *Matrix among thread repliers: %#v", repliers)
	}
	if matrixNotifier.Threads == nil {
		t.Fatal("Matrix: BuildEnabled did not wire Threads even though notify.matrix.thread_capture is on — " +
			"a delivered investigation's thread root is never registered, so every later reply misses")
	}

	slackNotifier, ok := repliers["slack"].(*SlackBot)
	if !ok {
		t.Fatalf("no *SlackBot among thread repliers: %#v", repliers)
	}
	if slackNotifier.Threads == nil {
		t.Fatal("Slack: BuildEnabled did not wire Threads even though notify.slack.thread_capture is on")
	}
	// SlackBot's baseURL ("https://slack.com") is not YAML-configurable by
	// design (it's a real endpoint, not a deployment setting) — point this
	// already-built instance at the local test server, the same override
	// slack_test.go uses.
	slackNotifier.baseURL = slackSrv.URL

	// Deliver through both notifiers exactly as built (not hand-constructed)
	// and confirm the registry recorded each root under the CALLER's
	// transport, not a hardcoded one.
	ctx := context.Background()
	if err := matrixNotifier.Deliver(ctx, providers.Investigation{Title: "matrix-inv"}); err != nil {
		t.Fatalf("matrix Deliver: %v", err)
	}
	if !matrixGotBody {
		t.Fatal("matrix homeserver never received the send request")
	}
	if tc, ok := reg.Get("$matrix-root"); !ok {
		t.Fatal("matrix delivery was never registered in the thread registry")
	} else if tc.Transport != "matrix" {
		t.Errorf("matrix delivery registered with Transport = %q, want %q", tc.Transport, "matrix")
	}

	if err := slackNotifier.Deliver(ctx, providers.Investigation{Title: "slack-inv"}); err != nil {
		t.Fatalf("slack Deliver: %v", err)
	}
	if tc, ok := reg.Get("111.222"); !ok {
		t.Fatal("slack delivery was never registered in the thread registry")
	} else if tc.Transport != "slack" {
		t.Errorf("slack delivery registered with Transport = %q, want %q", tc.Transport, "slack")
	}
}
