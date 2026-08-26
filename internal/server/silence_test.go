// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/slackcard"
	"github.com/Smana/runlore/internal/thread"
)

type recordedSilence struct {
	key    string
	window time.Duration
	user   string
}

type recordSilence struct {
	got []recordedSilence
	err error
}

func (r *recordSilence) Silence(key string, window time.Duration, user string, _ time.Time) error {
	if r.err != nil {
		return r.err
	}
	r.got = append(r.got, recordedSilence{key: key, window: window, user: user})
	return nil
}

// sendSilence posts a signed menu-selection interaction. Note the payload shape:
// the 🔕 menu carries its choice in selected_option.value, NOT in value, and the
// TriggerKey rides in the action's block_id.
func sendSilence(t *testing.T, srv *Server, secret, blockID, value string) *httptest.ResponseRecorder {
	t.Helper()
	return postSignedInteraction(t, srv, secret,
		`{"user":{"id":"U9","username":"bob"},"actions":[{"action_id":"`+slackcard.SilenceActionID+`",`+
			`"block_id":`+quoteJSON(t, blockID)+`,"selected_option":{"value":`+quoteJSON(t, value)+`}}]}`)
}

// quoteJSON renders v as a JSON string literal, quotes and all, so fixtures stay
// correct by construction rather than by every caller avoiding quotes.
func quoteJSON(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("quote %q: %v", v, err)
	}
	return string(b)
}

// postSignedInteraction form-encodes payload, signs it the way Slack does, and
// serves it. Every sender in this file shares it so the signing boilerplate has
// one copy: a change to how an interaction is authenticated lands in one place.
func postSignedInteraction(t *testing.T, srv *Server, secret, payload string) *httptest.ResponseRecorder {
	t.Helper()
	body := "payload=" + url.QueryEscape(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSlackSilenceInteraction(t *testing.T) {
	rec := &recordSilence{}
	const secret = "shh"
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)

	if rr := sendSilence(t, srv, secret, "sil:ns/app:CrashLoop", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	want := recordedSilence{key: "ns/app:CrashLoop", window: 4 * time.Hour, user: "U9"}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Fatalf("recorded = %+v, want exactly one %+v", rec.got, want)
	}
}

// TestSlackSilenceMalformedPayloadsRecordNothing: every rejected shape must be
// acked (200, so Slack does not retry) and must record nothing.
func TestSlackSilenceMalformedPayloadsRecordNothing(t *testing.T) {
	const secret = "shh"
	for _, tc := range []struct {
		name    string
		blockID string
		value   string
	}{
		{"block_id without the sil: prefix", "ns/app:CrashLoop", "4h"},
		{"empty key after the prefix", "sil:", "4h"},
		{"a value that is not a duration", "sil:k", "forever"},
		{"an empty value", "sil:k", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordSilence{}
			srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
			if rr := sendSilence(t, srv, secret, tc.blockID, tc.value); rr.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (acked, not retried)", rr.Code)
			}
			if len(rec.got) != 0 {
				t.Errorf("recorded %+v, want nothing", rec.got)
			}
		})
	}
}

// TestSlackSilenceDisabledIsAckedNotFatal: silencing off must ack the click
// rather than 404, panic, or silently succeed.
func TestSlackSilenceDisabledIsAckedNotFatal(t *testing.T) {
	const secret = "shh"
	// A feedback-only server: the endpoint is up, but s.silence is nil.
	srv := New(nil, Actions{Feedback: &recordFeedback{}, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

// TestSlackSilenceEndpointUpWithSilenceAlone: a deployment that enabled ONLY
// silencing must get a live endpoint. Guards the enablement condition in
// handleSlackInteraction, which previously required approvals or feedback.
func TestSlackSilenceEndpointUpWithSilenceAlone(t *testing.T) {
	const secret = "shh"
	srv := New(nil, Actions{Silence: &recordSilence{}, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "1h"); rr.Code == http.StatusNotFound {
		t.Fatal("endpoint 404s with silencing as the only enabled capability")
	}
}

// TestSlackSilenceRecorderErrorIsAcked: a ledger write failure must reach the
// human as a message, never as a 500 Slack will retry.
func TestSlackSilenceRecorderErrorIsAcked(t *testing.T) {
	const secret = "shh"
	rec := &recordSilence{err: errTestSilence}
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)
	if rr := sendSilence(t, srv, secret, "sil:k", "4h"); rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

var errTestSilence = errors.New("ledger is full of bees")

// TestSilenceReadsAStaticSelectPayload pins the reason the 2026-08-26 element
// swap needed no migration: Slack delivers the chosen option at
// actions[0].selected_option.value for a static_select exactly as it did for an
// overflow, so cards posted before the swap stay clickable after it. That claim
// was verified against Slack's payload docs during design and is the whole reason
// there is no dual-shape parsing here — if Slack ever diverges, it must fail in
// this test rather than as a silently dead control on every card still in
// scrollback.
func TestSilenceReadsAStaticSelectPayload(t *testing.T) {
	rec := &recordSilence{}
	const secret = "shh"
	srv := New(nil, Actions{Silence: rec, SlackSecret: secret}, nil, nil, nil, nil, discardLog)

	if rr := sendSilence(t, srv, secret, "sil:k", "24h"); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	want := recordedSilence{key: "k", window: 24 * time.Hour, user: "U9"}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Fatalf("recorded = %+v, want exactly one %+v", rec.got, want)
	}
}

// The pure-function tests for the rewrite itself now live beside it in
// internal/slackcard, which is where the walk moved so that the card BUILDER and
// the card REWRITER stop keeping two copies of the card's shape. What stays here
// is what only this package can prove: that the wiring honours the refusal.

// testSlackResponseURL is shaped like a real interaction response_url so the SSRF
// guard in postSlackResponse ACCEPTS it. That is the point: the guard takes no
// host but *.slack.com, so pointing the tests at an httptest server would have
// them pass through a refusal path and prove nothing about what gets posted.
const testSlackResponseURL = "https://hooks.slack.com/actions/T024BE7LD/1234567890/aBcDeFgHiJkLmNoPqRsTuVwX"

// captureSlackResponse stands in for Slack's response_url endpoint at the
// transport layer, which is the only seam that leaves the guard above running for
// real while still keeping the request off the network.
type captureSlackResponse struct {
	t     *testing.T
	posts *[]map[string]any
	// failCard answers the card rewrite with 500 while still accepting the
	// acknowledgement, which is how Slack fails in the case worth testing: the
	// silence is recorded, and the announcement of it is the part that did not
	// land.
	failCard bool
}

func (c captureSlackResponse) RoundTrip(req *http.Request) (*http.Response, error) {
	c.t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		c.t.Errorf("response_url body is not JSON: %v — %s", err, body)
	}
	*c.posts = append(*c.posts, decoded)
	if c.failCard && decoded["replace_original"] == true {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// testSlackSecret signs every interaction in this file. It is a constant rather
// than a returned value because no test cares what it is, only that the sender
// and the server agree on it.
const testSlackSecret = "shh"

// capturedSilenceServer builds a silence-enabled server whose response_url posts
// are decoded into got instead of being sent. Pass a recordSilence carrying an
// err to drive the ordering invariant: record first, then decorate.
func capturedSilenceServer(t *testing.T, posts *[]map[string]any, rec *recordSilence) *Server {
	t.Helper()
	return silenceServerWithTransport(t, posts, rec, false)
}

func silenceServerWithTransport(t *testing.T, posts *[]map[string]any, rec *recordSilence, failCard bool) *Server {
	t.Helper()
	srv := New(nil, Actions{Silence: rec, SlackSecret: testSlackSecret}, nil, nil, nil, nil, discardLog)
	srv.slackHTTP = &http.Client{Transport: captureSlackResponse{t: t, posts: posts, failCard: failCard}}
	return srv
}

// A successful silence answers the response_url TWICE — the card rewrite and the
// ephemeral ack to the clicker — so tests select the one they mean rather than
// assuming a single post. Both return nil when that answer was not sent, which is
// itself what several tests assert.
func cardPost(posts []map[string]any) map[string]any {
	for _, p := range posts {
		if p["replace_original"] == true {
			return p
		}
	}
	return nil
}

func ackPost(posts []map[string]any) map[string]any {
	for _, p := range posts {
		if p["response_type"] == "ephemeral" {
			return p
		}
	}
	return nil
}

// markerText returns the text of the trailing context block the rewrite appends,
// read from the DECODED payload rather than a re-marshalled dump. encoding/json
// escapes < and > to \u003c/\u003e on the wire, so a mention searched for in a
// dump never matches — even though Slack decodes it back to "<@U9>" before it
// ever renders. Asserting on the dump would fail a correct mention and pass a
// broken one written as an escape.
func markerText(t *testing.T, card map[string]any) string {
	t.Helper()
	blocks, _ := card["blocks"].([]any)
	if len(blocks) == 0 {
		return ""
	}
	last, _ := blocks[len(blocks)-1].(map[string]any)
	els, _ := last["elements"].([]any)
	if len(els) == 0 {
		return ""
	}
	el, _ := els[0].(map[string]any)
	text, _ := el["text"].(string)
	return text
}

// sendSilenceCard is sendSilence plus the two payload fields the card rewrite
// needs: where to post the rebuilt card, and the card it is rebuilding. Passing
// blocks == "" omits message.blocks entirely, which is the shape that must fall
// back rather than blank the card. text is the card's top-level "text", the
// notification/accessibility fallback a rewrite must carry over.
func sendSilenceCard(t *testing.T, srv *Server, secret, blockID, value, responseURL, blocks, text string) *httptest.ResponseRecorder {
	t.Helper()
	// text and responseURL are quoted through encoding/json rather than spliced in:
	// a realistic fallback like `harbor-db "crash-looping"` would otherwise produce
	// invalid JSON, and the handler's 400 would read as "the silence path rejected
	// my card" instead of "my fixture was malformed".
	msg := ""
	if blocks != "" {
		msg = `,"message":{"blocks":` + blocks + `,"text":` + quoteJSON(t, text) + `}`
	}
	return postSignedInteraction(t, srv, secret,
		`{"user":{"id":"U9","username":"bob"},"response_url":`+quoteJSON(t, responseURL)+`,`+
			`"actions":[{"action_id":"`+slackcard.SilenceActionID+`","block_id":`+quoteJSON(t, blockID)+
			`,"selected_option":{"value":`+quoteJSON(t, value)+`}}]`+msg+`}`)
}

const testSilenceCardBlocks = `[
  {"type":"section","text":{"type":"mrkdwn","text":"*Why:* something broke"}},
  {"type":"actions","block_id":"sil:k","elements":[
    {"type":"button","action_id":"runlore_feedback_up"},
    {"type":"static_select","action_id":"runlore_silence"}]}]`

// TestSilenceKeepsTheNotificationFallbackText pins a field that is invisible on
// screen and therefore easy to drop: the card's top-level "text", which is the
// one-line summary Slack shows in push notifications and to block-less clients
// (notify.fallbackText builds it). replace_original REPLACES the message, so a
// rewrite that posts only blocks clears it — leaving every later notification for
// that message, and every screen reader, with nothing to read.
func TestSilenceKeepsTheNotificationFallbackText(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	const fallback = "🔍 harbor-db crash-looping — Action required"
	blocks := testSilenceCardBlocks
	rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, blocks, fallback)
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	card := cardPost(posts)
	if card == nil {
		t.Fatal("the card was never rewritten")
	}
	if card["text"] != fallback {
		t.Errorf("text = %v, want the original fallback %q preserved", card["text"], fallback)
	}
}

// TestSilenceRewritesTheCardPublicly pins both halves of what the first live
// silence exposed on 2026-08-26: the acknowledgement must not be ephemeral
// (nobody but the clicker learned the alert had been handled, which invites a
// second person to investigate it) and the card must carry the marker afterwards
// (scrollback could not tell a handled finding from an unhandled one).
func TestSilenceRewritesTheCardPublicly(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, testSilenceCardBlocks, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	card := cardPost(posts)
	if card == nil {
		t.Fatal("nothing was posted with replace_original — the card was not rewritten in place")
	}
	if card["response_type"] == "ephemeral" {
		t.Error("the card rewrite is ephemeral: nobody but the clicker sees it")
	}
	if card["blocks"] == nil {
		t.Fatal("no blocks posted — the rebuild did not reach the response_url")
	}
	if marker := markerText(t, card); !strings.Contains(marker, "Silenced by") {
		t.Errorf("the posted card carries no marker, got %q", marker)
	}
	dump, _ := json.Marshal(card["blocks"])
	if strings.Contains(string(dump), "runlore_silence") {
		t.Error("the silence control survived onto the rewritten card")
	}
	if !strings.Contains(string(dump), "something broke") {
		t.Error("the rewrite dropped the finding it was marking")
	}
}

// TestSilenceWithoutBlocksLeavesTheCardIntact is the end-to-end half of the hard
// invariant: a payload with no blocks must degrade to the old ephemeral note, not
// post an empty card. TestSilencedCardRefusesRatherThanBlanksTheCard pins the pure
// function; this pins that the wiring actually honours its refusal, which is the
// half a caller can silently get wrong by ignoring the second return value.
func TestSilenceWithoutBlocksLeavesTheCardIntact(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	if rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, "", ""); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	if card := cardPost(posts); card != nil {
		t.Errorf("rewrote the card with nothing to rebuild from: %v", card)
	}
	ack := ackPost(posts)
	if ack == nil {
		t.Fatal("no ephemeral fallback posted — the click went unacknowledged")
	}
	if ack["blocks"] != nil {
		t.Errorf("posted blocks with nothing to rebuild from: %v", ack["blocks"])
	}
}

// TestSilenceLedgerFailureLeavesTheCardUnmarked pins the ordering the design
// requires: record first, then decorate. A marker on a card whose silence was
// never stored is a lie the reader has no way to detect — the suppression simply
// does not happen, and the channel is told it did.
func TestSilenceLedgerFailureLeavesTheCardUnmarked(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{err: errTestSilence})

	if rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, testSilenceCardBlocks, ""); rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	dump, _ := json.Marshal(posts)
	if strings.Contains(string(dump), "Silenced by") {
		t.Errorf("the card was marked despite the ledger write failing: %s", dump)
	}
}

// TestSilenceStillWarnsTheClicker pins what the public card marker must NOT cost.
// The marker answers "has anyone dealt with this?" for a colleague scanning the
// channel; it says nothing about what the click switched OFF. SilenceAck does —
// "RunLore will NOT investigate this incident … no model call, no notification,
// no record" — and, since #556, it names the escape hatches THIS deployment
// actually has, which is the one place feedback_buttons:false is disclosed.
//
// Rewriting the card and acknowledging the clicker are two different answers to
// two different readers, and response_url takes up to five, so the success path
// owes both. Answering only the card leaves the person who clicked less informed
// than before the card was ever rewritten.
func TestSilenceStillWarnsTheClicker(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, testSilenceCardBlocks, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	if cardPost(posts) == nil {
		t.Fatal("the card was not rewritten")
	}
	ack := ackPost(posts)
	if ack == nil {
		t.Fatal("the card was rewritten but the clicker got no acknowledgement at all")
	}
	text, _ := ack["text"].(string)
	if !strings.Contains(text, "will NOT investigate") {
		t.Errorf("the ack no longer warns that investigation is off: %q", text)
	}
}

// TestSilenceMarkerMentionsTheUserByID pins the ONE form Slack linkifies inside a
// Block Kit mrkdwn element: <@U9>. A bare "@bob" renders as literal text there —
// link_names is a chat.postMessage option and has no effect on blocks — so the
// marker would name the silencer without notifying them or linking their profile,
// which is most of why the marker names them at all.
func TestSilenceMarkerMentionsTheUserByID(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, testSilenceCardBlocks, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	card := cardPost(posts)
	if card == nil {
		t.Fatal("the card was not rewritten")
	}
	marker := markerText(t, card)
	if !strings.Contains(marker, "<@U9>") {
		t.Errorf("the marker does not mention the user by id: %q", marker)
	}
	if strings.Contains(marker, "@bob") {
		t.Errorf("the marker carries a bare @username, which Slack renders as literal text: %q", marker)
	}
}

// TestSilenceTellsTheClickerWhenTheCardWentUnmarked covers both ways the marker
// can fail to land. The silence itself succeeded in each — it is in the ledger and
// RunLore is suppressing — so the failure is invisible to the clicker unless they
// are told: they walk away believing the channel knows the finding is handled,
// which is the exact "a colleague starts investigating it anyway" defect the
// marker exists to prevent, restored silently.
func TestSilenceTellsTheClickerWhenTheCardWentUnmarked(t *testing.T) {
	for _, tc := range []struct {
		name     string
		blocks   string
		failCard bool
	}{
		{"Slack sent no blocks to rebuild from", "", false},
		{"Slack refused the rewrite", testSilenceCardBlocks, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts []map[string]any
			rec := &recordSilence{}
			srv := silenceServerWithTransport(t, &posts, rec, tc.failCard)

			rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, tc.blocks, "")
			if rr.Code != http.StatusOK {
				t.Fatalf("silence = %d, want 200", rr.Code)
			}
			if len(rec.got) != 1 {
				t.Fatalf("the silence was not recorded: %+v — this test is about the ANNOUNCEMENT failing, not the suppression", rec.got)
			}
			ack := ackPost(posts)
			if ack == nil {
				t.Fatal("no acknowledgement reached the clicker at all")
			}
			text, _ := ack["text"].(string)
			if !strings.Contains(text, thread.SilenceCardUnmarked) {
				t.Errorf("the clicker was not told the card went unmarked: %q", text)
			}
			if !strings.Contains(text, "will NOT investigate") {
				t.Errorf("the disclosure replaced the ack instead of being appended to it: %q", text)
			}
		})
	}
}

// TestSilenceSaysNothingAboutTheCardWhenItWasMarked is the other half: the notice
// above is a real warning, so it must not appear on the happy path where it would
// train people to ignore it.
func TestSilenceSaysNothingAboutTheCardWhenItWasMarked(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "48h", testSlackResponseURL, testSilenceCardBlocks, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	ack := ackPost(posts)
	if ack == nil {
		t.Fatal("no acknowledgement reached the clicker")
	}
	if text, _ := ack["text"].(string); strings.Contains(text, thread.SilenceCardUnmarked) {
		t.Errorf("cried wolf about an unmarked card that was in fact marked: %q", text)
	}
}

// TestSilenceOnAnAlreadyRewrittenCardDoesNotStackAMarker is the end-to-end half of
// slackcard's double-marker refusal: a second engineer clicking a stale card must
// not have their marker appended under the first one, reporting two different
// windows for one finding.
func TestSilenceOnAnAlreadyRewrittenCardDoesNotStackAMarker(t *testing.T) {
	var posts []map[string]any
	srv := capturedSilenceServer(t, &posts, &recordSilence{})

	// A card that has already been through the rewrite: no silence control left.
	const rewritten = `[
	  {"type":"section","text":{"type":"mrkdwn","text":"*Why:* something broke"}},
	  {"type":"actions","elements":[{"type":"button","action_id":"runlore_feedback_up"}]},
	  {"type":"context","elements":[{"type":"mrkdwn","text":"🔕 Silenced by <@U1> until later · 4h"}]}]`

	rr := sendSilenceCard(t, srv, testSlackSecret, "sil:k", "24h", testSlackResponseURL, rewritten, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("silence = %d, want 200", rr.Code)
	}
	if card := cardPost(posts); card != nil {
		t.Errorf("rewrote a card that had already been rewritten: %v", card)
	}
	ack := ackPost(posts)
	if ack == nil {
		t.Fatal("no acknowledgement reached the clicker")
	}
	text, _ := ack["text"].(string)
	// The distinction that matters: this card IS marked, just with someone else's
	// window. Telling the clicker "the channel cannot tell this finding is handled"
	// would send them to write a redundant note while the real problem — the card
	// shows 4h, the ledger now holds theirs — goes unsaid.
	if !strings.Contains(text, thread.SilenceCardStale) {
		t.Errorf("the clicker was not told the card carries the EARLIER window: %q", text)
	}
	if strings.Contains(text, thread.SilenceCardUnmarked) {
		t.Errorf("claimed the card is unmarked, but it plainly carries a marker: %q", text)
	}
}
