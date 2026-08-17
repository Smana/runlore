// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/thread"
)

// TestNewNormalisesTypedNilActions pins the fix for a typed-nil interface bug
// that made an endpoint lie about its own state and then panic.
//
// Every optional field of Actions is an INTERFACE, but the app builders that
// fill them return CONCRETE POINTERS and return nil to mean "not configured":
// BuildThreadMention returns *thread.Mention (nil for three separate
// misconfigurations — no forge, no bot-token delivery target, no thread-capable
// notifier), BuildAuto returns *action.Auto (nil unless action mode is "auto"),
// and the feedback recorder is an *outcome.Ledger. A nil concrete pointer stored
// in an interface is a NON-NIL interface value, so the `== nil` tests that gate
// these endpoints never fired.
//
// Threads is the field where that actually shipped, and it cost two things at
// once: /slack/events answered 401 rather than 404 to an operator's pre-flight
// probe — reading as "live, you just signed it wrong", the opposite of the truth
// — and a REAL signed app_mention got past the guard, was acked 200, and then
// dereferenced the nil receiver inside Mention.HandleMention. The dispatcher
// recovered the panic, so the process survived and the human's note was silently
// lost. Pauser and Feedback were one careless edit from the same outcome: both
// are assigned from nil-returning concrete builders and are safe today only
// because RunServe happens to nil-check them at the call site.
//
// Each case uses the REAL concrete type production assigns, not a stub, so the
// test keeps testing the shipped shape rather than one invented here.
func TestNewNormalisesTypedNilActions(t *testing.T) {
	// Exactly what the app builders hand RunServe on their failure paths: a nil
	// pointer with a concrete type attached.
	var (
		unbuiltThreads *thread.Mention
		unbuiltPauser  *action.Auto
		unbuiltLedger  *outcome.Ledger
	)

	for _, tc := range []struct {
		name   string
		acts   Actions
		method string
		path   string
		why    string
	}{
		{
			name:   "Threads: an unsigned probe reports not-enabled, not unauthorized",
			acts:   Actions{SlackSecret: testSigningSecret, Threads: unbuiltThreads},
			method: http.MethodPost,
			path:   "/slack/events",
			why: "thread capture is not wired, so the endpoint must say so; 401 tells an operator " +
				"the feature is live and only the signature was wrong",
		},
		{
			name:   "Pauser: the kill-switch reports not-enabled",
			acts:   Actions{Token: "t", Pauser: unbuiltPauser},
			method: http.MethodPost,
			path:   "/actions/pause",
			why:    "auto-execution is not configured, so the kill-switch must not present itself as usable",
		},
		{
			name:   "Pauser: resume reports not-enabled",
			acts:   Actions{Token: "t", Pauser: unbuiltPauser},
			method: http.MethodPost,
			path:   "/actions/resume",
			why:    "auto-execution is not configured, so the kill-switch must not present itself as usable",
		},
		{
			name:   "Feedback: interactions stay closed when nothing is wired",
			acts:   Actions{SlackSecret: testSigningSecret, Feedback: unbuiltLedger},
			method: http.MethodPost,
			path:   "/slack/interactions",
			why: "neither approvals nor feedback is wired, so no interactive callback should be " +
				"exposed at all",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(nil, tc.acts, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s = %d, want 404 — %s", tc.method, tc.path, rec.Code, tc.why)
			}
		})
	}

	// The half that cost a human their note: a correctly signed mention must be
	// refused outright rather than acked 200 and dropped into a nil receiver. A
	// 404 here IS the proof nothing was dispatched — handleSlackEvent returns
	// before it ever reaches the dispatcher.
	t.Run("Threads: a real signed mention is refused, never dispatched", func(t *testing.T) {
		s := New(nil, Actions{SlackSecret: testSigningSecret, Threads: unbuiltThreads},
			nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
	})
}
