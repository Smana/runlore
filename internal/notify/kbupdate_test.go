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
	"github.com/Smana/runlore/internal/thread"
)

// hostileKBUpdate fills EVERY field providers.KBUpdate documents as untrusted —
// Note, Title, Author, URL, Root and Channel, all six — with the payload that
// transport treats as markup. Filling all six rather than the ones a renderer
// happens to use today is the point: a field this announcement does not render
// yet is one escape away from being rendered, and the assertion has to already
// cover it.
//
// transport and delivery are PARAMETERS rather than constants because the
// announcement now has two renderings and three destinations, and hostility is a
// property of each. A fixture pinned to Transport "slack" with no Channel and no
// Delivery — which is what this was — can only ever exercise the channel post:
// kbAnnounceTargets refuses the thread route without both handles, so every
// neutralisation assertion in this file silently covered one of the two paths.
// Replacing both ReplyInThread calls with a raw, unescaped, unbounded post left
// the entire repository green.
//
// Channel carries the payload rather than a benign id for the reason Root does:
// it is a transport-reported string, so it is untrusted, and on the thread route
// it is also ADDRESSING. Hostile bytes there prove it is used as a handle and
// never interpolated into the message.
func hostileKBUpdate(payload, transport string, delivery providers.KBDelivery) providers.KBUpdate {
	return providers.KBUpdate{
		Transport: transport,
		Root:      payload,
		Channel:   payload,
		Delivery:  delivery,
		Route:     providers.KBRouteOpenPR,
		PR:        99,
		URL:       payload,
		Title:     "Operator note: " + payload,
		Author:    payload,
		Note:      "the registry PVC filled up — " + payload,
		At:        time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}
}

// kbDestinations is every destination an announcement can be delivered at.
// Hostility tests run the whole set: the in-thread rendering and the channel
// rendering are different messages built by different functions and posted by
// different methods, and "both" is the only case that exercises them together.
var kbDestinations = []providers.KBDelivery{
	providers.KBDeliverChannel, providers.KBDeliverThread, providers.KBDeliverBoth,
}

// kbDestName names a destination for a subtest, spelling the zero value "channel"
// rather than "".
func kbDestName(d providers.KBDelivery) string {
	if d == providers.KBDeliverChannel {
		return "channel"
	}
	return string(d)
}

// postedTexts returns the rendered message of EVERY post captured, so an
// assertion covers each one rather than only (*sent)[0]. A "both" delivery makes
// two posts from two different renderers, and reading the first would leave
// whichever the transport happens to send second wholly unasserted.
func postedTexts(t *testing.T, sent []string) []string {
	t.Helper()
	if len(sent) == 0 {
		t.Fatal("nothing was posted at all")
	}
	out := make([]string, 0, len(sent))
	for _, body := range sent {
		out = append(out, postedThreadText(t, body))
	}
	return out
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
	for _, dest := range kbDestinations {
		t.Run(kbDestName(dest), func(t *testing.T) {
			sinks, sent := kbUpdateSinkOnFakeTransport(t)
			up := hostileKBUpdate("<!channel> <https://evil.example|https://github.com/acme/kb/pull/7>", "slack", dest)

			*sent = nil
			if err := sinks["slack"].DeliverKBUpdate(context.Background(), up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			for i, text := range postedTexts(t, *sent) {
				for _, live := range []string{"<!channel>", "<https://evil.example|"} {
					if strings.Contains(text, live) {
						t.Errorf("post %d: an untrusted field reached Slack as live mrkdwn (%q):\n%s", i, live, text)
					}
				}
				if !strings.Contains(text, "&lt;!channel&gt;") {
					t.Errorf("post %d: the untrusted text must still be READABLE, escaped rather than censored:\n%s", i, text)
				}
				if strings.Contains(text, "") {
					t.Errorf("post %d: an untrusted-span mark reached the chat system:\n%s", i, text)
				}
				// RunLore's own framing is not escaped with the untrusted spans: the
				// blockquote markers are what keep the quoted note distinguishable
				// from RunLore's own claims, and mrkdwnEscaper would turn them into
				// literal "&gt; ". Only the CHANNEL form quotes the note at all — the
				// in-thread form drops it — so the marker is required exactly where
				// the quote is and absent where it is not, which also pins that the
				// two forms did not quietly become one message again.
				if quoted := strings.Contains(text, "the registry PVC filled up"); quoted != strings.Contains(text, "\n> ") {
					t.Errorf("post %d: the quoted note and its blockquote markers must arrive together:\n%s", i, text)
				}
				for _, line := range strings.Split(text, "\n") {
					if strings.HasPrefix(line, "&gt;") {
						t.Errorf("post %d: RunLore's own blockquote marker was escaped, so the quote no longer renders as one: %q", i, line)
					}
				}
			}
		})
	}
}

// TestMatrixKBUpdateAnnouncementNeutralisesUntrustedFields is the same guarantee
// against Matrix's own hazard, across the same destinations. A plain m.notice
// body has no markup to inject, but .m.rule.roomnotif matches "@room" in
// content.body and notifies every member of the room — the direct analogue of
// Slack's <!channel>, reachable the same way, through text RunLore's model
// wrote. That push rule matches a THREADED m.notice exactly as it matches an
// unthreaded one, so the m.thread relation buys the room nothing.
func TestMatrixKBUpdateAnnouncementNeutralisesUntrustedFields(t *testing.T) {
	for _, dest := range kbDestinations {
		t.Run(kbDestName(dest), func(t *testing.T) {
			sinks, sent := kbUpdateSinkOnFakeTransport(t)
			up := hostileKBUpdate("@room now", "matrix", dest)

			*sent = nil
			if err := sinks["matrix"].DeliverKBUpdate(context.Background(), up); err != nil {
				t.Fatalf("DeliverKBUpdate: %v", err)
			}
			for i, body := range postedTexts(t, *sent) {
				if strings.Contains(body, "@room") {
					t.Errorf("post %d: an untrusted field reached the room as a live @room ping:\n%s", i, body)
				}
				if !strings.Contains(body, "@\u2060room") {
					t.Errorf("post %d: the token must be marked, not censored — a human still has to be able to read it:\n%s", i, body)
				}
				if strings.Contains(body, "") {
					t.Errorf("post %d: an untrusted-span mark reached the room:\n%s", i, body)
				}
			}
		})
	}
}

// TestAnnouncementProvenanceCannotForgeALine is the in-thread form's own version
// of the blockquote-escape defect below, aimed at the field that form still
// renders.
//
// kbThreadAnnouncement drops the title and the note quote, so the message is the
// headline plus "By <author> in a slack thread" — two lines, one of which
// interpolates an untrusted value at the LEFT MARGIN with no blockquote around
// it. thread.QuoteUntrusted, which is what makes a mandatory break harmless
// inside the note, never sees this field, and neither escaper touches a line
// separator. One U+2028 in an author therefore renders a third visual line the
// sender chose, directly under RunLore's real headline and indented identically
// — and the headline is precisely the line worth forging, because it is the one
// claiming a pull request exists.
//
// Every mandatory break is driven rather than U+2028 alone, for the reason
// thread.mandatoryBreaks exists: a list that learns one break at a time is wrong
// until the next report. The channel destination runs with it because the same
// field reaches the same margin there — the flattening closes both at once.
func TestAnnouncementProvenanceCannotForgeALine(t *testing.T) {
	const forged = "\U0001F4DA Knowledge base updated — opened PR #999 — https://evil.example/kb"

	sinks, sent := kbUpdateSinkOnFakeTransport(t)
	for _, br := range []struct{ name, sep string }{
		{"LF U+000A", "\n"},
		{"CR U+000D", "\r"},
		{"CRLF", "\r\n"},
		{"VT U+000B", "\v"},
		{"FF U+000C", "\f"},
		{"NEL U+0085", "\u0085"},
		{"LS U+2028", "\u2028"},
		{"PS U+2029", "\u2029"},
	} {
		for _, dest := range kbDestinations {
			t.Run(br.name+"/"+kbDestName(dest), func(t *testing.T) {
				up := providers.KBUpdate{
					Transport: "slack", Root: "111.222", Channel: "C-ORIGIN", Delivery: dest,
					Route: providers.KBRouteOpenPR, PR: 42, URL: "https://github.com/o/r/pull/42",
					Author: "alice" + br.sep + forged,
					Note:   "the registry PVC filled up",
				}
				*sent = nil
				if err := sinks["slack"].DeliverKBUpdate(context.Background(), up); err != nil {
					t.Fatalf("DeliverKBUpdate: %v", err)
				}
				for i, text := range postedTexts(t, *sent) {
					for n, line := range strings.Split(text, "\n") {
						if n == 0 {
							continue // RunLore's own headline, the only one allowed
						}
						if strings.HasPrefix(strings.TrimSpace(line), "\U0001F4DA") {
							t.Errorf("post %d: a %s in the author put a forged RunLore headline at the left margin:\n%q",
								i, br.name, text)
						}
					}
					// Flattened, not censored: a human must still be able to read
					// what the author field said, on the one line it is given.
					if !strings.Contains(text, "alice") || !strings.Contains(text, "opened PR #999") {
						t.Errorf("post %d: the author was censored rather than flattened:\n%q", i, text)
					}
				}
			})
		}
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
//
// The comment case additionally denies the "Knowledge base updated" claim, which
// it used to make: that note went onto the curator's draft, so the file the
// catalog gains is theirs and nothing of the note is in it until a human folds it
// in. See TestKBHeadlineClaimsTheCatalogGainedOnlyWhenItDid for the property; this
// pins it on the assembled channel message, which is what a reader actually sees.
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
			want: []string{"Knowledge base updated", "opened PR #99", "https://github.com/o/r/pull/99",
				"Operator note: OOM in payments", "sre-jane", "> the real cause was a spot reclaim", "slack"},
			deny: []string{"for review"},
		},
		{
			name: "comment names the pull request it joined",
			up: providers.KBUpdate{
				Transport: "matrix", Root: "$evt", Route: providers.KBRouteComment, PR: 42,
				URL: "https://github.com/o/r/pull/42", Author: "sre-jane", Note: "it recurs after node rotation",
			},
			want: []string{"for review on knowledge-base PR #42", "https://github.com/o/r/pull/42", "sre-jane",
				"> it recurs after node rotation", "matrix"},
			deny: []string{"opened", "Knowledge base updated"},
		},
		{
			name: "an unnumbered forge URL still announces the write",
			up: providers.KBUpdate{
				Transport: "slack", Route: providers.KBRouteOpenPR,
				URL: "https://github.com/o/r/kb", Note: "capture this",
			},
			want: []string{"Knowledge base updated", "opened PR", "https://github.com/o/r/kb", "> capture this"},
			deny: []string{"for review", "#0"},
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
// request PATH alongside the body, and returns them keyed by transport the way
// kbUpdateSinkOnFakeTransport does.
//
// The path is what carries the destination on Matrix — the room id is in the
// URL, not the payload — so a test that read only bodies could not tell an
// announcement delivered into the originating room from one delivered into the
// notifier's configured room, which is the entire distinction being tested.
func kbRoutingSinks(t *testing.T) (map[string]providers.KBUpdateNotifier, *[]kbPost) {
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
	mx := NewMatrix(srv.URL, "!configured-room:example.org", "tok")
	return map[string]providers.KBUpdateNotifier{"slack": bot, "matrix": mx}, &sent
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
		Author: "sre-jane", Note: "a spot reclaim", Title: "Operator note: OOM in payments",
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
		sink             string
		up               providers.KBUpdate
		wantRoot, wantTo string
	}{
		{
			sink:     "slack",
			up:       slackOrigin(providers.KBDeliverThread),
			wantRoot: "111.222", wantTo: "C-ORIGIN",
		},
		{
			sink:     "matrix",
			up:       matrixOrigin(providers.KBDeliverThread),
			wantRoot: "$evt-root", wantTo: "/_matrix/client/v3/rooms/!room-of-origin:example.org/send/m.room.message/",
		},
	} {
		t.Run(tc.sink, func(t *testing.T) {
			sinks, posts := kbRoutingSinks(t)
			if err := sinks[tc.sink].DeliverKBUpdate(context.Background(), tc.up); err != nil {
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
			text := postedThreadText(t, (*posts)[0].body)
			if !strings.Contains(text, "sre-jane") || !strings.Contains(text, "Knowledge base updated") {
				t.Errorf("the threaded announcement lost its own content (who wrote the note, what landed):\n%s", text)
			}
			// And it is the SHORT form. In the thread, the reply RunLore already
			// posted carries the entry title and quotes the note back; repeating
			// both here is the echo this feature exists to remove, moved closer to
			// its source rather than removed. Without these two assertions the
			// reduced renderer can be swapped for the full one with the suite green
			// — which is how the echo would come back.
			for _, unwanted := range []struct{ frag, why string }{
				{"Operator note: OOM in payments", "the entry title — the reply one message up already states it"},
				{"a spot reclaim", "the note itself — the reply already quotes it back to the person who typed it"},
			} {
				if strings.Contains(text, unwanted.frag) {
					t.Errorf("the in-thread announcement restates %s:\n%s", unwanted.why, text)
				}
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
		sink   string
		wantTo string
	}{
		{
			name:   "matrix sink, slack-born note",
			up:     slackOrigin(providers.KBDeliverThread),
			sink:   "matrix",
			wantTo: "/_matrix/client/v3/rooms/!configured-room:example.org/send/m.room.message/",
		},
		{
			name:   "slack sink, matrix-born note",
			up:     matrixOrigin(providers.KBDeliverThread),
			sink:   "slack",
			wantTo: "C-CONFIGURED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sinks, posts := kbRoutingSinks(t)
			if err := sinks[tc.sink].DeliverKBUpdate(context.Background(), tc.up); err != nil {
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
			sinks, posts := kbRoutingSinks(t)
			up := slackOrigin(providers.KBDeliverThread)
			up.Root, up.Channel = tc.root, tc.chann
			if err := sinks["slack"].DeliverKBUpdate(context.Background(), up); err != nil {
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
//
// Both transports are driven. They do not share an implementation — each
// DeliverKBUpdate composes its own two posts — so forcing the room half off when
// the thread half fires leaves the other transport's test green, and Matrix is
// the one where that mistake is easiest to make: its channel post is the
// original inline m.send while Slack's is a helper call.
func TestAnnouncementBothReachesTheThreadAndTheChannel(t *testing.T) {
	for _, tc := range []struct {
		sink                   string
		up                     providers.KBUpdate
		wantRoot               string
		threadDest, configured string
	}{
		{
			sink:       "slack",
			up:         slackOrigin(providers.KBDeliverBoth),
			wantRoot:   "111.222",
			threadDest: "C-ORIGIN",
			configured: "C-CONFIGURED",
		},
		{
			sink:       "matrix",
			up:         matrixOrigin(providers.KBDeliverBoth),
			wantRoot:   "$evt-root",
			threadDest: "/_matrix/client/v3/rooms/!room-of-origin:example.org/send/m.room.message/",
			configured: "/_matrix/client/v3/rooms/!configured-room:example.org/send/m.room.message/",
		},
	} {
		t.Run(tc.sink, func(t *testing.T) {
			sinks, posts := kbRoutingSinks(t)
			if err := sinks[tc.sink].DeliverKBUpdate(context.Background(), tc.up); err != nil {
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
			if got, ok := seen[tc.wantRoot]; !ok || !strings.HasPrefix(got, tc.threadDest) {
				t.Errorf("no announcement reached the originating thread (root→dest: %v)", seen)
			}
			if got, ok := seen[""]; !ok || !strings.HasPrefix(got, tc.configured) {
				t.Errorf("no unthreaded announcement reached the configured channel/room (root→dest: %v)", seen)
			}
		})
	}
}

// TestBothStillPostsToTheChannelWhenTheThreadPostFails pins that the two
// deliveries are independent. A wedged or renamed thread must not cost the
// broadcast a write that already landed on the forge, so the errors are JOINED
// rather than short-circuited.
//
// Matrix is driven alongside Slack, and it is the transport this matters most
// on: a thread root that has been redacted still exists as an event id, so the
// homeserver accepts the relation and the message renders detached — and a
// room whose power levels changed rejects the send outright. Its channel post is
// also the one written inline rather than through a helper, so a `return err`
// slipped into the thread half is easiest to miss exactly here.
func TestBothStillPostsToTheChannelWhenTheThreadPostFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		up       providers.KBUpdate
		failWhen string // substring of the request that identifies the THREAD post
		build    func(url string) providers.KBUpdateNotifier
	}{
		{
			name:     "slack",
			up:       slackOrigin(providers.KBDeliverBoth),
			failWhen: "thread_ts",
			build: func(url string) providers.KBUpdateNotifier {
				bot := NewSlackBot("xoxb-test", "C-CONFIGURED")
				bot.baseURL = url
				return bot
			},
		},
		{
			name:     "matrix",
			up:       matrixOrigin(providers.KBDeliverBoth),
			failWhen: "m.thread",
			build: func(url string) providers.KBUpdateNotifier {
				return NewMatrix(url, "!configured-room:example.org", "tok")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sent []kbPost
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				sent = append(sent, kbPost{path: r.URL.Path, body: string(body)})
				if strings.Contains(string(body), tc.failWhen) {
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"ok":false,"error":"thread_not_found"}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true,"ts":"1","event_id":"$1"}`))
			}))
			defer srv.Close()

			if err := tc.build(srv.URL).DeliverKBUpdate(context.Background(), tc.up); err == nil {
				t.Error("a failed thread post must be reported, not swallowed here — the announcer is what decides to swallow it")
			}
			if len(sent) != 2 {
				t.Fatalf("posted %d messages, want 2 — the channel half must survive the thread half failing", len(sent))
			}
			if root, _ := sent[1].threadedTo(t); root != "" {
				t.Errorf("the second post is threaded (%q); the channel half never was", root)
			}
		})
	}
}

// TestKBUpdateFieldsAreAllRenderedOrDeliberatelyNot keeps the renderers honest
// about the event they are given. providers.KBUpdate has a reflection test
// forcing every field to be classified trusted or untrusted; this is the
// notifier's side of it — a field added to the event must be either shown to the
// operator or listed here as deliberately withheld, so "we forgot to render it"
// cannot pass as "we chose not to".
//
// BOTH renderings are walked, each against its own list. There are two messages
// now — the channel form and the shorter in-thread form — and this used to
// inspect one: it set Delivery to "both" but on a matrix-born event delivered
// through the SLACK sink, which is not the originating transport, so the thread
// route was never taken and only the channel post was ever read. It looked
// stronger than it was.
//
// The in-thread list is also what pins the reduced form itself. Title and Note
// are WITHHELD there, deliberately: the reply to the person who typed is sitting
// directly above it carrying both, and repeating them is the echo this
// destination exists to remove. If someone points the thread post back at
// kbUpdateAnnouncement, those two entries fail — which is the guard the
// destination's whole justification rests on.
func TestKBUpdateFieldsAreAllRenderedOrDeliberatelyNot(t *testing.T) {
	up := providers.KBUpdate{
		Transport: "matrix", Root: "$evt-root", Channel: "!room-of-origin:example.org",
		Delivery: providers.KBDeliverBoth, Route: providers.KBRouteOpenPR, PR: 4242,
		URL: "https://github.com/o/r/pull/4242", Title: "Operator note: OOM",
		Author: "sre-jane", ModelDrafted: true, Note: "a spot reclaim",
		At: time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	}
	// Values a list may search for, one per field, so an entry cannot be written
	// against a value the fixture does not carry.
	value := map[string]string{
		"Transport": "matrix",
		"Route":     "opened PR",
		"PR":        "4242",
		"URL":       "https://github.com/o/r/pull/4242",
		"Title":     "Operator note: OOM",
		"Author":    "sre-jane",
		// Set true on the fixture so there is a string to search for. A boolean
		// field renders as WORDING rather than as its own value, which is exactly
		// why it needs to be in this guard: a renderer can drop it without dropping
		// anything a reader would notice was missing.
		"ModelDrafted": "Drafted by RunLore",
		"Note":         "a spot reclaim",
		"Root":         "$evt-root",
		"Channel":      "!room-of-origin:example.org",
		"Delivery":     "both",
		"At":           "2026-08-16T09:00:00Z",
	}

	for _, form := range []struct {
		name   string
		render func(providers.KBUpdate) string
		shows  []string
	}{
		{
			// The CHANNEL form is the only thing anyone in that channel sees about
			// the write, so it carries the entry name and quotes the note.
			name: "channel", render: kbUpdateAnnouncement,
			shows: []string{"Transport", "Route", "PR", "URL", "Title", "Author", "ModelDrafted", "Note"},
		},
		{
			// The IN-THREAD form drops Title and Note: the reply above it already
			// carries the same entry name and the same quote of the same note.
			// What is left is exactly the delta the reply lacks.
			// ModelDrafted is shown HERE too, unlike Title and Note: the reply above
			// does say the note was drafted, but this message can arrive first (the
			// announcement is scheduled before the reply is posted) and can be the
			// only one in the thread if the best-effort reply fails.
			name: "thread", render: kbThreadAnnouncement,
			shows: []string{"Transport", "Route", "PR", "URL", "Author", "ModelDrafted"},
		},
	} {
		t.Run(form.name, func(t *testing.T) {
			shown := map[string]bool{}
			for _, name := range form.shows {
				shown[name] = true
			}
			// Rendered through the real transport rather than read off the composer,
			// so what is asserted is what goes on the wire.
			text := thread.RenderReply(form.render(up), escapeMrkdwn)

			rt := reflect.TypeOf(up)
			for i := range rt.NumField() {
				name := rt.Field(i).Name
				want, ok := value[name]
				if !ok {
					t.Errorf("KBUpdate.%s has no entry in the value map — add one so both forms can be "+
						"checked for it", name)
					continue
				}
				switch {
				case shown[name] && !strings.Contains(text, want):
					t.Errorf("the %s announcement must show KBUpdate.%s as %q, and does not:\n%s",
						form.name, name, want, text)
				case !shown[name] && strings.Contains(text, want):
					t.Errorf("the %s announcement renders KBUpdate.%s, which it is listed as withholding — "+
						"update the list or stop rendering it:\n%s", form.name, name, text)
				}
			}
			// The reverse direction: a `shows` entry naming a field KBUpdate no
			// longer has asserts nothing about anything, and the loop above only
			// walks the STRUCT, so a stale name would sit there reading in review as
			// a decision someone made.
			for _, name := range form.shows {
				if _, ok := rt.FieldByName(name); !ok {
					t.Errorf("the %s form claims to show %q, but KBUpdate has no such field — stale entry",
						form.name, name)
				}
			}
		})
	}

	// The value map is checked the same way, for the same reason.
	rt := reflect.TypeOf(up)
	for name := range value {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("the value map classifies %q, but KBUpdate has no such field — stale entry, "+
				"so nothing is checking whatever replaced it", name)
		}
	}
}

// TestKBHeadlineNamesEachRouteDistinctly: the headline is RunLore's own claim
// about what it did, and the three routes did materially different things. A
// reader in the notifier's channel is deciding whether the knowledge is safely
// captured, and only the append and open_pr routes put it in an entry — a
// comment is a remark a human still has to fold in. Reusing one word for all
// three would answer that question wrongly for two of them.
func TestKBHeadlineNamesEachRouteDistinctly(t *testing.T) {
	want := map[providers.KBRoute]string{
		providers.KBRouteComment: "📝 Note left for review on knowledge-base PR #7",
		providers.KBRouteOpenPR:  "📚 Knowledge base updated — opened PR #7",
		providers.KBRouteAppend:  "📚 Knowledge base updated — added to the entry on PR #7",
	}
	seen := map[string]providers.KBRoute{}
	for route, head := range want {
		got := kbHeadline(providers.KBUpdate{Route: route, PR: 7})
		if got != head {
			t.Errorf("kbHeadline(%q) = %q, want %q", route, got, head)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("routes %q and %q announce identically (%q) — a reader cannot tell them apart", route, other, got)
		}
		seen[got] = route
	}
}

// TestKBHeadlineClaimsTheCatalogGainedOnlyWhenItDid is the honesty half, and it
// is a property rather than a fixture: the routes are partitioned by whether
// what they wrote is INSIDE the entry the catalog gains on merge, and only that
// side may assert the knowledge base was updated.
//
// KBRouteAppend's own doc draws exactly that line — it exists as a route
// separate from KBRouteComment because "what this route writes is inside the
// entry the catalog gains when the PR merges, and what a comment writes is not".
// kbHeadline nonetheless prefixed all three identically and differentiated only
// the verb, so the comment route announced "📚 Knowledge base updated — noted on
// PR #7" for a write that changed no entry at all: the merged file is the
// curator's draft, and the note is feedback a human still has to fold in. It
// matters most on the in-thread form, which is the headline and one other line.
func TestKBHeadlineClaimsTheCatalogGainedOnlyWhenItDid(t *testing.T) {
	const claim = "Knowledge base updated"
	for _, tt := range []struct {
		route     providers.KBRoute
		inTheFile bool
	}{
		{providers.KBRouteOpenPR, true},
		{providers.KBRouteAppend, true},
		{providers.KBRouteComment, false},
		// An unknown route is a route this renderer has not learned about, so it
		// must not assert anything about the catalog. kbRoute passes an unrecognised
		// value straight through rather than guessing, which is only safe if the
		// default here fails toward the weaker claim.
		{providers.KBRoute("some-future-route"), false},
	} {
		t.Run(string(tt.route), func(t *testing.T) {
			got := kbHeadline(providers.KBUpdate{Route: tt.route, PR: 7})
			if asserts := strings.Contains(got, claim); asserts != tt.inTheFile {
				t.Errorf("kbHeadline(%q) = %q; claims %q = %v, want %v — "+
					"only a route that writes inside the entry the catalog gains may say so",
					tt.route, got, claim, asserts, tt.inTheFile)
			}
			if !strings.Contains(got, "#7") {
				t.Errorf("kbHeadline(%q) = %q, want it to name the pull request", tt.route, got)
			}
		})
	}
}

// TestKBHeadlineLeadsWithARunLoreStatusGlyph keeps the comment route's new
// wording inside the protection the old one had for free. Every headline is
// RunLore's own claim and sits at the left margin, and the defence against
// untrusted text posing as one of those lines is thread.QuoteUntrusted stripping
// these glyphs out of quoted note text — which only works for glyphs the strip
// list knows.
func TestKBHeadlineLeadsWithARunLoreStatusGlyph(t *testing.T) {
	for _, route := range []providers.KBRoute{
		providers.KBRouteOpenPR, providers.KBRouteAppend, providers.KBRouteComment, providers.KBRoute("unknown"),
	} {
		head := kbHeadline(providers.KBUpdate{Route: route, PR: 7})
		if stripped := thread.QuoteUntrusted(head); strings.Contains(stripped, strings.TrimSpace(head[:4])) {
			t.Errorf("kbHeadline(%q) = %q leads with a glyph thread.QuoteUntrusted does not strip, so a "+
				"note quoting this line back would forge a RunLore claim", route, head)
		}
	}
}

// TestKBProvenanceLineNeverFilesModelProseUnderAHumansName is the announcement's
// half of a distinction every other surface already made.
//
// "By sre-jane in a slack thread" is a claim about who wrote the note, and on
// the freeform route it was false: RunLore's chat model wrote that text from
// sre-jane's message, which is precisely why the thread reply has openedWith,
// the entry body says "@sre-jane did not write it", and the entry description
// leads with the provenance clause so a clipped listing cannot lose it. This
// line said the opposite of all three.
//
// It matters most in the reduced in-thread form, where the message is the
// headline and this line and nothing else — and where, if the best-effort reply
// fails, this is the only thing in the thread.
func TestKBProvenanceLineNeverFilesModelProseUnderAHumansName(t *testing.T) {
	for _, tt := range []struct {
		name string
		up   providers.KBUpdate
		deny []string
		want []string
	}{
		{
			name: "the human typed it",
			up:   providers.KBUpdate{Transport: "slack", Author: "sre-jane"},
			want: []string{"By ", "sre-jane", "slack"},
			deny: []string{"RunLore"},
		},
		{
			name: "RunLore's model drafted it",
			up:   providers.KBUpdate{Transport: "slack", Author: "sre-jane", ModelDrafted: true},
			// Still names the human — they are whose message produced it, and a
			// reader needs that to follow it up — but never as the author.
			want: []string{"sre-jane", "slack", "RunLore", "not their own words"},
			deny: []string{"By "},
		},
		{
			name: "drafted, with no author reported",
			up:   providers.KBUpdate{Transport: "slack", ModelDrafted: true},
			want: []string{"RunLore", "slack"},
		},
		{
			name: "drafted, with nothing else known",
			up:   providers.KBUpdate{ModelDrafted: true},
			// The provenance line is optional when there is nothing to say; it is
			// NOT optional when what there is to say is that a human did not write
			// this. An empty line here would drop the one fact that must travel.
			want: []string{"RunLore"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := kbProvenanceLine(tt.up)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("kbProvenanceLine = %q, want it to contain %q", got, w)
				}
			}
			for _, d := range tt.deny {
				if strings.Contains(got, d) {
					t.Errorf("kbProvenanceLine = %q, must not contain %q", got, d)
				}
			}
		})
	}
}

// TestAnnouncementFormsBothCarryTheDraftedProvenance pins it on the assembled
// messages, in both destinations, because the field being set is worth nothing
// if a renderer drops it — and the two forms are separate functions, so the one
// that gets fixed alone is the one that stops being tested.
func TestAnnouncementFormsBothCarryTheDraftedProvenance(t *testing.T) {
	up := providers.KBUpdate{
		Transport: "slack", Root: "111.222", Channel: "C1", Route: providers.KBRouteOpenPR, PR: 7,
		URL: "https://github.com/o/r/pull/7", Title: "Operator note: OOM",
		Author: "sre-jane", Note: "It was a spot reclaim.", ModelDrafted: true,
	}
	for name, render := range map[string]func(providers.KBUpdate) string{
		"channel": kbUpdateAnnouncement,
		"thread":  kbThreadAnnouncement,
	} {
		t.Run(name, func(t *testing.T) {
			text := thread.RenderReply(render(up), escapeMrkdwn)
			if !strings.Contains(text, "RunLore") || !strings.Contains(text, "not their own words") {
				t.Errorf("the %s announcement presents model-drafted prose as sre-jane's own words:\n%s", name, text)
			}
		})
	}
}
