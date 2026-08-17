// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// hostileKBUpdate fills EVERY field providers.KBUpdate documents as untrusted —
// Note, Title, Author, URL and Root, all five — with the payload that transport
// treats as markup. Filling all five rather than the ones a renderer happens to
// use today is the point: a field this announcement does not render yet is one
// escape away from being rendered, and the assertion has to already cover it.
func hostileKBUpdate(payload string) providers.KBUpdate {
	return providers.KBUpdate{
		Transport: "slack",
		Root:      payload,
		Route:     providers.KBRouteOpenPR,
		PR:        99,
		URL:       payload,
		Title:     "Operator note: " + payload,
		Author:    payload,
		Note:      "the registry PVC filled up — " + payload,
		At:        time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}
}

// kbUpdateSinkOnFakeTransport wires both chat notifiers at one httptest server
// and returns them keyed by transport, with the sink holding what they posted.
func kbUpdateSinkOnFakeTransport(t *testing.T) (map[string]providers.KBUpdateNotifier, *[]string) {
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
	return map[string]providers.KBUpdateNotifier{"slack": bot, "matrix": mx}, &sent
}

// TestSlackKBUpdateAnnouncementNeutralisesUntrustedFields is the guarantee this
// egress needs from its first commit rather than from a later review.
//
// The announcement is a NEW destination for model-authored text: on the
// freeform route RunLore's own chat model wrote KBUpdate.Note, and unlike the
// thread reply this lands in the notifier's CONFIGURED channel, where RunLore
// posts investigation findings. Unescaped, "<!channel>" in that text mass-pings
// the channel and "<https://evil|https://github.com/acme/kb/pull/7>" renders as
// a clickable link whose visible text is a trusted knowledge-base URL — the
// same pair PR3's Untrusted/RenderReply boundary was built for, arriving
// through a different door.
func TestSlackKBUpdateAnnouncementNeutralisesUntrustedFields(t *testing.T) {
	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	up := hostileKBUpdate("<!channel> <https://evil.example|https://github.com/acme/kb/pull/7>")

	*sent = nil
	if err := sinks["slack"].DeliverKBUpdate(context.Background(), up); err != nil {
		t.Fatalf("DeliverKBUpdate: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("posted %d messages, want 1", len(*sent))
	}
	text := postedThreadText(t, (*sent)[0])

	for _, live := range []string{"<!channel>", "<https://evil.example|"} {
		if strings.Contains(text, live) {
			t.Errorf("an untrusted field reached Slack as live mrkdwn (%q):\n%s", live, text)
		}
	}
	if !strings.Contains(text, "&lt;!channel&gt;") {
		t.Errorf("the untrusted text must still be READABLE, escaped rather than censored:\n%s", text)
	}
	// RunLore's own framing is not escaped with it: the blockquote markers are
	// what keep the quoted note distinguishable from RunLore's own claims, and
	// mrkdwnEscaper would turn them into literal "&gt; ".
	if !strings.Contains(text, "\n> ") {
		t.Errorf("the quoted note lost its blockquote markers to escaping:\n%s", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "&gt;") {
			t.Errorf("RunLore's own blockquote marker was escaped, so the quote no longer renders as one: %q", line)
		}
	}
	if strings.Contains(text, "\ue000") {
		t.Errorf("an untrusted-span mark reached the chat system:\n%s", text)
	}
}

// TestMatrixKBUpdateAnnouncementNeutralisesUntrustedFields is the same
// guarantee against Matrix's own hazard. A plain m.notice body has no markup to
// inject, but .m.rule.roomnotif matches "@room" in content.body and notifies
// every member of the room — the direct analogue of Slack's <!channel>, and
// reachable the same way, through text RunLore's model wrote.
func TestMatrixKBUpdateAnnouncementNeutralisesUntrustedFields(t *testing.T) {
	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	up := hostileKBUpdate("@room now")

	*sent = nil
	if err := sinks["matrix"].DeliverKBUpdate(context.Background(), up); err != nil {
		t.Fatalf("DeliverKBUpdate: %v", err)
	}
	if len(*sent) != 1 {
		t.Fatalf("posted %d messages, want 1", len(*sent))
	}
	body := postedThreadText(t, (*sent)[0])

	if strings.Contains(body, "@room") {
		t.Errorf("an untrusted field reached the room as a live @room ping:\n%s", body)
	}
	if !strings.Contains(body, "@\u2060room") {
		t.Errorf("the token must be marked, not censored — a human still has to be able to read it:\n%s", body)
	}
	if strings.Contains(body, "\ue000") {
		t.Errorf("an untrusted-span mark reached the room:\n%s", body)
	}
}

// TestAnnouncedNoteCannotBreakOutOfTheBlockquote is the announcement's half of
// the same defect, and the worse half.
//
// The blockquote is what stops a line of note text sitting where RunLore's own
// claims sit, and the announcement is the surface where that matters most: it
// lands in the CONFIGURED channel, next to investigation findings, under the
// same bot identity, and the headline it would be forging is right there in the
// same message for comparison. Splitting the note on "\n" alone made that
// forgery a single rune away — UAX #14 gives seven characters a mandatory
// break, and a client renders a new visual line at every one of them, at the
// left margin, outside the quote.
//
// This is asserted byte for byte on the text each transport actually posts, for
// both of them, because the announcement had no second measure to fall back on:
// it did not strip RunLore's status glyphs either, so the escaped line came out
// reading exactly like kbHeadline. Sharing thread.QuoteUntrusted is what closes
// both at once, and the 📚 the note carries below is what pins the glyph strip
// arriving with it.
func TestAnnouncedNoteCannotBreakOutOfTheBlockquote(t *testing.T) {
	const head = "📚 Knowledge base updated — opened PR #42 — https://github.com/o/r/pull/42"
	const forged = "Knowledge base updated — opened PR #999 — https://evil.example/kb"

	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	for _, br := range []struct{ name, sep string }{
		{"LF U+000A", "\n"},
		{"CR U+000D", "\r"},
		{"CRLF is one break, not two", "\r\n"},
		{"VT U+000B", "\v"},
		{"FF U+000C", "\f"},
		{"NEL U+0085", "\u0085"},
		{"LS U+2028", "\u2028"},
		{"PS U+2029", "\u2029"},
	} {
		up := providers.KBUpdate{
			Route: providers.KBRouteOpenPR, PR: 42, URL: "https://github.com/o/r/pull/42",
			Note: "harmless" + br.sep + "📚 " + forged,
		}
		for transport, sink := range sinks {
			t.Run(br.name+"/"+transport, func(t *testing.T) {
				*sent = nil
				if err := sink.DeliverKBUpdate(context.Background(), up); err != nil {
					t.Fatalf("DeliverKBUpdate: %v", err)
				}
				got := postedThreadText(t, (*sent)[0])
				want := head + "\n> harmless\n> " + forged
				if got != want {
					t.Errorf("a %s in the note escaped the blockquote.\n got %q\nwant %q", br.name, got, want)
				}
			})
		}
	}
}

// TestAnnouncementFieldCapsHaveTheirDocumentedValues pins the NUMBERS, the way
// config.TestThreadDefaultsHaveTheirDocumentedValues pins the thread defaults.
//
// The bound test below cannot: every ceiling it checks is recomputed from the
// constant under test, so both sides move together. Raise kbNotePreviewBytes
// from 512 to 3072 and it stays green while the announcement grows unchecked —
// and the announcement is the wider egress of the two, landing in the configured
// channel where investigation findings go rather than in the thread the note
// came from.
//
// 512 is derived rather than picked, and derived the same way the thread reply's
// own notePreviewBytes is (see this file's const block): every untrusted span is
// escaped before it goes on the wire, and the widest escape — Slack's "&" to
// "&amp;" — expands 5x, so 512 bytes reaches the transport as at most ~2.5k
// characters. The three one-line field caps bound values that arrive length-
// capped by NOTHING upstream: KBUpdate.Author in particular is redacted and
// flattened but never shortened (internal/thread's 100-byte cap bounds a MODEL
// PROMPT, not what reaches a notifier).
func TestAnnouncementFieldCapsHaveTheirDocumentedValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"kbNotePreviewBytes", kbNotePreviewBytes, 512},
		{"kbTitleBytes", kbTitleBytes, 200},
		{"kbAuthorBytes", kbAuthorBytes, 100},
		{"kbURLBytes", kbURLBytes, 512},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d — if this was a deliberate retune, redo the 5x-escape "+
				"arithmetic against slackReplyBytes/matrixReplyBytes, and restate the number "+
				"wherever the docs quote it (internal/docsguard will list the pages)", tc.name, tc.got, tc.want)
		}
	}
}

// TestKBUpdateAnnouncementIsBoundedPerTransport pins the second half of making
// this egress safe: what is RENDERED is bounded, independently of what was
// WRITTEN.
//
// KBUpdate.Note deliberately carries the whole note, up to max_note_bytes —
// 8 KiB by default and operator-raisable — so a webhook sink gets the complete
// record. Rendered straight into a chat channel that is an 8 KiB post, on a
// path any channel member can trigger. KBUpdate.Author is redacted and
// flattened but never length-capped at all (chat.go's 100-byte cap is
// prompt-side only), so it arrives unbounded.
func TestKBUpdateAnnouncementIsBoundedPerTransport(t *testing.T) {
	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	tail := "THE-VERY-END-OF-THE-NOTE"
	up := providers.KBUpdate{
		Transport: "slack",
		Root:      "111.222",
		Route:     providers.KBRouteOpenPR,
		PR:        99,
		URL:       "https://github.com/o/r/pull/99",
		Title:     strings.Repeat("t", 4<<10),
		Author:    strings.Repeat("a", 4<<10),
		Note:      strings.Repeat("note text ", 800) + tail, // 8 KiB, the default cap
		At:        time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}

	for transport, sink := range sinks {
		t.Run(transport, func(t *testing.T) {
			*sent = nil
			if err := sink.DeliverKBUpdate(context.Background(), up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			text := postedThreadText(t, (*sent)[0])

			if len(text) > 4<<10 {
				t.Errorf("announcement is %d bytes: an 8 KiB note became an 8 KiB channel post", len(text))
			}
			if strings.Contains(text, tail) {
				t.Errorf("the whole note was rendered; only a preview belongs in a channel post:\n%.500s", text)
			}
			// Each one-line field is cut to its OWN cap, exactly. Asking only
			// that 200 bytes of author or 400 of title be absent left twice the
			// real ceiling unchecked in both cases — a renderer passing 150 to
			// kbField for the author would have satisfied it while posting half
			// again what the cap allows. The expected run is the cap minus the
			// ellipsis kbField appends in place of what it dropped, so a value
			// silently hard-cut with no mark fails here too: a truncated field
			// read as a whole one is the thing that mark exists to prevent.
			for _, f := range []struct {
				name  string
				char  byte
				limit int
			}{
				{"Author", 'a', kbAuthorBytes},
				{"Title", 't', kbTitleBytes},
			} {
				got, want := longestRun(text, f.char), f.limit-len(kbFieldEllipsis)
				if got != want {
					t.Errorf("KBUpdate.%s rendered as a %d-byte run, want exactly %d (its cap is %d, less the %q that marks the cut):\n%.500s",
						f.name, got, want, f.limit, kbFieldEllipsis, text)
				}
			}
			// A reader who cannot tell the quote was shortened reads what
			// survived as the whole note.
			if !strings.Contains(text, "truncated") {
				t.Errorf("a shortened quote must say so:\n%.500s", text)
			}
			// The record itself is never what gets dropped.
			if !strings.Contains(text, "https://github.com/o/r/pull/99") {
				t.Errorf("the pull-request URL must survive:\n%.500s", text)
			}
		})
	}
}

// longestRun returns the length of the longest unbroken run of c in s. It is
// how the test above measures a rendered one-line field without asking the
// renderer where it put it: the fixture fills each field with a single repeated
// character no other part of the announcement repeats.
func longestRun(s string, c byte) int {
	best, run := 0, 0
	for i := range len(s) {
		if s[i] != c {
			run = 0
			continue
		}
		run++
		if run > best {
			best = run
		}
	}
	return best
}

// TestKBUpdateAnnouncementNamesWhatLanded pins the content an operator reading
// the channel needs: which route the write took, the pull request, the entry it
// created, and who it came from.
//
// The route WORDING is asserted on both routes, and each case also denies the
// other route's words. That is not symmetry for its own sake: the headline is
// RunLore's own claim about what it did, and hard-coding it to always say
// "opened" passed this whole suite — the comment case listed no route wording at
// all — so an announcement could tell a channel a new pull request had been
// opened when the note was in fact appended as a comment to one someone else
// owns. Denying the opposite wording is what stops a fix from over-correcting
// into the mirror-image defect.
func TestKBUpdateAnnouncementNamesWhatLanded(t *testing.T) {
	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	for _, tc := range []struct {
		name string
		up   providers.KBUpdate
		want []string
		deny []string
	}{
		{
			name: "open_pr names the entry it created",
			up: providers.KBUpdate{
				Transport: "slack", Root: "111.222", Route: providers.KBRouteOpenPR, PR: 99,
				URL: "https://github.com/o/r/pull/99", Title: "Operator note: OOM in payments",
				Author: "sre-jane", Note: "the real cause was a spot reclaim",
			},
			want: []string{"opened PR #99", "https://github.com/o/r/pull/99", "Operator note: OOM in payments",
				"sre-jane", "> the real cause was a spot reclaim", "slack"},
			deny: []string{"noted on"},
		},
		{
			name: "comment names the pull request it joined",
			up: providers.KBUpdate{
				Transport: "matrix", Root: "$evt", Route: providers.KBRouteComment, PR: 42,
				URL: "https://github.com/o/r/pull/42", Author: "sre-jane", Note: "it recurs after node rotation",
			},
			want: []string{"noted on PR #42", "https://github.com/o/r/pull/42", "sre-jane",
				"> it recurs after node rotation", "matrix"},
			deny: []string{"opened"},
		},
		{
			name: "an unnumbered forge URL still announces the write",
			up: providers.KBUpdate{
				Transport: "slack", Route: providers.KBRouteOpenPR,
				URL: "https://github.com/o/r/kb", Note: "capture this",
			},
			want: []string{"opened PR", "https://github.com/o/r/kb", "> capture this"},
			deny: []string{"noted on", "#0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*sent = nil
			if err := sinks["slack"].DeliverKBUpdate(context.Background(), tc.up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			text := postedThreadText(t, (*sent)[0])
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Errorf("announcement does not name %q:\n%s", want, text)
				}
			}
			for _, deny := range tc.deny {
				if strings.Contains(text, deny) {
					t.Errorf("announcement says %q on the %s route — it is reporting a route the write did not take:\n%s",
						deny, tc.up.Route, text)
				}
			}
		})
	}
}

// TestEveryChatNotifierAnnouncesKBUpdates is the guard against the failure mode
// the capability check itself creates: Multi SKIPS a notifier that does not
// implement KBUpdateNotifier, silently and by design, so a chat sink that never
// grew the method turns announce_kb_updates into a switch that does nothing at
// all — configured, logged as enabled, delivering to nobody.
//
// Every notifier that posts to a channel is listed here on purpose, the
// incoming-webhook Slack path included: it is a documented, supported delivery
// target, and an operator on it would otherwise get exactly that silent no-op.
func TestEveryChatNotifierAnnouncesKBUpdates(t *testing.T) {
	for _, n := range []providers.Notifier{
		NewSlack("https://hooks.slack.com/services/x"),
		NewSlackBot("xoxb-test", "C1"),
		NewMatrix("https://hs.example.org", "!room:example.org", "tok"),
	} {
		if _, ok := n.(providers.KBUpdateNotifier); !ok {
			t.Errorf("%T does not implement providers.KBUpdateNotifier — Multi will skip it silently and the operator's announce_kb_updates will do nothing",
				n)
		}
	}
}

// TestMultiFansAKBUpdateOutToTheRealChatNotifiers closes the same gap end to
// end, through the fan-out an operator actually configures rather than through
// a type assertion: both real chat notifiers receive the announcement.
func TestMultiFansAKBUpdateOutToTheRealChatNotifiers(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, string(body))
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1","event_id":"$1"}`))
	}))
	defer srv.Close()

	bot := NewSlackBot("xoxb-test", "C1")
	bot.baseURL = srv.URL
	mx := NewMatrix(srv.URL, "!room:example.org", "tok")
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), bot, mx)

	if err := m.DeliverKBUpdate(context.Background(), providers.KBUpdate{
		Transport: "slack", Root: "111.222", Route: providers.KBRouteOpenPR, PR: 99,
		URL: "https://github.com/o/r/pull/99", Author: "sre-jane", Note: "a spot reclaim",
	}); err != nil {
		t.Fatalf("DeliverKBUpdate: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("the fan-out posted %d announcements, want one per chat notifier (2)", len(sent))
	}
	for i, body := range sent {
		if !strings.Contains(postedThreadText(t, body), "https://github.com/o/r/pull/99") {
			t.Errorf("announcement %d does not carry the pull request:\n%s", i, body)
		}
	}
}

// TestKBUpdateAnnouncementNeverPostsIntoAThread pins the DEFAULT destination,
// which is also the only one `announce_kb_updates: true` has ever selected.
// The thread reply is the acknowledgement to the person who typed; the
// announcement is what reaches everyone who was not reading that thread. At the
// channel destination it must carry no threading relation — not Slack's
// thread_ts, not Matrix's m.thread — even with both origin handles in hand.
//
// The zero-valued Delivery is asserted alongside the explicit channel value on
// purpose: providers.KBDelivery's zero value IS the channel precisely so a
// producer that predates routing cannot change destination by omission, and a
// test that only ever set the field would not notice that guarantee breaking.
func TestKBUpdateAnnouncementNeverPostsIntoAThread(t *testing.T) {
	for _, delivery := range []providers.KBDelivery{"", providers.KBDeliverChannel} {
		sinks, sent := kbUpdateSinkOnFakeTransport(t)
		up := providers.KBUpdate{
			Transport: "slack", Root: "111.222", Channel: "C-ORIGIN", Delivery: delivery,
			Route: providers.KBRouteOpenPR, PR: 99,
			URL: "https://github.com/o/r/pull/99", Note: "a spot reclaim",
		}
		for transport, sink := range sinks {
			t.Run(transport+"/"+string(delivery), func(t *testing.T) {
				*sent = nil
				if err := sink.DeliverKBUpdate(context.Background(), up); err != nil {
					t.Fatalf("DeliverKBUpdate: %v", err)
				}
				if len(*sent) != 1 {
					t.Fatalf("posted %d messages, want exactly 1 — the channel destination is one post", len(*sent))
				}
				for _, forbidden := range []string{"thread_ts", "m.thread", "111.222"} {
					if strings.Contains((*sent)[0], forbidden) {
						t.Errorf("the announcement carries %q — it must post to the configured channel, never into the originating thread:\n%s",
							forbidden, (*sent)[0])
					}
				}
			})
		}
	}
}

// kbRoutingSinks wires both chat notifiers at one httptest server, recording the
// request PATH alongside the body.
//
// The path is what carries the destination on Matrix — the room id is in the
// URL, not the payload — so a test that read only bodies could not tell an
// announcement delivered into the originating room from one delivered into the
// notifier's configured room, which is the entire distinction being tested.
func kbRoutingSinks(t *testing.T) (slack, matrix providers.KBUpdateNotifier, posts *[]kbPost) {
	t.Helper()
	var sent []kbPost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, kbPost{path: r.URL.Path, body: string(body)})
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1","event_id":"$1"}`))
	}))
	t.Cleanup(srv.Close)

	bot := NewSlackBot("xoxb-test", "C-CONFIGURED")
	bot.baseURL = srv.URL
	return bot, NewMatrix(srv.URL, "!configured-room:example.org", "tok"), &sent
}

type kbPost struct{ path, body string }

// threadedTo reports the thread root a post relates to, and the destination it
// was addressed at, for whichever transport wrote it. "" for a root means the
// post carries no threading relation at all.
func (p kbPost) threadedTo(t *testing.T) (root, dest string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(p.body), &m); err != nil {
		t.Fatalf("decode posted body: %v", err)
	}
	if ts, ok := m["thread_ts"].(string); ok { // Slack
		ch, _ := m["channel"].(string)
		return ts, ch
	}
	if rel, ok := m["m.relates_to"].(map[string]any); ok { // Matrix
		if rel["rel_type"] == "m.thread" {
			id, _ := rel["event_id"].(string)
			return id, p.path
		}
	}
	if ch, ok := m["channel"].(string); ok {
		return "", ch
	}
	return "", p.path
}

// slackOrigin / matrixOrigin are one landed write as each transport reports it,
// with both thread handles present.
func slackOrigin(d providers.KBDelivery) providers.KBUpdate {
	return providers.KBUpdate{
		Transport: "slack", Root: "111.222", Channel: "C-ORIGIN", Delivery: d,
		Route: providers.KBRouteOpenPR, PR: 99, URL: "https://github.com/o/r/pull/99",
		Author: "sre-jane", Note: "a spot reclaim",
	}
}

func matrixOrigin(d providers.KBDelivery) providers.KBUpdate {
	up := slackOrigin(d)
	up.Transport, up.Root, up.Channel = "matrix", "$evt-root", "!room-of-origin:example.org"
	return up
}

// TestAnnouncementRoutesIntoTheOriginatingThread is the feature: with the thread
// destination selected, the transport the note was typed in delivers the
// announcement INTO that thread instead of beside it.
//
// The echo it removes is specific to a single-transport deployment, and so is
// the reasoning. The channel destination is defended as a second destination —
// "the thread reply answers the person who typed, the channel post reaches
// everyone who was not reading that thread" — which holds only when there are
// several sinks. With one, the thread already lives inside the channel the
// announcement posts to, so the second destination IS the first: three notes in
// one thread produced three channel posts restating what the thread had just
// said, and the only remedy was turning the feature off and losing the signal.
//
// Both halves are asserted, and the destination half is the one that bites: a
// Slack post carrying thread_ts but addressed at the notifier's CONFIGURED
// channel rather than the thread's own lands nowhere useful, and on Matrix the
// room is in the URL where a body-only assertion cannot see it at all.
func TestAnnouncementRoutesIntoTheOriginatingThread(t *testing.T) {
	for _, tc := range []struct {
		name             string
		up               providers.KBUpdate
		sink             func(slack, matrix providers.KBUpdateNotifier) providers.KBUpdateNotifier
		wantRoot, wantTo string
	}{
		{
			name:     "slack",
			up:       slackOrigin(providers.KBDeliverThread),
			sink:     func(s, _ providers.KBUpdateNotifier) providers.KBUpdateNotifier { return s },
			wantRoot: "111.222", wantTo: "C-ORIGIN",
		},
		{
			name:     "matrix",
			up:       matrixOrigin(providers.KBDeliverThread),
			sink:     func(_, m providers.KBUpdateNotifier) providers.KBUpdateNotifier { return m },
			wantRoot: "$evt-root", wantTo: "/_matrix/client/v3/rooms/!room-of-origin:example.org/send/m.room.message/",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slack, matrix, posts := kbRoutingSinks(t)
			if err := tc.sink(slack, matrix).DeliverKBUpdate(context.Background(), tc.up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			if len(*posts) != 1 {
				t.Fatalf("posted %d messages, want exactly 1 — the thread destination replaces the channel post, "+
					"it does not accompany it; that echo is what the operator turned the feature off to escape", len(*posts))
			}
			root, dest := (*posts)[0].threadedTo(t)
			if root != tc.wantRoot {
				t.Errorf("the announcement relates to thread root %q, want %q:\n%s", root, tc.wantRoot, (*posts)[0].body)
			}
			if !strings.HasPrefix(dest, tc.wantTo) {
				t.Errorf("the announcement was addressed at %q, want the thread's own channel/room %q — "+
					"a threaded post sent to the configured channel lands in the wrong conversation", dest, tc.wantTo)
			}
			// It is still the announcement, not the thread reply: the provenance
			// line is the whole reason to want it in the thread at all.
			if text := postedThreadText(t, (*posts)[0].body); !strings.Contains(text, "sre-jane") ||
				!strings.Contains(text, "Knowledge base updated") {
				t.Errorf("the threaded announcement lost its own content (who wrote the note, what landed):\n%s", text)
			}
		})
	}
}

// TestAnnouncementFallsBackToTheChannelForANonOriginatingSink pins the decision
// taken for every sink that is NOT the transport the note was typed in.
//
// Only one sink can ever reply into a given thread. A Matrix room cannot reply
// into a Slack thread; the choice for the others is fall back to the channel or
// skip them, and this contract is FALL BACK. Skipping would answer "stop
// repeating yourself in my one channel" with "stop telling the other rooms":
// the echo exists only on the originating transport, where the thread already
// sits inside the announcement's channel, and a room that never saw the thread
// has no echo to remove and no other way to learn the knowledge base moved.
func TestAnnouncementFallsBackToTheChannelForANonOriginatingSink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		up     providers.KBUpdate
		sink   func(slack, matrix providers.KBUpdateNotifier) providers.KBUpdateNotifier
		wantTo string
	}{
		{
			name:   "matrix sink, slack-born note",
			up:     slackOrigin(providers.KBDeliverThread),
			sink:   func(_, m providers.KBUpdateNotifier) providers.KBUpdateNotifier { return m },
			wantTo: "/_matrix/client/v3/rooms/!configured-room:example.org/send/m.room.message/",
		},
		{
			name:   "slack sink, matrix-born note",
			up:     matrixOrigin(providers.KBDeliverThread),
			sink:   func(s, _ providers.KBUpdateNotifier) providers.KBUpdateNotifier { return s },
			wantTo: "C-CONFIGURED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slack, matrix, posts := kbRoutingSinks(t)
			if err := tc.sink(slack, matrix).DeliverKBUpdate(context.Background(), tc.up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			if len(*posts) != 1 {
				t.Fatalf("posted %d messages, want exactly 1 — a sink that cannot reach the thread must still "+
					"announce to its own channel, never be skipped", len(*posts))
			}
			root, dest := (*posts)[0].threadedTo(t)
			if root != "" {
				t.Errorf("a non-originating sink threaded its announcement to %q — it cannot reach another "+
					"transport's thread, and a relation to a root it does not own is a broken message", root)
			}
			if !strings.HasPrefix(dest, tc.wantTo) {
				t.Errorf("the announcement was addressed at %q, want the sink's own configured destination %q", dest, tc.wantTo)
			}
		})
	}
}

// TestThreadRoutingDoesNotSilenceAThreadlessSink is the same fallback for a sink
// that cannot reply into ANY thread, on its own transport or another's.
//
// An incoming-webhook Slack posts to the channel its URL was issued for and has
// no thread_ts to set. It reports Transport "slack" nowhere and cannot, so it
// would be the easiest sink to lose to a routing change: nothing about it errors,
// and an operator who moved their announcements into threads would simply stop
// seeing them here with no signal at all.
func TestThreadRoutingDoesNotSilenceAThreadlessSink(t *testing.T) {
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, string(body))
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	hook := NewSlack(srv.URL)
	if err := hook.DeliverKBUpdate(context.Background(), slackOrigin(providers.KBDeliverThread)); err != nil {
		t.Fatalf("DeliverKBUpdate: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("posted %d messages, want exactly 1 — a sink with no thread to reply into must still announce", len(sent))
	}
	if strings.Contains(sent[0], "thread_ts") {
		t.Errorf("an incoming webhook cannot address a thread; it must not claim to:\n%s", sent[0])
	}
	if !strings.Contains(postedThreadText(t, sent[0]), "https://github.com/o/r/pull/99") {
		t.Errorf("the fallback announcement lost its content:\n%s", sent[0])
	}
}

// TestAnnouncementFallsBackToTheChannelWithoutBothThreadHandles covers the
// originating transport with an incomplete origin.
//
// A thread root alone does not address a reply: Slack's chat.postMessage needs
// the channel and Matrix's send needs the room, and a KBUpdate can carry a root
// without one — a thread context rebuilt from a Matrix event stamp after a
// restart, an origin that was never a thread. The write has already landed, so
// the fallback must ANNOUNCE it at channel level rather than drop it: losing an
// announcement for a write that really happened is the failure this whole path
// exists to avoid.
func TestAnnouncementFallsBackToTheChannelWithoutBothThreadHandles(t *testing.T) {
	for _, tc := range []struct {
		name        string
		root, chann string
	}{
		{name: "no channel", root: "111.222"},
		{name: "no root", chann: "C-ORIGIN"},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slack, _, posts := kbRoutingSinks(t)
			up := slackOrigin(providers.KBDeliverThread)
			up.Root, up.Channel = tc.root, tc.chann
			if err := slack.DeliverKBUpdate(context.Background(), up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			if len(*posts) != 1 {
				t.Fatalf("posted %d messages, want exactly 1 — an unaddressable thread must fall back to the "+
					"channel, not swallow the announcement for a write that landed", len(*posts))
			}
			root, dest := (*posts)[0].threadedTo(t)
			if root != "" {
				t.Errorf("threaded to %q with an incomplete origin (root=%q channel=%q) — a half handle cannot "+
					"address a reply", root, tc.root, tc.chann)
			}
			if dest != "C-CONFIGURED" {
				t.Errorf("addressed at %q, want the configured channel C-CONFIGURED", dest)
			}
		})
	}
}

// TestAnnouncementBothReachesTheThreadAndTheChannel pins the third destination:
// the thread gets the announcement's provenance (who wrote the note, on which
// chat system — which the thread reply does not carry) and the channel still
// gets the broadcast.
//
// It is deliberately NOT what `true` maps to: `both` produces strictly more
// messages than the shipped behaviour, so an operator has to ask for it.
func TestAnnouncementBothReachesTheThreadAndTheChannel(t *testing.T) {
	slack, _, posts := kbRoutingSinks(t)
	if err := slack.DeliverKBUpdate(context.Background(), slackOrigin(providers.KBDeliverBoth)); err != nil {
		t.Fatalf("DeliverKBUpdate: %v", err)
	}
	if len(*posts) != 2 {
		t.Fatalf("posted %d messages, want 2 (the thread and the channel)", len(*posts))
	}
	seen := map[string]string{}
	for _, p := range *posts {
		root, dest := p.threadedTo(t)
		seen[root] = dest
	}
	if got, ok := seen["111.222"]; !ok || got != "C-ORIGIN" {
		t.Errorf("no announcement reached the originating thread (root→dest: %v)", seen)
	}
	if got, ok := seen[""]; !ok || got != "C-CONFIGURED" {
		t.Errorf("no unthreaded announcement reached the configured channel (root→dest: %v)", seen)
	}
}

// TestBothStillPostsToTheChannelWhenTheThreadPostFails pins that the two
// deliveries are independent. A wedged or renamed thread must not cost the
// broadcast a write that already landed on the forge, so the errors are joined
// rather than short-circuited.
func TestBothStillPostsToTheChannelWhenTheThreadPostFails(t *testing.T) {
	var sent []kbPost
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = append(sent, kbPost{path: r.URL.Path, body: string(body)})
		if strings.Contains(string(body), "thread_ts") {
			_, _ = w.Write([]byte(`{"ok":false,"error":"thread_not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1"}`))
	}))
	defer srv.Close()

	bot := NewSlackBot("xoxb-test", "C-CONFIGURED")
	bot.baseURL = srv.URL

	err := bot.DeliverKBUpdate(context.Background(), slackOrigin(providers.KBDeliverBoth))
	if err == nil {
		t.Error("a failed thread post must be reported, not swallowed here — the announcer is what decides to swallow it")
	}
	if len(sent) != 2 {
		t.Fatalf("posted %d messages, want 2 — the channel half must survive the thread half failing", len(sent))
	}
	if root, _ := sent[1].threadedTo(t); root != "" {
		t.Errorf("the second post is threaded (%q); the channel half never was", root)
	}
}

// TestKBUpdateFieldsAreAllRenderedOrDeliberatelyNot keeps the renderer honest
// about the event it is given. providers.KBUpdate has a reflection test forcing
// every field to be classified trusted or untrusted; this is the notifier's
// side of it — a field added to the event must be either shown to the operator
// or listed here as deliberately withheld, so "we forgot to render it" cannot
// pass as "we chose not to".
func TestKBUpdateFieldsAreAllRenderedOrDeliberatelyNot(t *testing.T) {
	// What the announcement must show, and the form it shows it in — Route is a
	// sentence a human reads, not the raw enum value.
	shows := map[string]string{
		"Transport": "matrix",
		"Route":     "opened PR",
		"PR":        "4242",
		"URL":       "https://github.com/o/r/pull/4242",
		"Title":     "Operator note: OOM",
		"Author":    "sre-jane",
		"Note":      "a spot reclaim",
	}
	// Root and Channel are opaque per-transport handles (a Slack thread_ts and
	// channel id, a Matrix event id and room id). They identify the thread for a
	// programmatic consumer and address a threaded delivery; printed into a
	// channel they are noise a human cannot act on, and the announcement already
	// names the transport it came from. At is the wall clock of a message the
	// chat system timestamps itself. Delivery is an instruction to the sink about
	// where to post, not a fact about the write, so nothing about it belongs in
	// the message a human reads.
	withheld := map[string]string{
		"Root":     "$evt-root",
		"Channel":  "!room-of-origin:example.org",
		"Delivery": "both",
		"At":       "2026-08-16T09:00:00Z",
	}

	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	up := providers.KBUpdate{
		Transport: "matrix", Root: "$evt-root", Channel: "!room-of-origin:example.org",
		Delivery: providers.KBDeliverBoth, Route: providers.KBRouteOpenPR, PR: 4242,
		URL: "https://github.com/o/r/pull/4242", Title: "Operator note: OOM",
		Author: "sre-jane", Note: "a spot reclaim", At: time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}
	*sent = nil
	if err := sinks["slack"].DeliverKBUpdate(context.Background(), up); err != nil {
		t.Fatalf("DeliverKBUpdate: %v", err)
	}
	text := postedThreadText(t, (*sent)[0])

	rt := reflect.TypeOf(up)
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		want, shown := shows[name]
		hide, hidden := withheld[name]
		switch {
		case shown == hidden:
			t.Errorf("KBUpdate.%s is in neither or both of shows/withheld — decide whether an operator reading the channel is told it", name)
		case shown && !strings.Contains(text, want):
			t.Errorf("KBUpdate.%s must be announced as %q, and is not:\n%s", name, want, text)
		case hidden && strings.Contains(text, hide):
			t.Errorf("KBUpdate.%s is listed as deliberately withheld but is rendered — update the list or stop rendering it:\n%s", name, text)
		}
	}

	// The reverse direction, which its internal/providers sibling
	// (TestKBUpdateClassifiesEveryFieldForEscaping) already checks and this side
	// did not: an entry naming a field KBUpdate no longer has asserts nothing
	// about anything. The loop above only walks the STRUCT, so a renamed or
	// deleted field leaves its entry behind, still listed, still reading in
	// review as a decision someone made about a field that is being rendered —
	// and the new field that replaced it falls straight into the "in neither"
	// arm, whose message is about the new name rather than the stale one.
	for _, m := range []map[string]string{shows, withheld} {
		for name := range m {
			if _, ok := rt.FieldByName(name); !ok {
				t.Errorf("shows/withheld classifies %q, but KBUpdate has no such field — stale entry, "+
					"so nothing is checking whatever replaced it", name)
			}
		}
	}
}
