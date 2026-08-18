// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// recordSink collects Feedback calls and signals doneAt when the expected
// number has landed, so tests wait on real progress instead of sleeping.
type recordSink struct {
	mu     sync.Mutex
	got    []string // "key/rating/user"
	doneAt int
	done   chan struct{}
}

func (r *recordSink) Feedback(key, rating, user string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, key+"/"+rating+"/"+user)
	if len(r.got) == r.doneAt {
		close(r.done)
	}
	return nil
}

// reactionJSON builds one m.reaction timeline event.
func reactionJSON(sender, target, key string) string {
	return fmt.Sprintf(`{"type":"m.reaction","sender":%q,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":%q,"key":%q}}}`, sender, target, key)
}

// TestMatrixFeedbackRun scripts a homeserver: the first /sync is a position
// handshake whose (historical) events must be SKIPPED; the second batch carries
// a 👍 (variation selector included), a 👎, a foreign emoji, and a reaction to a
// message without the trigger field — only the two votes reach the sink, with
// the Matrix user ids as identities. ctx cancellation stops Run.
//
// Constructs the listener WithFeedbackReactions: Fix 4 made reaction
// recording its own opt-in (mirroring WithThreadCapture) rather than the
// zero-options default, so this pre-existing test needs that option to keep
// exercising what it always meant to.
func TestMatrixFeedbackRun(t *testing.T) {
	const room = "!r:hs"
	sink := &recordSink{doneAt: 2, done: make(chan struct{})}
	var syncCalls int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@runlore:hs"})
		case strings.HasSuffix(r.URL.Path, "/displayname"):
			// Run's non-fatal display-name lookup: 404 is the spec-accurate
			// "no display name set" response, not exercised further by this test.
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/sync"):
			mu.Lock()
			syncCalls++
			n := syncCalls
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("sync auth = %q", got)
			}
			timeline := ""
			switch n {
			case 1:
				// Handshake response carrying a HISTORICAL reaction that must be skipped.
				if r.URL.Query().Get("since") != "" {
					t.Errorf("first sync must carry no since, got %q", r.URL.Query().Get("since"))
				}
				timeline = reactionJSON("@old:hs", "$msg1", "👍")
			case 2:
				if r.URL.Query().Get("since") != "s1" {
					t.Errorf("second sync since = %q, want s1", r.URL.Query().Get("since"))
				}
				timeline = strings.Join([]string{
					reactionJSON("@alice:hs", "$msg1", "👍️"), // variation selector
					reactionJSON("@bob:hs", "$msg1", "👎"),
					reactionJSON("@carol:hs", "$msg1", "🎉"), // foreign emoji: ignored
					reactionJSON("@dave:hs", "$human", "👍"), // keyless target: ignored
					reactionJSON("@eve:hs", "$spoof", "👍"),  // forged-field target: ignored
				}, ",")
			default:
				// Quiet long-poll: nothing new. (The real server would hold; the test
				// returns immediately — Run just re-polls.)
			}
			_, _ = fmt.Fprintf(w, `{"next_batch":"s%d","rooms":{"join":{%q:{"timeline":{"events":[%s]}}}}}`, n, room, timeline)
		case strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"):
			switch {
			case strings.HasSuffix(r.URL.Path, "/event/$msg1"):
				_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@runlore:hs",
					"content": map[string]any{triggerKeyContentField: "trig-1"}})
			case strings.HasSuffix(r.URL.Path, "/event/$spoof"):
				// The attack the self-check closes: a room member posts their OWN
				// message carrying the trigger field, then votes on it.
				_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@eve:hs",
					"content": map[string]any{triggerKeyContentField: "victim-trigger"}})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@human:hs",
					"content": map[string]any{"body": "a human message"}})
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	f := NewMatrixFeedback(srv.URL, room, "tok", sink, slog.New(slog.NewTextHandler(io.Discard, nil)), WithFeedbackReactions())
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { f.Run(ctx); close(stopped) }()

	select {
	case <-sink.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the two votes")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on ctx cancel")
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	want := []string{"trig-1/up/@alice:hs", "trig-1/down/@bob:hs"}
	if len(sink.got) != 2 || sink.got[0] != want[0] || sink.got[1] != want[1] {
		t.Fatalf("recorded = %v, want %v", sink.got, want)
	}
}

// TestMatrixSyncFilterNarrowsToEnabledCapabilities pins Fix 4's wire-level
// half: the /sync filter requests ONLY the event types the capabilities
// actually enabled on this listener need — m.reaction only with
// WithFeedbackReactions, m.room.message only with WithThreadCapture. A
// listener started for one capability alone must never even ASK the
// homeserver for the other's events. Covers all four flag combinations
// (neither is exercised here at the wire level only — app.BuildMatrixFeedback
// is what guarantees a listener is never actually started with neither on).
func TestMatrixSyncFilterNarrowsToEnabledCapabilities(t *testing.T) {
	const room = "!r:hs"
	log := matrixTestLog()

	tests := []struct {
		name       string
		reactions  bool
		thread     bool
		wantReact  bool
		wantThread bool
	}{
		{name: "neither enabled: filter requests nothing"},
		{name: "reactions only", reactions: true, wantReact: true},
		{name: "thread capture only", thread: true, wantThread: true},
		{name: "both enabled", reactions: true, thread: true, wantReact: true, wantThread: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotFilter string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotFilter = r.URL.Query().Get("filter")
				_, _ = fmt.Fprintf(w, `{"next_batch":"s1","rooms":{}}`)
			}))
			defer srv.Close()

			var opts []MatrixFeedbackOption
			if tc.reactions {
				opts = append(opts, WithFeedbackReactions())
			}
			if tc.thread {
				opts = append(opts, WithThreadCapture(&thread.Mention{}, thread.NewDispatcher(1, time.Minute, log), thread.NewDispatcher(1, time.Minute, log)))
			}
			f := NewMatrixFeedback(srv.URL, room, "tok", nil, log, opts...)
			if _, _, err := f.sync(context.Background(), ""); err != nil {
				t.Fatalf("sync: %v", err)
			}

			if got := strings.Contains(gotFilter, "m.reaction"); got != tc.wantReact {
				t.Errorf("filter carries m.reaction = %v, want %v: %s", got, tc.wantReact, gotFilter)
			}
			if got := strings.Contains(gotFilter, "m.room.message"); got != tc.wantThread {
				t.Errorf("filter carries m.room.message = %v, want %v: %s", got, tc.wantThread, gotFilter)
			}
		})
	}
}

// TestMatrixHandleReactionGatedByFlag pins Fix 4's code-level half: even a
// well-formed m.reaction event must be recorded ONLY when feedbackReactions
// is on — a listener started for thread capture alone (or with neither
// capability, defence in depth beyond the /sync filter narrowing) must never
// route a reaction into the outcome ledger, an opt-in its operator never
// granted and one that feeds the learning loop's trust weighting.
func TestMatrixHandleReactionGatedByFlag(t *testing.T) {
	const room = "!r:hs"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@runlore:hs",
			"content": map[string]any{triggerKeyContentField: "trig-1"}})
	}))
	defer srv.Close()

	reaction := func() matrixEvent {
		e := matrixEvent{Type: "m.reaction", Sender: "@alice:hs"}
		e.Content.RelatesTo.RelType = "m.annotation"
		e.Content.RelatesTo.EventID = "$msg1"
		e.Content.RelatesTo.Key = "👍"
		return e
	}

	tests := []struct {
		name      string
		opts      []MatrixFeedbackOption
		wantCount int
	}{
		{name: "neither capability on: not recorded", wantCount: 0},
		{name: "feedback_reactions only: recorded", opts: []MatrixFeedbackOption{WithFeedbackReactions()}, wantCount: 1},
		{
			name:      "thread_capture only, reactions off: not recorded",
			opts:      []MatrixFeedbackOption{WithThreadCapture(&thread.Mention{}, thread.NewDispatcher(1, time.Minute, matrixTestLog()), thread.NewDispatcher(1, time.Minute, matrixTestLog()))},
			wantCount: 0,
		},
		{
			name: "both capabilities on: recorded",
			opts: []MatrixFeedbackOption{
				WithFeedbackReactions(),
				WithThreadCapture(&thread.Mention{}, thread.NewDispatcher(1, time.Minute, matrixTestLog()), thread.NewDispatcher(1, time.Minute, matrixTestLog())),
			},
			wantCount: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordSink{}
			f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(), tc.opts...)
			f.self = "@runlore:hs"

			f.handleReaction(context.Background(), reaction())

			sink.mu.Lock()
			got := len(sink.got)
			sink.mu.Unlock()
			if got != tc.wantCount {
				t.Errorf("recorded %d reactions, want %d", got, tc.wantCount)
			}
		})
	}
}

// TestMatrixFeedbackSyncErrorRetries: a failing homeserver is logged and
// retried, never fatal — Run keeps polling and records once the server heals.
// Constructs the listener WithFeedbackReactions — see TestMatrixFeedbackRun's
// doc for why this pre-existing test needed that option after Fix 4.
func TestMatrixFeedbackSyncErrorRetries(t *testing.T) {
	const room = "!r:hs"
	sink := &recordSink{doneAt: 1, done: make(chan struct{})}
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_matrix/client/v3/account/whoami" {
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@runlore:hs"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/displayname") {
			// Run's non-fatal display-name lookup: 404 is the spec-accurate "no
			// display name set" response, kept out of the `calls` counter below,
			// which counts /sync attempts specifically.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@runlore:hs",
				"content": map[string]any{triggerKeyContentField: "k"}})
			return
		}
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			w.WriteHeader(http.StatusBadGateway) // transient failure
		case 2:
			_, _ = fmt.Fprintf(w, `{"next_batch":"s1","rooms":{}}`) // handshake
		default:
			_, _ = fmt.Fprintf(w, `{"next_batch":"s%d","rooms":{"join":{%q:{"timeline":{"events":[%s]}}}}}`,
				n, room, reactionJSON("@a:hs", "$m", "👍"))
		}
	}))
	defer srv.Close()

	f := NewMatrixFeedback(srv.URL, room, "tok", sink, slog.New(slog.NewTextHandler(io.Discard, nil)), WithFeedbackReactions())
	// Shrink the retry pause for the test via a tiny wrapper: not configurable by
	// design (operators shouldn't tune it), so just tolerate the one 5s backoff.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go f.Run(ctx)
	select {
	case <-sink.done:
	case <-ctx.Done():
		t.Fatal("timed out waiting for recovery after a transient sync failure")
	}
}

// TestMatrixFetchDisplayName pins fetchDisplayName's three outcomes: a
// resolved name, the spec-accurate 404 "no display name set" (a valid answer,
// not an error — see the function's own doc), and a genuine homeserver
// failure. Run's own non-fatal wiring around this call is exercised
// end-to-end by TestMatrixFeedbackRun and TestMatrixFeedbackSyncErrorRetries,
// both of which serve 404 here and still complete their /sync work
// unaffected.
func TestMatrixFetchDisplayName(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantName   string
		wantErr    bool
		wantErrStr string
	}{
		{name: "resolved", status: http.StatusOK, body: `{"displayname":"Ops Bot"}`, wantName: "Ops Bot"},
		{name: "404: no display name set is a valid answer, not an error", status: http.StatusNotFound, wantName: ""},
		{name: "homeserver failure", status: http.StatusInternalServerError, wantErr: true, wantErrStr: "matrix displayname status 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Path; got != "/_matrix/client/v3/profile/@runlore:hs/displayname" {
					t.Errorf("unexpected path %s", got)
				}
				w.WriteHeader(tc.status)
				if tc.body != "" {
					_, _ = w.Write([]byte(tc.body))
				}
			}))
			defer srv.Close()

			f := NewMatrixFeedback(srv.URL, "!r:hs", "tok", nil, matrixTestLog())
			got, err := f.fetchDisplayName(context.Background(), "@runlore:hs")
			if tc.wantErr {
				if err == nil {
					t.Fatal("fetchDisplayName: want an error, got nil")
				}
				if !strings.Contains(err.Error(), tc.wantErrStr) {
					t.Errorf("err = %q, want it to contain %q", err.Error(), tc.wantErrStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("fetchDisplayName: %v", err)
			}
			if got != tc.wantName {
				t.Errorf("displayName = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// TestMatrixContextFor pins contextFor against the four cases that matter: a
// stamped event sent by the bot, the identical stamp sent by somebody else
// (the trust anchor: it must attribute nothing), a legacy trigger-key-only
// event, and an unstamped event. triggerKeyFor is exercised against the same
// fixtures to prove it is unchanged — same fetch, same self-check, same
// cache, now shared through contextFor.
func TestMatrixContextFor(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"

	responses := map[string]string{
		"$evt1": fmt.Sprintf(`{"sender":%q,"content":{%q:{"trigger_key":"tk-1","title":"OOMKilled","resource":"prod/api","verdict":"action_required","curated_url":"https://x/pr/1","dup_fingerprint":"fp1","recalled_entry":"catalog/oom.md"}}}`,
			self, threadContentField),
		"$evt2": fmt.Sprintf(`{"sender":"@eve:hs","content":{%q:{"trigger_key":"victim-trigger"}}}`, threadContentField),
		"$evt3": fmt.Sprintf(`{"sender":%q,"content":{%q:"tk-legacy"}}`, self, triggerKeyContentField),
		"$evt4": fmt.Sprintf(`{"sender":%q,"content":{"body":"a human reply, no stamp"}}`, self),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for id, body := range responses {
			if strings.HasSuffix(r.URL.Path, "/event/"+id) {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		t.Fatalf("unexpected event fetch: %s", r.URL.Path)
	}))
	defer srv.Close()

	tests := []struct {
		name        string
		eventID     string
		wantCtx     thread.Context
		wantOK      bool
		wantTrigKey string
	}{
		{
			name:    "stamped and sent by the bot",
			eventID: "$evt1",
			wantCtx: thread.Context{
				Transport: "matrix", Root: "$evt1", Channel: room,
				TriggerKey: "tk-1", DupFingerprint: "fp1", Title: "OOMKilled",
				Resource: "prod/api", Verdict: providers.VerdictActionRequired,
				CuratedURL: "https://x/pr/1", RecalledEntry: "catalog/oom.md",
			},
			wantOK:      true,
			wantTrigKey: "tk-1",
		},
		{
			name:        "stamped but sent by somebody else must attribute nothing",
			eventID:     "$evt2",
			wantCtx:     thread.Context{},
			wantOK:      false,
			wantTrigKey: "",
		},
		{
			name:    "legacy trigger-key-only event",
			eventID: "$evt3",
			wantCtx: thread.Context{
				Transport: "matrix", Root: "$evt3", Channel: room, TriggerKey: "tk-legacy",
			},
			wantOK:      true,
			wantTrigKey: "tk-legacy",
		},
		{
			name:        "unstamped event",
			eventID:     "$evt4",
			wantCtx:     thread.Context{},
			wantOK:      false,
			wantTrigKey: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewMatrixFeedback(srv.URL, room, "tok", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			f.self = self

			gotCtx, gotOK, err := f.contextFor(context.Background(), tc.eventID)
			if err != nil {
				t.Fatalf("contextFor: %v", err)
			}
			if gotOK != tc.wantOK {
				t.Errorf("contextFor ok = %v, want %v", gotOK, tc.wantOK)
			}
			if !reflect.DeepEqual(gotCtx, tc.wantCtx) {
				t.Errorf("contextFor = %+v, want %+v", gotCtx, tc.wantCtx)
			}

			// A fresh instance (fresh cache) so this exercises the fetch path too,
			// not a cache hit left over from the contextFor call above.
			g := NewMatrixFeedback(srv.URL, room, "tok", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
			g.self = self
			gotKey, err := g.triggerKeyFor(context.Background(), tc.eventID)
			if err != nil {
				t.Fatalf("triggerKeyFor: %v", err)
			}
			if gotKey != tc.wantTrigKey {
				t.Errorf("triggerKeyFor = %q, want %q", gotKey, tc.wantTrigKey)
			}
		})
	}
}

// fakeMentionReplier is thread.Replier: it records every reply so
// handleMessage's dispatch can be observed without a live forge or a real
// Matrix homeserver reply endpoint. doneAt/done let a test wait for the
// detached work to finish deterministically, never by sleeping.
type fakeMentionReplier struct {
	mu     sync.Mutex
	calls  []mentionReply
	doneAt int
	done   chan struct{}
}

// mentionReply is one recorded ReplyInThread call.
type mentionReply struct{ root, channel, text string }

func (f *fakeMentionReplier) ReplyInThread(_ context.Context, root, channel, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, mentionReply{root: root, channel: channel, text: text})
	if f.done != nil && len(f.calls) == f.doneAt {
		close(f.done)
	}
	return nil
}

func (f *fakeMentionReplier) snapshot() []mentionReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mentionReply(nil), f.calls...)
}

// matrixThreadCaptureServer serves the two root events handleMessage's
// contextFor gate needs: $root-ours (stamped, sent by self — one of RunLore's
// own investigation messages) and $root-foreign (identically stamped, but sent
// by somebody else — the trust anchor contextFor must reject). Any other path
// fails the test loudly rather than hanging, matching TestMatrixContextFor's
// convention.
func matrixThreadCaptureServer(t *testing.T, self string) *httptest.Server {
	t.Helper()
	responses := map[string]string{
		"$root-ours":    fmt.Sprintf(`{"sender":%q,"content":{%q:"trig-ours"}}`, self, triggerKeyContentField),
		"$root-foreign": fmt.Sprintf(`{"sender":"@eve:hs","content":{%q:"trig-foreign"}}`, triggerKeyContentField),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for id, body := range responses {
			if strings.HasSuffix(r.URL.Path, "/event/"+id) {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		t.Fatalf("unexpected event fetch: %s", r.URL.Path)
	}))
}

// newTestMatrixHandler builds a MatrixFeedback wired for handleMessage tests:
// self is set directly (Run normally resolves it via /whoami). Mentions'
// Registry is nil (disabled) — every dispatched mention therefore resolves
// its thread.Context from handleMessage's event-stamp fallback (Fix 3), the
// same "registry miss" path a leader failover or a TTL expiry hits in
// production. The Responder's Forge is a no-op-succeeding fake purely so that
// fallback write completes without erroring; these tests still assert only
// routing (root/channel), not what the Forge records — that stays thread
// package's own Responder/Forge coverage. busyDispatch defaults to a roomy
// standalone dispatcher when nil, so callers that don't care about
// busy-notice behaviour don't have to construct one.
func newTestMatrixHandler(t *testing.T, srv *httptest.Server, room, self string, dispatch, busyDispatch *thread.Dispatcher, rep *fakeMentionReplier) *MatrixFeedback {
	t.Helper()
	log := matrixTestLog()
	if busyDispatch == nil {
		busyDispatch = thread.NewDispatcher(4, time.Minute, log)
	}
	responder := &thread.Responder{Forge: &fakeThreadForge{}, Log: log}
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, log, WithThreadCapture(&thread.Mention{Responder: responder, Replier: rep, Log: log}, dispatch, busyDispatch))
	f.self = self
	return f
}

// TestMatrixHandleMessage pins handleMessage's routing: which addressed,
// threaded messages get dispatched to the mention handler (with the thread
// ROOT, never the reply's own event id), and which are dropped before ever
// reaching Dispatch. Bodies use a realistic bare-MXID Matrix shape
// ("@runlore:hs note: …"), not Slack's "<@U…>" encoding — a real Matrix
// client never sends the latter, and stripSelfMention is what makes this
// shape parse at all (see matrix_grammar_test.go for the grammar coverage
// itself; these assertions only check routing, not intent classification).
func TestMatrixHandleMessage(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	t.Run("threaded message mentioning the bot dispatches with the root event id", func(t *testing.T) {
		rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
		f := newTestMatrixHandler(t, srv, room, self, thread.NewDispatcher(4, time.Minute, matrixTestLog()), nil, rep)
		e := matrixEvent{Sender: "@alice:hs", EventID: "$reply1"}
		e.Content.Body = "@runlore:hs note: it was a bad deploy"
		e.Content.Mentions.UserIDs = []string{self}
		e.Content.RelatesTo.RelType = "m.thread"
		e.Content.RelatesTo.EventID = "$root-ours"

		f.handleMessage(context.Background(), e)
		waitForReplies(t, rep)

		if got := rep.snapshot(); len(got) != 1 || got[0].root != "$root-ours" || got[0].channel != room {
			t.Fatalf("replies = %+v, want one reply rooted at $root-ours/%s", got, room)
		}
	})

	t.Run("reply-fallback message resolves its root the same way", func(t *testing.T) {
		rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
		f := newTestMatrixHandler(t, srv, room, self, thread.NewDispatcher(4, time.Minute, matrixTestLog()), nil, rep)
		e := matrixEvent{Sender: "@alice:hs", EventID: "$reply2"}
		// No m.mentions: addressed via the body containing the bot's localpart —
		// the fallback for clients that don't send MSC3952 mentions.
		e.Content.Body = "runlore note: it was a bad deploy"
		e.Content.RelatesTo.InReplyTo.EventID = "$root-ours"

		f.handleMessage(context.Background(), e)
		waitForReplies(t, rep)

		if got := rep.snapshot(); len(got) != 1 || got[0].root != "$root-ours" {
			t.Fatalf("replies = %+v, want one reply rooted at $root-ours", got)
		}
	})

	notDispatched := []struct {
		name string
		e    func() matrixEvent
	}{
		{
			name: "message not mentioning the bot dispatches nothing",
			e: func() matrixEvent {
				e := matrixEvent{Sender: "@alice:hs", EventID: "$reply3"}
				e.Content.Body = "just chatting, no mention here"
				e.Content.RelatesTo.RelType = "m.thread"
				e.Content.RelatesTo.EventID = "$root-ours"
				return e
			},
		},
		{
			name: "message from the bot itself dispatches nothing",
			e: func() matrixEvent {
				e := matrixEvent{Sender: self, EventID: "$reply4"}
				e.Content.Body = "@runlore:hs note: talking to myself"
				e.Content.Mentions.UserIDs = []string{self}
				e.Content.RelatesTo.RelType = "m.thread"
				e.Content.RelatesTo.EventID = "$root-ours"
				return e
			},
		},
		{
			name: "message with no thread relation dispatches nothing",
			e: func() matrixEvent {
				e := matrixEvent{Sender: "@alice:hs", EventID: "$reply5"}
				e.Content.Body = "@runlore:hs note: floating message"
				e.Content.Mentions.UserIDs = []string{self}
				return e
			},
		},
		{
			name: "message whose root is not one of RunLore's dispatches nothing",
			e: func() matrixEvent {
				e := matrixEvent{Sender: "@alice:hs", EventID: "$reply6"}
				e.Content.Body = "@runlore:hs note: not really ours"
				e.Content.Mentions.UserIDs = []string{self}
				e.Content.RelatesTo.RelType = "m.thread"
				e.Content.RelatesTo.EventID = "$root-foreign"
				return e
			},
		},
	}
	for _, tc := range notDispatched {
		t.Run(tc.name, func(t *testing.T) {
			rep := &fakeMentionReplier{}
			d := thread.NewDispatcher(4, time.Minute, matrixTestLog())
			f := newTestMatrixHandler(t, srv, room, self, d, nil, rep)

			f.handleMessage(context.Background(), tc.e())
			// Drain blocks until any in-flight dispatched work finishes; with
			// nothing dispatched it returns immediately — deterministic either
			// way, never a sleep.
			d.Drain(context.Background())

			if got := rep.snapshot(); len(got) != 0 {
				t.Fatalf("replies = %+v, want none dispatched", got)
			}
		})
	}
}

// TestMatrixHandleMessageRegistryMissFallsBackToEventStamp is the end-to-end
// regression test for Fix 3, at the Matrix transport level: with the thread
// registry enabled but missing this specific root — standing in for a miss
// (TTL expiry, the 2000-entry size cap, or a leader failover onto a replica
// whose registry JSONL had not yet caught up; see
// internal/app.BuildThreadRegistry) — a note in a thread rooted at one of
// RunLore's own investigation messages must still be recorded, using the
// context handleMessage already resolved off the root event's own stamp
// (contextFor), rather than refused with "I don't have context for this
// thread" even though the event carries everything needed to answer it.
//
// The registry is deliberately ENABLED (a real, empty backing file), not
// disabled: since thread.Mention.HandleMention now rehydrates a registry miss
// via Registry.GetOrCreate (see internal/thread/mention.go), a genuinely
// DISABLED registry (empty path) hits ErrThreadNotEstablishable and refuses
// by design — that is pinned separately by
// TestMentionFallbackOnDisabledRegistryDoesNotWriteWithZeroValueContext in
// internal/thread. This test's whole premise is the miss-on-an-otherwise-live-
// registry path, so it must exercise that path, not the disabled one.
func TestMatrixHandleMessageRegistryMissFallsBackToEventStamp(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10) // enabled, but no entry for this root: a genuine miss
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	forge := &fakeThreadForge{}
	responder := &thread.Responder{Forge: forge, Registry: reg, Log: matrixTestLog()}
	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	mention := &thread.Mention{Responder: responder, Registry: reg, Replier: rep, Log: matrixTestLog()}

	dispatch := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	busy := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog(), WithThreadCapture(mention, dispatch, busy))
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply-fallback"}
	e.Content.Body = "@runlore:hs note: the real cause was a spot-node reclaim"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	f.handleMessage(context.Background(), e)
	waitForReplies(t, rep)

	got := rep.snapshot()
	if len(got) != 1 {
		t.Fatalf("replies = %+v, want exactly one", got)
	}
	if strings.Contains(strings.ToLower(got[0].text), "don't have context") {
		t.Fatalf("reply = %q, want the note recorded via the event-stamp fallback, not refused", got[0].text)
	}
	if opened, _ := forge.counts(); opened != 1 {
		t.Fatalf("forge.opened = %d, want 1 — the note must still land in the knowledge base via the fallback context", opened)
	}
}

// fakeChatModel is a scriptable providers.ModelProvider standing in for the
// chat layer's one model call, counting invocations and keeping the request so
// a test can assert BOTH that the model was reached exactly once and that the
// thread's own context reached it. Guarded by a mutex because Complete runs on
// a Dispatch worker goroutine while the test reads these fields on its own.
type fakeChatModel struct {
	mu      sync.Mutex
	resp    providers.CompletionResponse
	calls   int
	lastReq providers.CompletionRequest
}

func (f *fakeChatModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	return f.resp, nil
}

func (f *fakeChatModel) stats() (calls int, req providers.CompletionRequest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastReq
}

// chatToolReply is the well-formed shape thread.Chat expects back: exactly one
// forced submit_thread_reply tool call carrying the answer and the note the
// model proposes. The tool name is internal/thread's own constant, unexported
// there, so it is spelled out here — a drift between the two makes Chat.Answer
// report failure and the deterministic capture path answer instead, which this
// test's assertions on the posted text catch rather than wave through.
func chatToolReply(reply, kbNote string) providers.CompletionResponse {
	return providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{
			ID:   "1",
			Name: "submit_thread_reply",
			Args: fmt.Sprintf(`{"reply":%q,"kb_note":%q}`, reply, kbNote),
		}},
		Usage: providers.Usage{InputTokens: 100, OutputTokens: 20},
	}
}

// TestMatrixFeedbackDrivesTheChatLayerEndToEnd is Matrix's half of the wiring
// test internal/thread.TestMentionDrivesTheChatLayerEndToEnd is Slack's:
// nothing drove the MATRIX entry point with a chat model configured, so nothing
// proved the model's answer is actually POSTED — into the right room, in the
// right thread — on this transport. Every other Matrix thread-capture test runs
// with Chat nil and stops at routing; every chat test lives in internal/thread
// and never touches handleMessage. Between them, a chat layer that answered
// perfectly and a Matrix path that dropped the answer would both look correct,
// which is exactly the class that shipped a non-functional feature in PR2.
//
// It is driven through the real handleMessage → Dispatch →
// thread.Mention.HandleMention → Responder → Chat path, off a real m.room.message
// event rooted in one of RunLore's own investigation messages, with a real
// (registry-hit) thread context.
//
// The proposed note's own text names a DIFFERENT pull request from the one the
// thread is anchored to. That is the point of the third assertion: Chat supplies
// note CONTENT only, and the route stays derived from the thread context alone
// (see Responder.write), so a note mentioning PR #999 must still land as a
// comment on the thread's PR #42 — never wherever the model's words pointed.
func TestMatrixFeedbackDrivesTheChatLayerEndToEnd(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Put(thread.Context{
		Root: "$root-ours", Channel: room, Title: "pod crash-looping",
		CuratedURL: "https://github.com/o/r/pull/42",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	model := &fakeChatModel{resp: chatToolReply(
		"The CNI was ruled out — it was a spot reclaim.",
		"The real cause was a spot-node reclaim, not the CNI. See https://github.com/o/r/pull/999.",
	)}
	forge := &fakeThreadForge{}
	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	responder := &thread.Responder{
		Forge:    forge,
		Registry: reg,
		Chat:     &thread.Chat{Model: model, Log: matrixTestLog()},
		Log:      matrixTestLog(),
	}
	mention := &thread.Mention{Responder: responder, Registry: reg, Replier: rep, Log: matrixTestLog()}
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog(), WithThreadCapture(
		mention,
		thread.NewDispatcher(4, time.Minute, matrixTestLog()),
		thread.NewDispatcher(4, time.Minute, matrixTestLog()),
	))
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply-chat"}
	e.Content.Body = "@runlore:hs was it the CNI?"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	f.handleMessage(context.Background(), e)
	waitForReplies(t, rep)

	calls, req := model.stats()
	if calls != 1 {
		t.Fatalf("Complete called %d times, want exactly 1 — an addressed freeform message with model.chat configured must reach the model exactly once", calls)
	}
	// The model answered from the thread's own context rather than from nothing:
	// a wiring that reached the model with an empty context would still produce a
	// reply, and would still be broken.
	prompt := req.Messages[0].Content
	for _, want := range []string{"pod crash-looping", "was it the CNI?", "@alice:hs"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the assembled prompt is missing %q — the thread's context never reached the model:\n%s", want, prompt)
		}
	}

	got := rep.snapshot()
	if len(got) != 1 {
		t.Fatalf("replies = %+v, want exactly one", got)
	}
	if got[0].root != "$root-ours" || got[0].channel != room {
		t.Fatalf("reply posted to root %q in room %q, want root %q in room %q — the answer must go back where it was asked",
			got[0].root, got[0].channel, "$root-ours", room)
	}
	// Read as the human sees it: the untrusted-span marks are the transport's
	// business (see thread.RenderReply), not this contract's.
	posted := thread.RenderReply(got[0].text, nil)
	if !strings.Contains(posted, "> The CNI was ruled out — it was a spot reclaim.") {
		t.Fatalf("the model's answer never reached the room: %q", posted)
	}
	if !strings.Contains(posted, "📝 Noted on the knowledge-base PR #42") {
		t.Fatalf("the human was not told where their note landed: %q", posted)
	}

	comments := forge.commentsSnapshot()
	if opened, _ := forge.counts(); opened != 0 || len(comments) != 1 || comments[0].number != 42 {
		t.Fatalf("forge writes = %d opened / %+v commented, want exactly one comment on #42 — the note is routed by the thread's context, never by the model's text",
			opened, comments)
	}
	if !strings.Contains(comments[0].body, "spot-node reclaim, not the CNI") {
		t.Fatalf("the filed note lost the model's own text:\n%s", comments[0].body)
	}
	// A CURATED PR: someone else's draft, under review. A note on it is feedback
	// a human reconciles at merge time, never a rewrite of their entry file —
	// see thread.noteTarget for why the two destinations are not interchangeable,
	// and why only the PR thread capture itself opened gets the append route.
	if n := forge.appends(); n != 0 {
		t.Fatalf("appends = %d, want 0 — RunLore must never rewrite an entry a human drafted", n)
	}
	stored, ok := reg.Get("$root-ours")
	if !ok {
		t.Fatal("the thread went missing from the registry")
	}
	if stored.Notes != 1 {
		t.Fatalf("Notes = %d, want 1 — a note filed through the chat route spends the same per-thread allowance", stored.Notes)
	}
}

// TestMatrixAddressedLocalpartBoundary pins Fix 3: the localpart fallback
// (the "runlore" in "@runlore:example.org") must only match as a whole word —
// a plain strings.Contains treats "sre" as addressing RunLore inside
// "misread", or "ops" inside "oops". This matters beyond cosmetics: a false
// positive here still hands the message to thread.Responder.Handle, which
// cannot tell an accidental match from a deliberate one once addressed() has
// said yes. Ordinary prose carrying "note:" anywhere in it — never intended
// for RunLore at all — is then recorded to the knowledge base exactly as if
// it had been genuinely addressed, and prose without it spends a model call
// instead.
func TestMatrixAddressedLocalpartBoundary(t *testing.T) {
	tests := []struct {
		name string
		self string
		e    matrixEvent
		want bool
	}{
		{
			name: "exact mention via m.mentions.user_ids (primary path, unchanged)",
			self: "@runlore:hs",
			e: func() matrixEvent {
				e := matrixEvent{}
				e.Content.Body = "unrelated text"
				e.Content.Mentions.UserIDs = []string{"@runlore:hs"}
				return e
			}(),
			want: true,
		},
		{
			name: "localpart as a whole word",
			self: "@sre:hs",
			e: func() matrixEvent {
				e := matrixEvent{}
				e.Content.Body = "hey sre, can you look at this"
				return e
			}(),
			want: true,
		},
		{
			name: "localpart embedded in a larger word must NOT match",
			self: "@sre:hs",
			e: func() matrixEvent {
				e := matrixEvent{}
				e.Content.Body = "this alert was misread by the pager"
				return e
			}(),
			want: false,
		},
		{
			name: "localpart at the very start of the body",
			self: "@sre:hs",
			e: func() matrixEvent {
				e := matrixEvent{}
				e.Content.Body = "sre please take a look"
				return e
			}(),
			want: true,
		},
		{
			name: "localpart at the very end of the body",
			self: "@sre:hs",
			e: func() matrixEvent {
				e := matrixEvent{}
				e.Content.Body = "please take a look sre"
				return e
			}(),
			want: true,
		},
		{
			name: "empty localpart never matches",
			self: "@:hs", // selfLocalpart("@:hs") == ""
			e: func() matrixEvent {
				e := matrixEvent{}
				e.Content.Body = "hs is mentioned here but there is no localpart to match"
				return e
			}(),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &MatrixFeedback{self: tc.self}
			if got := f.addressed(tc.e); got != tc.want {
				t.Errorf("addressed(%+v) = %v, want %v", tc.e.Content.Body, got, tc.want)
			}
		})
	}
}

// TestMatrixAddressedFullMXIDBoundary is the companion of
// TestMatrixAddressedLocalpartBoundary above: Fix 3 added the word-boundary
// rule to the localpart fallback but left the full-MXID fallback
// (e.Content.Body containing f.self verbatim) on a plain strings.Contains, so
// "@runlore:hs" still matched embedded inside a longer token like
// "x@runlore:hs" or "@runlore:hs2" — the exact false-positive class Fix 3 was
// meant to close, just on the other fallback path.
//
// self is deliberately "@:hs" — an EMPTY localpart — throughout this table,
// not a normal id like "@runlore:hs". With a real localpart, self's own
// literal text always embeds "@localpart:" as a substring, and localpart's
// immediate neighbours ('@' before, ':' after) are non-alphanumeric BY
// CONSTRUCTION — so the (already boundary-checked, per Fix 3) localpart
// fallback would independently return true for every case below regardless
// of whether the full-MXID path is fixed, masking exactly the bug this test
// exists to catch. An empty localpart makes `lp != "" && …` short-circuit
// false, isolating the full-MXID fallback as the only path that can return
// true — the same isolation trick TestMatrixAddressedLocalpartBoundary's own
// "empty localpart never matches" case relies on, in reverse.
func TestMatrixAddressedFullMXIDBoundary(t *testing.T) {
	const self = "@:hs"
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "full MXID as a whole token", body: "hey @:hs can you take a look", want: true},
		{name: "full MXID embedded in a larger token (preceded) must NOT match", body: "please cc x@:hs on this", want: false},
		{name: "full MXID embedded in a larger token (followed) must NOT match", body: "reaches @:hstail instead", want: false},
		{name: "full MXID at the very start of the body", body: "@:hs please look", want: true},
		{name: "full MXID at the very end of the body", body: "please look @:hs", want: true},
		{name: "full MXID as the entire body", body: "@:hs", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := matrixEvent{}
			e.Content.Body = tc.body
			f := &MatrixFeedback{self: self}
			if got := f.addressed(e); got != tc.want {
				t.Errorf("addressed(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestContainsWordRuneBoundary pins containsWord's boundary check as
// RUNE-aware, not byte-aware. The pre-fix check read one BYTE adjacent to a
// match and asked whether it was ASCII-alphanumeric: a UTF-8 continuation
// byte from a non-ASCII letter or digit is never ASCII-alphanumeric by that
// test, so a match like "runlore" inside "runloré blah" was wrongly treated
// as bounded — body[7] is 0xC3, the first byte of 'é', which
// isAlphanumericByte reports as false, so containsWord returned true. That
// is a false POSITIVE: an unaddressed message treated as addressing RunLore,
// which — per containsWord's own doc — can open or comment on a real
// knowledge-base PR containing text nobody intended to record. This table
// exercises both sides of the match (a non-ASCII rune immediately before,
// and immediately after) plus a non-ASCII DIGIT case distinct from the
// letter cases: unicode.IsDigit and unicode.IsLetter are separate
// predicates (U+FF13 FULLWIDTH DIGIT THREE is a digit but not a letter), so
// a fix that checked only IsLetter would still misclassify it as a
// boundary.
func TestContainsWordRuneBoundary(t *testing.T) {
	const word = "runlore"
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "non-ASCII letter immediately after the match is not a boundary (the bug)",
			body: "runloreé blah", // "runlore" immediately followed by 'é', not "runlore" with its 'e' replaced
			want: false,
		},
		{
			name: "non-ASCII letter immediately before the match is not a boundary",
			body: "érunlore blah",
			want: false,
		},
		{
			name: "non-ASCII digit immediately after the match is not a boundary",
			body: "runlore３ blah", // U+FF13 FULLWIDTH DIGIT THREE: IsDigit true, IsLetter false
			want: false,
		},
		{
			name: "non-ASCII digit immediately before the match is not a boundary",
			body: "３runlore blah",
			want: false,
		},
		{
			name: "punctuation neighbour is still a boundary — must still match",
			body: "runlore, please look",
			want: true,
		},
		{
			name: "existing ASCII case is unaffected — must still not match",
			body: "runlored note: x",
			want: false,
		},
		{
			name: "word alone",
			body: "runlore",
			want: true,
		},
		{
			name: "word at the very start of the body",
			body: "runlore please",
			want: true,
		},
		{
			name: "word at the very end of the body",
			body: "please look runlore",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsWord(tc.body, word); got != tc.want {
				t.Errorf("containsWord(%q, %q) = %v, want %v", tc.body, word, got, tc.want)
			}
		})
	}
}

// TestContainsWordCombiningMarkBoundary pins the residual gap
// TestContainsWordRuneBoundary's fix left open: it made the boundary check
// RUNE-aware, which correctly rejects NFC (precomposed) "é" — a single rune,
// U+00E9, that unicode.IsLetter reports true for. It does NOT, by itself,
// reject NFD (decomposed) "é": 'e' (U+0065) followed by U+0301 COMBINING
// ACUTE ACCENT as two separate runes. Matching "runlore" against NFD
// "runloré" stops right after the plain 'e', and the next rune is the
// combining mark alone — neither a letter nor a digit — so the pre-fix
// predicate misread it as a boundary and treated the accented word
// "runloré" as the bare word "runlore". Real Matrix clients send NFD in
// practice (it's the default output of macOS input methods), so this is not
// exotic. A combining mark continues the PRECEDING grapheme rather than
// starting a new character, so it must count as word-forming too.
func TestContainsWordCombiningMarkBoundary(t *testing.T) {
	const word = "runlore"
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "NFD combining acute accent immediately after the match is not a boundary",
			body: "runloré blah", // 'e' + U+0301 spells NFD "runloré", not "runlore"
			want: false,
		},
		{
			name: "NFC precomposed é immediately after the match is still not a boundary (regression guard)",
			body: "runloreé blah", // "runlore" + U+00E9 as one extra rune, the case TestContainsWordRuneBoundary already pins
			want: false,
		},
		{
			name: "punctuation neighbour is still a boundary — must still match",
			body: "runlore, please look",
			want: true,
		},
		{
			name: "word alone",
			body: "runlore",
			want: true,
		},
		{
			name: "word at the very start of the body",
			body: "runlore please",
			want: true,
		},
		{
			name: "word at the very end of the body",
			body: "please look runlore",
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsWord(tc.body, word); got != tc.want {
				t.Errorf("containsWord(%q, %q) = %v, want %v", tc.body, word, got, tc.want)
			}
		})
	}
}

// TestMatrixHandleMessageCaptureOffReturnsQuietly pins Fix 2: an m.room.message
// event reaching handleMessage while thread capture is off (threadCapture
// false, Dispatch and Mentions nil) must return quietly rather than
// dereference a nil *thread.Dispatcher. A non-compliant homeserver — or a
// future filter-construction regression — can deliver m.room.message even
// though the /sync filter never asked for it, and if its thread root happens
// to resolve against RunLore's own history (very plausible: investigation
// messages persist in the room regardless of the option), the pre-fix code
// panics. Run's own documented invariant is "a flaky homeserver must never
// crash the agent; at worst feedback pauses."
func TestMatrixHandleMessageCaptureOffReturnsQuietly(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	// No WithThreadCapture supplied: threadCapture is false, Dispatch and
	// Mentions are both nil — exactly the state of a deployment that never
	// opted in to notify.matrix.thread_capture.
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog())
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply9"}
	e.Content.Body = "<@runlore:hs> note: this must not panic"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours" // resolves against RunLore's own history

	f.handleMessage(context.Background(), e) // must return quietly, not panic
}

// TestMatrixHandleMessageBusyOnSaturation: when Dispatch has no free slot,
// handleMessage must tell the human to retry rather than drop the message
// silently — the same contract the Slack mention path uses. The Busy notice
// itself runs through the separate BusyDispatch (Fix 1), so this waits on the
// reply landing rather than draining the (still occupied) main Dispatch.
func TestMatrixHandleMessageBusyOnSaturation(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	d := thread.NewDispatcher(1, time.Minute, matrixTestLog())
	busy := thread.NewDispatcher(1, time.Minute, matrixTestLog())
	block, running := make(chan struct{}), make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(running); <-block }) {
		t.Fatal("first Go refused with a free slot")
	}
	<-running // the one slot is now occupied

	f := newTestMatrixHandler(t, srv, room, self, d, busy, rep)
	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply7"}
	e.Content.Body = "<@runlore:hs> note: please record this"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	f.handleMessage(context.Background(), e)
	waitForReplies(t, rep)
	close(block)
	d.Drain(context.Background())
	busy.Drain(context.Background())

	got := rep.snapshot()
	if len(got) != 1 {
		t.Fatalf("replies = %+v, want exactly one Busy notice", got)
	}
	if got[0].root != "$root-ours" || got[0].channel != room {
		t.Errorf("Busy reply = %+v, want root $root-ours channel %s", got[0], room)
	}
	if !strings.Contains(strings.ToLower(got[0].text), "too many messages") {
		t.Errorf("Busy reply text = %q, want it to explain the saturation", got[0].text)
	}
}

// blockingReplier wraps a fakeMentionReplier and blocks inside ReplyInThread
// until the test releases it, signalling entered the first time it is called.
// It lets a test prove a call happened off some OTHER goroutine (by observing
// entered fire while the caller has already returned) rather than inline.
type blockingReplier struct {
	inner   *fakeMentionReplier
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingReplier) ReplyInThread(ctx context.Context, root, channel, text string) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.inner.ReplyInThread(ctx, root, channel, text)
}

// TestMatrixHandleMessageBusyRunsDetached pins Fix 1: when Dispatch is
// saturated, handleMessage must hand the "I'm too busy" reply to BusyDispatch
// rather than call Mentions.Busy (a real HTTP POST) inline. It proves this by
// making the Busy reply's own network call block, then asserting
// handleMessage still RETURNS well before that call unblocks — the whole
// point being that a burst of addressed messages (the /sync filter allows
// limit:50 per batch) must never chain blocking network calls on the sync
// goroutine, or /sync stalls and 👍/👎 recording pauses right along with it.
func TestMatrixHandleMessageBusyRunsDetached(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()
	log := matrixTestLog()

	// Saturate the main dispatcher so handleMessage takes the Busy path.
	mainDispatch := thread.NewDispatcher(1, time.Minute, log)
	blockMain, runningMain := make(chan struct{}), make(chan struct{})
	if !mainDispatch.Go(context.Background(), func(context.Context) { close(runningMain); <-blockMain }) {
		t.Fatal("first Go refused with a free slot")
	}
	<-runningMain

	rep := &fakeMentionReplier{}
	blocking := &blockingReplier{inner: rep, entered: make(chan struct{}), release: make(chan struct{})}
	busyDispatch := thread.NewDispatcher(1, time.Minute, log)

	f := NewMatrixFeedback(srv.URL, room, "tok", nil, log,
		WithThreadCapture(&thread.Mention{Replier: blocking, Log: log}, mainDispatch, busyDispatch))
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply8"}
	e.Content.Body = "<@runlore:hs> note: please record this"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	returned := make(chan struct{})
	go func() {
		f.handleMessage(context.Background(), e)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("handleMessage blocked instead of dispatching the Busy reply off the sync goroutine")
	}

	// handleMessage already returned; now confirm the Busy reply actually ran
	// (on BusyDispatch's own goroutine), then let it finish.
	select {
	case <-blocking.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Busy notice was never dispatched")
	}
	close(blocking.release)
	close(blockMain)
	busyDispatch.Drain(context.Background())
	mainDispatch.Drain(context.Background())

	if got := rep.snapshot(); len(got) != 1 {
		t.Fatalf("replies = %+v, want exactly one Busy notice", got)
	}
}

// matrixTestLog is a discard logger shared by handleMessage tests.
func matrixTestLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// waitForReplies blocks until rep's doneAt-th reply lands, or fails the test
// after a bound — the dispatched work runs on another goroutine, so waiting on
// a channel the fake closes is the only deterministic way to observe it
// finish.
func waitForReplies(t *testing.T, rep *fakeMentionReplier) {
	t.Helper()
	select {
	case <-rep.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the dispatched mention handler to reply")
	}
}
