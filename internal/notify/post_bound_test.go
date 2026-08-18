// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/providers"
)

// The ceilings below are written as LITERALS on purpose, and every assertion in
// this file states the number rather than reading it back out of the constant it
// is meant to be checking.
//
// A previous round of this work found every byte ceiling in the package asserted
// against itself: the test computed its budget from slackReplyBytes, so the
// constant could be tripled and a 50 KB post went on the wire with the suite
// green. A bound whose test moves with it is a bound with no test at all.

// TestMatrixDeliverBodyCeilingHasItsDocumentedValue pins the NUMBER, and pins the
// derivation that produced it.
//
// Matrix.Deliver is the one post in this package that puts the SAME message on
// an event twice — once as the plaintext body, once as the HTML formatted_body —
// so the share a single-body reply may claim (matrixReplyBytes, half the event)
// has to be split between them. Raising this past a quarter of the event puts
// the two renderings plus the signatures, hashes and relations over the spec's
// hard 65,536-byte ceiling, which is a REJECTED event: the on-call is told
// nothing at all about an investigation that completed.
func TestMatrixDeliverBodyCeilingHasItsDocumentedValue(t *testing.T) {
	if got, want := matrixDeliverBodyBytes, 16<<10; got != want {
		t.Errorf("matrixDeliverBodyBytes = %d, want %d — the Matrix spec caps a whole event at "+
			"65,536 bytes including signatures, hashes and relations, and this event carries the "+
			"message TWICE (body + formatted_body), so each rendering may claim a quarter rather "+
			"than the half a single-body reply claims", got, want)
	}
	if both, half := 2*matrixDeliverBodyBytes, 65_536/2; both > half {
		t.Errorf("the two renderings together claim %d bytes, over the %d bytes one message may take "+
			"of a 65,536-byte event — the rest is the envelope: signatures, hashes, relations and "+
			"the trigger/thread stamps Deliver adds", both, half)
	}
}

// hugeProgressUpdate is an interim ping whose model text is far past any
// transport's budget. model.chat/model max_tokens is operator-configurable and
// nothing upstream caps ProgressUpdate.Interim — the loop passes the completion's
// raw text through redact.Secrets and no further (internal/investigate/loop.go
// emitProgress) — so this is a shape the running system produces, not a
// contrivance.
func hugeProgressUpdate() providers.ProgressUpdate {
	return providers.ProgressUpdate{
		Title:     "HarborDown",
		Step:      3,
		MaxSteps:  20,
		ToolsUsed: map[string]int{"kb_search": 1},
		Interim: strings.TrimSuffix(strings.Repeat(
			"the harbor-db pod is CrashLoopBackOff and the migration lock is still held\n", 3000), "\n"),
	}
}

// TestSlackProgressTextIsBoundedAtTheTransport is the guard on an unbounded
// model-derived string reaching chat.postMessage's "text".
//
// slackProgressBlocks bounds every block it emits (150 runes for the header,
// 2900 for the section). The "text" field beside them was bounded by nothing at
// all: it is escapeMrkdwn(FormatProgress(up)), and FormatProgress splices in the
// model's raw interim text. Over Slack's ~40,000-character limit the whole POST
// is rejected — so an operator who turned progress pings ON to stop a long
// investigation going silent gets exactly the silence they were avoiding, on
// precisely the long investigations that produce the most interim text.
//
// What survives a cut is the HEAD: line one is the status line ("Investigating:
// <title> — step 3/20"), which says which incident this is and how far along it
// got. That is the whole content of a progress ping. The model's interim
// rambling is what the reader can do without, and it sits at the end.
func TestSlackProgressTextIsBoundedAtTheTransport(t *testing.T) {
	up := hugeProgressUpdate()
	text, _ := slackProgressMessage(up)["text"].(string)

	if len(text) > 35_000 {
		t.Errorf("posted %d bytes in chat.postMessage's text, over the 35,000-byte budget — Slack "+
			"documents ~40,000 characters for that field and rejects the whole post beyond it, so "+
			"the progress ping an operator opted into never arrives", len(text))
	}
	if !strings.HasPrefix(text, "Investigating: HarborDown — step 3/20") {
		t.Errorf("the status headline did not survive the cut — it is the whole point of a progress "+
			"ping (which incident, how far along), and it is at the HEAD, so the head is what a "+
			"bound must keep:\n%.200s", text)
	}
	if !strings.Contains(text, "too long") {
		t.Errorf("a shortened post must say it was shortened; a reader who cannot tell reads what "+
			"survived as the whole of it. Got:\n%.300s", text)
	}

	// Whole lines, never part of one. What survived has to be a LINE-ALIGNED
	// prefix of the escaped message: a cut through the middle of a line leaves a
	// fragment of model prose butted against RunLore's own status framing, and
	// leaves it there with no marker saying where the model's words stopped.
	full := escapeMrkdwn(FormatProgress(up))
	kept, found := strings.CutSuffix(text, "\n"+postTruncatedNotice)
	if !found {
		t.Fatalf("the truncated post does not end in the truncation notice:\n%.300s", text)
	}
	if !strings.HasPrefix(full, kept) {
		t.Fatalf("what survived is not a prefix of the escaped message — the bound kept the wrong end")
	}
	if rest := full[len(kept):]; rest != "" && !strings.HasPrefix(rest, "\n") {
		t.Errorf("the cut landed inside a line: %q was dropped from the middle of one, leaving a "+
			"fragment of model prose at the left margin where RunLore's own status lines sit",
			rest[:min(len(rest), 60)])
	}
}

// TestSlackProgressTextIsBoundedAfterEscapingNotBefore pins WHERE the bound is
// applied, which is the half that is easy to get wrong and impossible to see.
//
// escapeMrkdwn expands: every "&" becomes "&amp;", five bytes for one. A ping
// measured BEFORE escaping can pass a byte check and still arrive at Slack five
// times over its budget — the bound would be real, tested, and bounding a string
// that never goes on the wire.
func TestSlackProgressTextIsBoundedAfterEscapingNotBefore(t *testing.T) {
	up := providers.ProgressUpdate{Title: "HarborDown", Step: 3, MaxSteps: 20,
		Interim: strings.Repeat("&", 20_000)}
	if raw := FormatProgress(up); len(raw) > 35_000 {
		t.Fatalf("test setup: the unescaped ping (%d bytes) must fit the 35,000-byte budget so only "+
			"the escaped size can trip the bound", len(raw))
	}

	text, _ := slackProgressMessage(up)["text"].(string)
	if len(text) > 35_000 {
		t.Errorf("posted %d escaped bytes over a 35,000-byte budget: the bound was applied to the "+
			"ping as composed, not to what actually goes on the wire", len(text))
	}
	if !strings.HasPrefix(text, "Investigating: HarborDown — step 3/20") {
		t.Errorf("the status headline did not survive:\n%.200s", text)
	}
}

// TestSlackProgressUnderBudgetIsPostedUnchanged is the other half of the bound:
// the ordinary ping — every ping, in practice — must reach the channel exactly
// as composed, with no truncation notice bolted onto it.
func TestSlackProgressUnderBudgetIsPostedUnchanged(t *testing.T) {
	up := providers.ProgressUpdate{Title: "HarborDown", Step: 5, MaxSteps: 20,
		ToolsUsed: map[string]int{"kb_search": 1}, Interim: "checking the harbor-db pod"}
	want := escapeMrkdwn(FormatProgress(up))
	if got, _ := slackProgressMessage(up)["text"].(string); got != want {
		t.Errorf("a ping within budget was altered:\ngot  %q\nwant %q", got, want)
	}
}

// TestSlackProgressBoundsOneUnbrokenLine covers the case dropping whole lines
// cannot fix: a status line whose untrusted title is on its own longer than the
// whole budget. Its head is kept — a cut on a rune boundary, so no invalid UTF-8
// reaches the wire, which would be a rejected post arrived at from the other
// side.
func TestSlackProgressBoundsOneUnbrokenLine(t *testing.T) {
	up := providers.ProgressUpdate{Title: strings.Repeat("é", 100_000), Step: 1, MaxSteps: 20}
	text, _ := slackProgressMessage(up)["text"].(string)

	if len(text) > 35_000 {
		t.Errorf("posted %d bytes over a 35,000-byte budget", len(text))
	}
	if !utf8.ValidString(text) {
		t.Error("the cut split a rune: invalid UTF-8 is a post Slack rejects outright")
	}
	if !strings.HasPrefix(text, "Investigating: é") {
		t.Errorf("the head of the status line was not kept:\n%.80q", text)
	}
	if !strings.Contains(text, "too long") {
		t.Errorf("a cut-off post must say it was cut off; got:\n%.200s", text)
	}
}

// deliveredMatrixEvent runs a real Matrix.Deliver against a fake homeserver and
// returns the event content that went on the wire — the only place the two
// bodies can be checked as the transport receives them.
func deliveredMatrixEvent(t *testing.T, inv providers.Investigation) (body, formatted string) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"event_id":"$1"}`))
	}))
	t.Cleanup(srv.Close)

	if err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got == nil {
		t.Fatal("no event reached the homeserver")
	}
	body, _ = got["body"].(string)
	formatted, _ = got["formatted_body"].(string)
	return body, formatted
}

// hugeInvestigation is a finding whose model-authored text is far past any
// event's budget. buildInvestigation (internal/investigate/tools.go) copies every
// string out of submit_findings verbatim — no field is length-capped and no list
// is count-capped — so the size of this message is whatever the model wrote.
// The "&" and angle brackets are there because they are the widest escapes both
// renderings apply, and they are exactly what model prose quoting a log line
// carries.
func hugeInvestigation() providers.Investigation {
	inv := sampleInvestigation()
	for i := range 400 {
		inv.RootCauses = append(inv.RootCauses, providers.Hypothesis{
			Summary:    fmt.Sprintf("hypothesis %d: harbor-core & harbor-db disagree on the <schema> version", i),
			Confidence: 0.4,
			Evidence:   []string{strings.Repeat("log line quoting a & and a <tag> from the pod ", 5)},
		})
	}
	return inv
}

// TestMatrixDeliverBodiesAreBounded is the guard on the second unbounded post:
// Matrix.Deliver splices Format(inv) — the model's title, every hypothesis, every
// evidence line — into an event body with no ceiling anywhere between the model
// and the homeserver.
//
// A Matrix event is capped at 65,536 bytes by the spec, signatures and hashes
// included, and a homeserver rejects the whole event past it. That is a
// completed investigation nobody is told about: the loop ran, spent the model
// budget, and the on-call sees nothing.
//
// The HEAD is what survives, and that follows from Format's own documented
// ordering: title, verdict, confidence, then the top root cause — the answer the
// reader came for is at the top, and the provenance footer (verified / KB link /
// token cost) is at the bottom. Cutting the other end would drop the verdict and
// keep the cost line.
func TestMatrixDeliverBodiesAreBounded(t *testing.T) {
	body, formatted := deliveredMatrixEvent(t, hugeInvestigation())

	for name, s := range map[string]string{"body": body, "formatted_body": formatted} {
		if len(s) > 16<<10 {
			t.Errorf("%s is %d bytes, over the 16,384-byte ceiling — the two renderings plus the "+
				"envelope must fit the spec's 65,536-byte event, and a homeserver rejects the whole "+
				"event beyond it", name, len(s))
		}
		if !utf8.ValidString(s) {
			t.Errorf("%s is not valid UTF-8: the cut split a rune, which is a rejected event arrived "+
				"at from the other side", name)
		}
		if !strings.Contains(s, "too long") {
			t.Errorf("%s does not say it was shortened; a reader who cannot tell reads what survived "+
				"as the whole card:\n%.300s", name, s)
		}
		// The head — verdict and top root cause — is what a cut must keep.
		for _, must := range []string{"Action required", "chart 1.15 enabled DB migrations"} {
			if !strings.Contains(s, must) {
				t.Errorf("%s lost %q: the verdict and the top root cause are the answer, and Format "+
					"puts them at the HEAD, so the head is the end a bound keeps:\n%.300s", name, must, s)
			}
		}
	}
	if both := len(body) + len(formatted); both > 65_536/2 {
		t.Errorf("the two bodies total %d bytes, over the %d one message may take of a 65,536-byte "+
			"event — the rest is signatures, hashes, relations and the stamps Deliver adds", both, 65_536/2)
	}
}

// TestMatrixDeliverBodiesStayConsistent is the requirement that makes this post
// different from every other one in the package: it carries the message TWICE.
//
// body and formatted_body are two renderings of one message, and a Matrix client
// shows whichever it supports. Bounding them independently cuts them at
// different points — the HTML rendering is much the larger, so it would lose
// more — and the card then says different things to two people in the same room,
// with nothing on either copy to say so. So the cut is chosen once, in the
// SOURCE, and both bodies are rendered from the same kept prefix.
func TestMatrixDeliverBodiesStayConsistent(t *testing.T) {
	body, formatted := deliveredMatrixEvent(t, hugeInvestigation())

	// mrkdwnToHTML renders every "\n" as one "<br/>" and html-escapes the rest, so
	// a "<br/>" in the output can only have come from a newline in the source: the
	// two counts match exactly when both bodies were cut at the same line.
	if got, want := strings.Count(formatted, "<br/>"), strings.Count(body, "\n"); got != want {
		t.Errorf("formatted_body has %d line breaks, body has %d — the two renderings were cut at "+
			"different points, so a reader on an HTML client and a reader on a plaintext client see "+
			"different cards with nothing saying so", got, want)
	}
	if !strings.HasSuffix(body, postTruncatedNotice) || !strings.HasSuffix(formatted, postTruncatedNotice) {
		t.Errorf("both bodies must end in the same truncation notice:\nbody      …%.80q\nformatted …%.80q",
			lastBytes(body, 80), lastBytes(formatted, 80))
	}
}

// TestMatrixDeliverHTMLCutDoesNotSplitATag is the other half of "two bodies":
// one of them is markup, and a byte cut through markup is malformed output.
//
// A cut inside "<strong>" leaves an unterminated tag; a cut inside "&amp;"
// leaves a dangling entity; a cut inside "<br/>" leaves both. Matrix clients
// sanitise formatted_body, and what a sanitiser does with a truncated tag is
// client-specific — anything from swallowing the rest of the card to rendering
// the raw markup at the reader.
//
// Cutting whole SOURCE lines is what makes this impossible rather than unlikely:
// mrkdwnToHTML's markup never spans a newline (boldRe and codeRe both exclude
// "\n"), so a whole-line prefix converts to exactly the complete tags the full
// message would have produced for those lines.
func TestMatrixDeliverHTMLCutDoesNotSplitATag(t *testing.T) {
	_, formatted := deliveredMatrixEvent(t, hugeInvestigation())

	if openAt, closeAt := strings.LastIndex(formatted, "<"), strings.LastIndex(formatted, ">"); openAt > closeAt {
		t.Errorf("formatted_body ends inside an unterminated tag: %q", formatted[openAt:])
	}
	for _, tag := range []string{"strong", "code"} {
		opened := strings.Count(formatted, "<"+tag+">")
		closed := strings.Count(formatted, "</"+tag+">")
		if opened != closed {
			t.Errorf("formatted_body has %d <%s> and %d </%s>: the cut split a tag pair", opened, tag, closed, tag)
		}
	}
	// html.EscapeString escapes every "&", so each one in the output opens an
	// entity — and each must be closed.
	if amp := strings.LastIndex(formatted, "&"); amp >= 0 && !strings.Contains(formatted[amp:], ";") {
		t.Errorf("formatted_body ends inside an unterminated HTML entity: %q", formatted[amp:])
	}
}

// TestMatrixDeliverUnderBudgetIsUnchanged is the ordinary card — every card, in
// practice. Both bodies must be byte-identical to what the notifier composed
// before the bound existed.
func TestMatrixDeliverUnderBudgetIsUnchanged(t *testing.T) {
	inv := sampleInvestigation()
	body, formatted := deliveredMatrixEvent(t, inv)

	msg := Format(inv)
	if want := escapeMatrixReply(plainFallback(msg)); body != want {
		t.Errorf("a card within budget had its plaintext body altered:\ngot  %q\nwant %q", body, want)
	}
	if want := mrkdwnToHTML(msg); formatted != want {
		t.Errorf("a card within budget had its formatted body altered:\ngot  %q\nwant %q", formatted, want)
	}
}

// TestMatrixDeliverBoundsOneUnbrokenLine covers the case dropping whole lines
// cannot fix: a model title with no newline in it, longer on its own than the
// budget. It is written entirely in "&" because that is the widest expansion the
// HTML rendering applies — five bytes out of one — which is precisely what makes
// a bound measured on the SOURCE useless here.
func TestMatrixDeliverBoundsOneUnbrokenLine(t *testing.T) {
	inv := sampleInvestigation()
	inv.Title = strings.Repeat("&", 100_000)
	body, formatted := deliveredMatrixEvent(t, inv)

	for name, s := range map[string]string{"body": body, "formatted_body": formatted} {
		if len(s) > 16<<10 {
			t.Errorf("%s is %d bytes over a 16,384-byte ceiling: the bound was measured on the "+
				"source, not on what the homeserver receives", name, len(s))
		}
		if !utf8.ValidString(s) {
			t.Errorf("%s is not valid UTF-8: the cut split a rune", name)
		}
		if !strings.Contains(s, "too long") {
			t.Errorf("%s does not say it was cut off:\n%.200s", name, s)
		}
	}
	if amp := strings.LastIndex(formatted, "&"); amp >= 0 && !strings.Contains(formatted[amp:], ";") {
		t.Errorf("formatted_body ends inside an unterminated HTML entity: %q", formatted[amp:])
	}
}

// TestPostBoundKeepsTheHeadWhereTheReplyBoundKeepsTheTail states the one design
// decision this file exists for, as an executable claim rather than a comment.
//
// A thread reply and a posted card are cut at OPPOSITE ends, and neither
// direction generalises to the other. In a reply, the record of what RunLore did
// ("Opened PR #99 …", the note quoted back) is composed LAST, and it is the part
// a human cannot reconstruct from anywhere else — so boundPostedReply drops the
// front. In a progress ping and a delivered finding, the headline and the verdict
// come FIRST and the provenance footer comes last — so boundPostedHead drops the
// back. Cutting the wrong end loses exactly the part the reader needed, in both
// directions, which is why this is two functions and not one with a flag.
func TestPostBoundKeepsTheHeadWhereTheReplyBoundKeepsTheTail(t *testing.T) {
	const maxBytes = 400
	const head = "🔍 the verdict and the answer"
	const tail = "📝 Noted on the knowledge-base PR #42 — https://github.com/o/r/pull/42"
	msg := head + "\n" + strings.Repeat("a padding line that is neither the head nor the tail\n", 20) + tail

	if got := boundPostedHead(msg, maxBytes); !strings.Contains(got, head) || strings.Contains(got, tail) {
		t.Errorf("boundPostedHead must keep the head and drop the tail — a card's verdict is at the "+
			"top and its cost footer at the bottom. Got:\n%s", got)
	}
	if got := boundPostedReply(msg, maxBytes); strings.Contains(got, head) || !strings.Contains(got, tail) {
		t.Errorf("boundPostedReply must keep the tail and drop the head — a reply's record of what "+
			"happened is at the end. Got:\n%s", got)
	}
	for name, got := range map[string]string{
		"boundPostedHead":  boundPostedHead(msg, maxBytes),
		"boundPostedReply": boundPostedReply(msg, maxBytes),
	} {
		if len(got) > maxBytes {
			t.Errorf("%s returned %d bytes over its %d-byte ceiling", name, len(got), maxBytes)
		}
	}
}

// lastBytes returns the final n bytes of s on a rune boundary, for error output.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}
