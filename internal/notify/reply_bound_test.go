// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// threadReplyBudgets is each transport's own ceiling, keyed the way
// Multi.ThreadRepliers keys its repliers, so the table below drives the real
// providers.ThreadNotifier surface rather than a concrete type — a third
// transport wired into Multi fails these the day it is added.
var threadReplyBudgets = map[string]int{
	"slack":  slackReplyBytes,
	"matrix": matrixReplyBytes,
}

// postedThreadText decodes the one field each transport carries a thread
// reply's text in: Slack's chat.postMessage "text", Matrix's m.notice "body".
func postedThreadText(t *testing.T, body string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	for _, k := range []string{"text", "body"} {
		if s, ok := m[k].(string); ok {
			return s
		}
	}
	t.Fatalf(`posted body carries neither a Slack "text" nor a Matrix "body": %s`, body)
	return ""
}

// threadRepliersOnFakeTransport wires both real repliers at one httptest
// server and returns them with the sink that captured what they posted.
func threadRepliersOnFakeTransport(t *testing.T) (map[string]providers.ThreadNotifier, *[]string) {
	t.Helper()
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, string(body))
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1","event_id":"$1"}`))
	}))
	t.Cleanup(srv.Close)

	bot := NewSlackBot("xoxb-test", "C1")
	bot.baseURL = srv.URL
	mx := NewMatrix(srv.URL, "!room:example.org", "tok")
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), bot, mx)
	return m.ThreadRepliers(), &sent
}

// TestTransportReplyBudgetsHaveTheirDocumentedValues pins the NUMBERS, the way
// config.TestThreadDefaultsHaveTheirDocumentedValues pins the thread defaults.
//
// Nothing else in this file can. threadReplyBudgets is BUILT from these two
// constants, and every bound test below reads its budget out of that map, so
// both sides of `len(text) > budget` move together and the assertions hold for
// any value at all. Raising slackReplyBytes to 100_000 — roughly 3x the ~40,000
// characters Slack documents — keeps the whole suite green while a 148-byte
// reply becomes a 50,476-byte post: rejected by Slack, so the human is told
// NOTHING about a note that WAS written to the forge, which is the one outcome
// this feature exists to make impossible and the exact failure this bound was
// added for.
//
// Each number is a property of the chat system, not a tuning knob, and each is
// derived: see the constants' own doc comments for the derivation. Changing one
// should be a deliberate edit here against the transport's published limit.
func TestTransportReplyBudgetsHaveTheirDocumentedValues(t *testing.T) {
	if got, want := slackReplyBytes, 35_000; got != want {
		t.Errorf("slackReplyBytes = %d, want %d — Slack documents ~40,000 characters for "+
			"chat.postMessage's text, and this stays under that with room for the JSON envelope; "+
			"raising it past the real limit turns a long reply into a rejected post and the human "+
			"hears nothing about a write that landed", got, want)
	}
	if got, want := matrixReplyBytes, 32<<10; got != want {
		t.Errorf("matrixReplyBytes = %d, want %d — the Matrix spec caps a whole event at 65,536 "+
			"bytes including signatures, hashes and relations, and the body is one field of it", got, want)
	}
	// The map every bound test reads its budget from really is built from the two
	// constants above: a transport wired into Multi whose budget came from
	// somewhere else would be pinned by neither.
	for transport, want := range map[string]int{"slack": slackReplyBytes, "matrix": matrixReplyBytes} {
		if got := threadReplyBudgets[transport]; got != want {
			t.Errorf("threadReplyBudgets[%q] = %d, want the transport's own constant (%d)", transport, got, want)
		}
	}
}

// TestThreadReplyIsBoundedAtTheTransport is the guard on the failure the whole
// thread-feedback feature exists to prevent, reached by turning a documented
// knob.
//
// model.chat.max_tokens is operator-configurable and config.Validate rejects
// only a negative value, so raising it raises what one ChatReply.Reply can be —
// and the reply path only TrimSpaces it. Neither ReplyInThread bounded the
// composed message before posting, so a large enough answer makes the post
// itself FAIL: Slack's text budget is ~40k characters and its worst-case
// mrkdwn escape (& to &amp;) expands 5x. The human is then told NOTHING, while
// their note WAS written to the forge — silence after a write, which is the one
// outcome this feature exists to make impossible.
//
// So the bound sits at the transport, where the limit lives, and what it drops
// is the model's prose. The PR link and the quoted note are the record of what
// happened; the tail of an answer is not. A truncated reply says so, because a
// human who cannot tell a message was shortened reads what survived as the
// whole of it.
func TestThreadReplyIsBoundedAtTheTransport(t *testing.T) {
	repliers, sent := threadRepliersOnFakeTransport(t)
	if len(repliers) == 0 {
		t.Fatal("test setup: no thread repliers to check")
	}
	// The shape internal/thread composes on the freeform route: the model's
	// answer through modelVoice, then RunLore's own status line, then the
	// recorded note quoted back (see Responder.freeform and recordedBlock).
	prose := strings.TrimSpace(strings.Repeat("the pod was evicted and rescheduled ", 3000))
	const kbURL = "https://github.com/o/r/pull/42"
	const noteQuote = "the real cause was a spot-node reclaim"
	reply := "> " + thread.Untrusted(prose) + "\n" +
		"📝 Noted on the knowledge-base PR #42 — " + thread.Untrusted(kbURL) + "\n" +
		"> " + thread.Untrusted(noteQuote)

	for transport, tn := range repliers {
		t.Run(transport, func(t *testing.T) {
			budget, ok := threadReplyBudgets[transport]
			if !ok {
				t.Fatalf("transport %q has no stated reply budget — a transport wired into Multi with no bound is the defect this test exists for", transport)
			}
			*sent = nil
			if err := tn.ReplyInThread(context.Background(), "$root", "", reply); err != nil {
				t.Fatalf("ReplyInThread: %v", err)
			}
			if len(*sent) != 1 {
				t.Fatalf("sent %d requests, want 1", len(*sent))
			}
			text := postedThreadText(t, (*sent)[0])

			if len(text) > budget {
				t.Errorf("posted %d bytes, over this transport's %d-byte budget — the post would be rejected and the human told nothing about a write that happened", len(text), budget)
			}
			// The record survives. Dropping THIS is the failure; dropping prose is
			// the remedy.
			for _, must := range []string{kbURL, noteQuote} {
				if !strings.Contains(text, must) {
					t.Errorf("the record of what was written did not survive truncation (%q missing) — prose is what gets dropped, never this:\n%.400s", must, text)
				}
			}
			if !strings.Contains(text, "too long") {
				t.Errorf("a shortened reply must say it was shortened; got:\n%.400s", text)
			}
			// Whole lines are dropped, never part of one: a blockquoted line cut
			// in the middle would leave model prose at the left margin, where
			// RunLore's own claims sit — the exact forgery thread.modelVoice's
			// blockquote exists to prevent.
			for _, line := range strings.Split(text, "\n") {
				if line == "" || strings.HasPrefix(line, "> ") {
					continue
				}
				if strings.HasPrefix(line, "⚠️") || strings.HasPrefix(line, "📝") {
					continue // RunLore's own status lines
				}
				t.Errorf("truncation left a fragment at the left margin, where RunLore's own lines sit: %q", line)
			}
		})
	}
}

// TestSlackThreadReplyIsBoundedAfterEscapingNotBefore pins WHERE the bound is
// applied, which is the half that is easy to get wrong and impossible to see.
//
// escapeMrkdwn expands: every "&" becomes "&amp;", five bytes for one. A reply
// measured BEFORE escaping can therefore pass a byte check and still arrive at
// Slack five times over its budget — the bound would be real, tested, and
// bounding the wrong string. What has to fit is what goes on the wire.
func TestSlackThreadReplyIsBoundedAfterEscapingNotBefore(t *testing.T) {
	repliers, sent := threadRepliersOnFakeTransport(t)
	tn := repliers["slack"]
	if tn == nil {
		t.Fatal("test setup: no slack replier")
	}
	// Comfortably under the budget as written, five times over it once escaped.
	amps := strings.Repeat("&", slackReplyBytes/2)
	reply := "> " + thread.Untrusted(amps) + "\n" +
		"📝 Noted on the knowledge-base PR #42 — " + thread.Untrusted("https://github.com/o/r/pull/42")
	if len(reply) > slackReplyBytes {
		t.Fatalf("test setup: the unescaped reply (%d bytes) must fit the budget (%d) so only the escaped size can trip the bound", len(reply), slackReplyBytes)
	}

	*sent = nil
	if err := tn.ReplyInThread(context.Background(), "$root", "", reply); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}
	text := postedThreadText(t, (*sent)[0])
	if len(text) > slackReplyBytes {
		t.Errorf("posted %d escaped bytes over a %d-byte budget: the bound was applied to the reply as composed, not to what actually goes on the wire", len(text), slackReplyBytes)
	}
	if !strings.Contains(text, "https://github.com/o/r/pull/42") {
		t.Errorf("the knowledge-base URL did not survive:\n%.400s", text)
	}
}

// TestThreadReplyUnderBudgetIsPostedUnchanged is the other half of the bound:
// the ordinary reply — every reply, in practice — must reach the room exactly
// as composed, with no truncation notice bolted onto it.
func TestThreadReplyUnderBudgetIsPostedUnchanged(t *testing.T) {
	repliers, sent := threadRepliersOnFakeTransport(t)
	reply := "> " + thread.Untrusted("It was a spot reclaim.") + "\n" +
		"📝 Noted on the knowledge-base PR #42 — " + thread.Untrusted("https://github.com/o/r/pull/42")
	want := "> It was a spot reclaim.\n📝 Noted on the knowledge-base PR #42 — https://github.com/o/r/pull/42"

	for transport, tn := range repliers {
		t.Run(transport, func(t *testing.T) {
			*sent = nil
			if err := tn.ReplyInThread(context.Background(), "$root", "", reply); err != nil {
				t.Fatalf("ReplyInThread: %v", err)
			}
			if got := postedThreadText(t, (*sent)[0]); got != want {
				t.Errorf("a reply within budget was altered:\ngot  %q\nwant %q", got, want)
			}
		})
	}
}

// TestThreadReplyBoundsOneUnbrokenLine covers the case dropping whole lines
// cannot fix: a model answer with no newline in it at all, longer on its own
// than the whole budget. There is no record below it to protect — the record is
// several short lines, and this reply has none — so the head is kept, which is
// the part a human reads first, and the blockquote marker that keeps it
// distinguishable from RunLore's own claims is kept with it.
func TestThreadReplyBoundsOneUnbrokenLine(t *testing.T) {
	repliers, sent := threadRepliersOnFakeTransport(t)
	one := strings.Repeat("x", 2*slackReplyBytes)

	for transport, tn := range repliers {
		t.Run(transport, func(t *testing.T) {
			budget := threadReplyBudgets[transport]
			*sent = nil
			if err := tn.ReplyInThread(context.Background(), "$root", "", "> "+thread.Untrusted(one)); err != nil {
				t.Fatalf("ReplyInThread: %v", err)
			}
			text := postedThreadText(t, (*sent)[0])
			if len(text) > budget {
				t.Errorf("posted %d bytes over a %d-byte budget", len(text), budget)
			}
			if !strings.HasPrefix(text, "> ") {
				t.Errorf("the blockquote marker was cut away, so model prose now sits at the left margin: %.80q", text)
			}
			if !strings.Contains(text, "too long") {
				t.Errorf("a cut-off reply must say it was cut off; got:\n%.200s", text)
			}
		})
	}
}
