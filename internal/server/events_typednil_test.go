// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/thread"
)

// lockedBuffer is a slog sink that is safe to read while a detached handler
// goroutine is still writing to it. Without it the assertions below race the
// dispatcher's panic-recovery logger under -race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestEventsTreatsATypedNilThreadHandlerAsDisabled pins the fix for a typed-nil
// interface bug that made /slack/events lie about its own state and then panic.
//
// Actions.Threads is an INTERFACE field (ThreadHandler). app.BuildThreadMention
// returns a CONCRETE *thread.Mention and returns nil for three separate
// misconfigurations — no forge configured, no bot-token delivery target
// resolved, no thread-capable notifier resolved. A nil *thread.Mention stored in
// an interface is a NON-NIL interface value, so handleSlackEvent's
// `if s.threads == nil` guard never fired in any of those three states. Two
// things went wrong at once:
//
//  1. The endpoint answered 401 instead of 404 for an unsigned probe, in a state
//     where capture cannot possibly work. An operator's pre-flight check reads
//     that as "enabled, I just signed it wrong" — the opposite of the truth.
//  2. A REAL signed app_mention got past the guard, was acked 200, and then
//     dereferenced the nil receiver inside Mention.HandleMention. The dispatcher
//     recovered the panic, so the process survived and the human's note was
//     silently lost with nothing but an ERROR line to show for it.
//
// The guard is a safety guard, so it must not depend on every caller in every
// future package remembering to nil-check before assigning. New normalises a
// typed-nil handler to a true nil interface, which is what this test pins.
func TestEventsTreatsATypedNilThreadHandlerAsDisabled(t *testing.T) {
	// Exactly what app.BuildThreadMention hands RunServe on each of its three
	// failure paths: a nil pointer with a concrete type attached.
	var unbuilt *thread.Mention

	newServer := func(sink *lockedBuffer) *Server {
		return New(nil, Actions{SlackSecret: testSigningSecret, Threads: unbuilt}, nil, nil, nil, nil,
			slog.New(slog.NewTextHandler(sink, nil)))
	}

	t.Run("unsigned probe reports not-enabled, not unauthorized", func(t *testing.T) {
		sink := &lockedBuffer{}
		s := newServer(sink)

		req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(`{"type":"url_verification"}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 — thread capture is not wired, so the endpoint must say so; "+
				"401 tells an operator the feature is live and only the signature was wrong", rec.Code)
		}
	})

	t.Run("a real signed mention is refused, never dispatched", func(t *testing.T) {
		sink := &lockedBuffer{}
		s := newServer(sink)

		body := `{"type":"event_callback","event_id":"Ev1","event":{"type":"app_mention","user":"U1","text":"<@U0BOT> note: the disk filled because of the log rotation job","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(testSigningSecret))
		_, _ = mac.Write([]byte("v0:" + ts + ":" + body))
		req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 — a 200 ack promises Slack the note was taken, "+
				"and nothing here can take it", rec.Code)
		}

		// The panic is recovered by Dispatcher.Go, so the only evidence the note
		// was swallowed is this log line. Give the detached goroutine a moment to
		// run: asserting immediately would pass for the wrong reason.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if strings.Contains(sink.String(), "recovered from panic in detached work") {
				t.Fatal("the mention was dispatched to a nil *thread.Mention and panicked — " +
					"the human's note is lost and only an ERROR line records it")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}
