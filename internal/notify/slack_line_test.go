// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

var (
	_ providers.LineNotifier = (*Slack)(nil)
	_ providers.LineNotifier = (*SlackBot)(nil)
	_ providers.LineNotifier = (*Multi)(nil)
)

func TestDeliverLine(t *testing.T) {
	var hook, bot []string
	hookSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hook = append(hook, string(b))
	}))
	defer hookSrv.Close()
	botSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bot = append(bot, string(b))
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1"}`))
	}))
	defer botSrv.Close()
	sl := NewSlack(hookSrv.URL)
	sb := NewSlackBot("xoxb-t", "C1")
	sb.baseURL = botSrv.URL

	const line = "Knowledge queue, as of 2026-11-02 09:00 UTC: 3 to decide, 3 merged. https://github.com/o/r/issues/7"
	for name, n := range map[string]providers.LineNotifier{"webhook": sl, "bot": sb, "multi": NewMulti(slog.Default(), sl, sb)} {
		hook, bot = nil, nil
		if err := n.DeliverLine(context.Background(), line); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(hook)+len(bot) == 0 {
			t.Fatalf("%s: nothing posted", name)
		}
		for _, body := range append(hook, bot...) {
			var m map[string]any
			if err := json.Unmarshal([]byte(body), &m); err != nil || m["text"] != line {
				t.Fatalf("%s: payload %q must carry the line as text", name, body)
			}
		}
	}
}

type lineSink struct{ err error }

func (l lineSink) Deliver(context.Context, providers.Investigation) error { return nil }
func (l lineSink) DeliverLine(context.Context, string) error              { return l.err }

func TestMultiDeliverLineReportsFailureAndSkipsPlainSinks(t *testing.T) {
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), &captureProgress{}, lineSink{err: errors.New("boom")})
	err := m.DeliverLine(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a failing line sink is the caller's to know: %v", err)
	}
	if err := NewMulti(slog.Default(), &captureProgress{}).DeliverLine(context.Background(), "x"); err != nil {
		t.Fatalf("no line-capable sink is not an error: %v", err)
	}
}
