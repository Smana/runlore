// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// silenceRecordSink is a FeedbackSink + SilenceSink test double. It implements
// BOTH interfaces — like *outcome.Ledger does in production — so
// WithSilenceReactions' type assertion on the configured sink succeeds, and a
// test can assert on either capability's calls independently (including their
// ABSENCE, which matters more here than their presence: see the guard-move
// tests below).
type silenceRecordSink struct {
	mu       sync.Mutex
	feedback []feedbackCall
	silences []silenceCall
}

type feedbackCall struct {
	key, rating, user string
	at                time.Time
}

type silenceCall struct {
	key, user string
	window    time.Duration
	at        time.Time
}

func (s *silenceRecordSink) Feedback(key, rating, user string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.feedback = append(s.feedback, feedbackCall{key: key, rating: rating, user: user, at: at})
	return nil
}

func (s *silenceRecordSink) Silence(key string, window time.Duration, user string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.silences = append(s.silences, silenceCall{key: key, window: window, user: user, at: at})
	return nil
}

func (s *silenceRecordSink) snapshot() (feedback []feedbackCall, silences []silenceCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]feedbackCall(nil), s.feedback...), append([]silenceCall(nil), s.silences...)
}

// matrixSilenceServer serves the single event fetch handleReaction's
// triggerKeyFor needs: $msg1, stamped and sent by self (one of RunLore's own
// investigation messages), and $spoof, identically stamped but sent by
// somebody else — the trust anchor contextFor must reject (see
// TestMatrixSilenceReactionOnForeignMessageRecordsNothing).
func matrixSilenceServer(t *testing.T, self, triggerKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/event/$msg1"):
			_ = json.NewEncoder(w).Encode(map[string]any{"sender": self,
				"content": map[string]any{triggerKeyContentField: triggerKey}})
		case strings.HasSuffix(r.URL.Path, "/event/$spoof"):
			// The attack the self-check closes: a room member posts their OWN
			// message carrying the trigger field, then silences on it.
			_ = json.NewEncoder(w).Encode(map[string]any{"sender": "@eve:hs",
				"content": map[string]any{triggerKeyContentField: "victim-trigger"}})
		default:
			t.Fatalf("unexpected event fetch: %s", r.URL.Path)
		}
	}))
}

// bellReaction builds one 🔕 (or any other key) m.reaction targeting target.
func bellReaction(sender, target, key string) matrixEvent {
	e := matrixEvent{Type: "m.reaction", Sender: sender}
	e.Content.RelatesTo.RelType = "m.annotation"
	e.Content.RelatesTo.EventID = target
	e.Content.RelatesTo.Key = key
	return e
}

// TestMatrixSilenceReactionRecordsOnOwnMessage pins the happy path: a 🔕 on
// one of RunLore's own messages records exactly one Silence call, at the
// configured default window, attributed to the reacting user.
func TestMatrixSilenceReactionRecordsOnOwnMessage(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixSilenceServer(t, self, "trig-1")
	defer srv.Close()

	sink := &silenceRecordSink{}
	f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(), WithSilenceReactions(4*time.Hour, 24*time.Hour))
	f.self = self

	before := time.Now()
	f.handleReaction(context.Background(), bellReaction("@alice:hs", "$msg1", "🔕"))
	after := time.Now()

	feedback, silences := sink.snapshot()
	if len(feedback) != 0 {
		t.Fatalf("feedback = %+v, want none recorded for a 🔕 reaction", feedback)
	}
	if len(silences) != 1 {
		t.Fatalf("silences = %+v, want exactly one", silences)
	}
	got := silences[0]
	if got.key != "trig-1" || got.window != 4*time.Hour || got.user != "@alice:hs" {
		t.Fatalf("silence = %+v, want key=trig-1 window=4h user=@alice:hs", got)
	}
	if got.at.Before(before) || got.at.After(after) {
		t.Fatalf("silence.at = %v, want between %v and %v", got.at, before, after)
	}
}

// TestMatrixSilenceReactionStripsVariationSelector pins the same handling
// 👍/👎 already get: the emoji variation selector (U+FE0F) is stripped, so
// "🔕️" and "🔕" are the same action.
func TestMatrixSilenceReactionStripsVariationSelector(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixSilenceServer(t, self, "trig-1")
	defer srv.Close()

	sink := &silenceRecordSink{}
	f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(), WithSilenceReactions(4*time.Hour, 24*time.Hour))
	f.self = self

	f.handleReaction(context.Background(), bellReaction("@alice:hs", "$msg1", "🔕️")) // with variation selector

	_, silences := sink.snapshot()
	if len(silences) != 1 || silences[0].key != "trig-1" {
		t.Fatalf("silences = %+v, want exactly one for trig-1", silences)
	}
}

// TestMatrixSilenceReactionOffRecordsNothing: with silenceReactions OFF, a 🔕
// records nothing — even when the listener is running for feedback reactions.
// This is the /sync-filter capability boundary's code-level half.
func TestMatrixSilenceReactionOffRecordsNothing(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixSilenceServer(t, self, "trig-1")
	defer srv.Close()

	sink := &silenceRecordSink{}
	f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(), WithFeedbackReactions())
	f.self = self

	f.handleReaction(context.Background(), bellReaction("@alice:hs", "$msg1", "🔕"))

	feedback, silences := sink.snapshot()
	if len(silences) != 0 {
		t.Fatalf("silences = %+v, want none: silence_reactions is off", silences)
	}
	if len(feedback) != 0 {
		t.Fatalf("feedback = %+v, want none: a 🔕 must never be recorded as a rating", feedback)
	}
}

// TestMatrixSilenceReactionsOnFeedbackOffGuardsPerCapability is the
// guard-placement test: with feedback_reactions OFF and silence_reactions ON,
// a 👍 must record nothing and a 🔕 must still record a silence. A single
// early return at the top of handleReaction (the pre-fix shape) cannot
// express this — it either grants both capabilities or neither.
func TestMatrixSilenceReactionsOnFeedbackOffGuardsPerCapability(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixSilenceServer(t, self, "trig-1")
	defer srv.Close()

	sink := &silenceRecordSink{}
	f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(), WithSilenceReactions(4*time.Hour, 24*time.Hour))
	f.self = self

	f.handleReaction(context.Background(), bellReaction("@alice:hs", "$msg1", "👍"))
	f.handleReaction(context.Background(), bellReaction("@bob:hs", "$msg1", "🔕"))

	feedback, silences := sink.snapshot()
	if len(feedback) != 0 {
		t.Fatalf("feedback = %+v, want none: feedback_reactions is off", feedback)
	}
	if len(silences) != 1 || silences[0].user != "@bob:hs" {
		t.Fatalf("silences = %+v, want exactly one from @bob:hs", silences)
	}
}

// TestMatrixSilenceReactionOnForeignMessageRecordsNothing pins the trust
// anchor: a 🔕 on an event NOT sent by the bot must record nothing, even
// though the event carries a forged trigger-key field. Without this, any
// room member could silence an arbitrary incident by posting their own
// message with a chosen io.runlore.trigger_key — a denial-of-investigation
// primitive. Routed unchanged through triggerKeyFor/contextFor.
func TestMatrixSilenceReactionOnForeignMessageRecordsNothing(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixSilenceServer(t, self, "trig-1")
	defer srv.Close()

	sink := &silenceRecordSink{}
	f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(), WithSilenceReactions(4*time.Hour, 24*time.Hour))
	f.self = self

	f.handleReaction(context.Background(), bellReaction("@eve:hs", "$spoof", "🔕"))

	feedback, silences := sink.snapshot()
	if len(silences) != 0 || len(feedback) != 0 {
		t.Fatalf("recorded = feedback:%+v silences:%+v, want nothing — the target was not sent by the bot", feedback, silences)
	}
}

// TestMatrixSilenceReactionOtherEmojiRecordsNothing: an unrelated emoji (🎉)
// must not be recorded as either a rating or a silence, even with both
// capabilities on.
func TestMatrixSilenceReactionOtherEmojiRecordsNothing(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixSilenceServer(t, self, "trig-1")
	defer srv.Close()

	sink := &silenceRecordSink{}
	f := NewMatrixFeedback(srv.URL, room, "tok", sink, matrixTestLog(),
		WithFeedbackReactions(), WithSilenceReactions(4*time.Hour, 24*time.Hour))
	f.self = self

	f.handleReaction(context.Background(), bellReaction("@alice:hs", "$msg1", "🎉"))

	feedback, silences := sink.snapshot()
	if len(feedback) != 0 || len(silences) != 0 {
		t.Fatalf("recorded = feedback:%+v silences:%+v, want nothing for an unrelated emoji", feedback, silences)
	}
}

// TestMatrixSilenceOptionGuardsMisconfiguration pins WithSilenceReactions'
// own validation: a non-positive default window, or a default exceeding the
// max, must leave the capability OFF rather than silently clamp it or panic
// later on a bogus ledger write.
func TestMatrixSilenceOptionGuardsMisconfiguration(t *testing.T) {
	const room = "!r:hs"
	tests := []struct {
		name          string
		defaultWindow time.Duration
		maxWindow     time.Duration
	}{
		{name: "zero default window", defaultWindow: 0, maxWindow: 24 * time.Hour},
		{name: "negative default window", defaultWindow: -time.Hour, maxWindow: 24 * time.Hour},
		{name: "default exceeds max", defaultWindow: 48 * time.Hour, maxWindow: 24 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &silenceRecordSink{}
			f := NewMatrixFeedback("http://unused", room, "tok", sink, matrixTestLog(), WithSilenceReactions(tc.defaultWindow, tc.maxWindow))
			if f.silenceReactions {
				t.Fatal("silenceReactions = true, want the capability left off on a misconfigured window")
			}
		})
	}
}

// TestMatrixSilenceOptionWarnsWhenItDropsTheCapability pins the promise
// WithSilenceReactions' own doc comment makes two lines above its guards — that
// threading maxWindow here makes "a misconfiguration visible at construction
// rather than at the first click". Both guards returned bare, so nothing was
// emitted anywhere: the operator set notify.matrix.silence_reactions: true,
// startup said nothing, and every 🔕 was ignored in silence.
func TestMatrixSilenceOptionWarnsWhenItDropsTheCapability(t *testing.T) {
	const room = "!r:hs"
	for _, tc := range []struct {
		name          string
		sink          FeedbackSink
		defaultWindow time.Duration
		maxWindow     time.Duration
		want          string
	}{
		{name: "zero default window", sink: &silenceRecordSink{}, defaultWindow: 0, maxWindow: 24 * time.Hour, want: "window"},
		{name: "default exceeds max", sink: &silenceRecordSink{}, defaultWindow: 48 * time.Hour, maxWindow: 24 * time.Hour, want: "window"},
		{name: "sink cannot record silences", sink: &recordSink{}, defaultWindow: 4 * time.Hour, maxWindow: 24 * time.Hour, want: "ledger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			f := NewMatrixFeedback("http://unused", room, "tok", tc.sink, log, WithSilenceReactions(tc.defaultWindow, tc.maxWindow))
			if f.silenceReactions {
				t.Fatal("silenceReactions = true, want the capability left off")
			}
			got := buf.String()
			if got == "" {
				t.Fatal("nothing was logged: the dropped capability is invisible until the first ignored 🔕")
			}
			if !strings.Contains(got, "level=WARN") {
				t.Errorf("log = %q, want a WARN — an ignored opt-in is not routine information", got)
			}
			if !strings.Contains(got, "silence_reactions") {
				t.Errorf("log = %q, want it to name the config key the operator turned on", got)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("log = %q, want it to say why (%q)", got, tc.want)
			}
		})
	}
}

// TestMatrixSilenceOptionRequiresSilenceCapableSink pins the other guard: a
// sink that does not implement SilenceSink must leave the capability off
// rather than panic on the first 🔕.
func TestMatrixSilenceOptionRequiresSilenceCapableSink(t *testing.T) {
	const room = "!r:hs"
	sink := &recordSink{} // implements FeedbackSink only, not SilenceSink
	f := NewMatrixFeedback("http://unused", room, "tok", sink, matrixTestLog(), WithSilenceReactions(4*time.Hour, 24*time.Hour))
	if f.silenceReactions {
		t.Fatal("silenceReactions = true, want the capability left off when the sink cannot record silences")
	}
}
