// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

func init() {
	Register(Descriptor{
		Name: "matrix",
		Build: func(d Deps) (providers.Notifier, error) {
			if mc := d.Cfg.Notify.Matrix; mc.Homeserver != "" && mc.RoomID != "" && mc.AccessTokenEnv != "" {
				if tok := os.Getenv(mc.AccessTokenEnv); tok != "" {
					m := NewMatrix(mc.Homeserver, mc.RoomID, tok)
					if mc.ThreadCapture {
						m.Threads = d.Threads
					}
					return m, nil
				}
			}
			return nil, nil
		},
	})
}

// Matrix delivers via the Matrix client-server send API.
type Matrix struct {
	homeserver string
	roomID     string
	token      string
	http       *http.Client
	txn        atomic.Int64

	// Threads, when set (notify.matrix.thread_capture), receives the thread
	// root of each delivered investigation, so a later reply can be
	// attributed. nil (the default) disables thread capture.
	Threads ThreadSink
}

// NewMatrix builds a Matrix notifier. homeserver is the base URL (e.g.
// https://matrix.org); roomID is like "!abc:hs"; token is an access token.
//
// The txn counter is seeded from the wall clock (UnixNano) rather than 0 so that
// transaction ids keep increasing across process restarts. Homeservers dedupe by
// (access_token, txnId); a fresh process starting back at "runlore-1" could
// otherwise collide with a pre-restart id and have its message silently dropped.
// Caveat: a backwards wall-clock jump across a restart could still collide — an
// acceptable residual given the dedup window is minutes and the prior behaviour
// offered zero protection.
func NewMatrix(homeserver, roomID, token string) *Matrix {
	m := &Matrix{
		homeserver: strings.TrimRight(homeserver, "/"),
		roomID:     roomID,
		token:      token,
		http:       httpx.SecureClient(15 * time.Second),
	}
	m.txn.Store(time.Now().UnixNano())
	return m
}

var _ providers.Notifier = (*Matrix)(nil)
var _ providers.ThreadNotifier = (*Matrix)(nil)
var _ providers.KBUpdateNotifier = (*Matrix)(nil)

// send posts content as an m.room.message event to room via the Matrix
// client-server send API, handling the transaction id, auth header, and status
// check shared by every event Matrix posts. It returns the event id the
// homeserver assigned, or "" if the response body didn't decode to one — a
// send that got a 2xx is still a success in that case; only the event id, used
// to register a thread root or chain a reply, is unknown.
func (m *Matrix) send(ctx context.Context, room string, content map[string]any) (string, error) {
	txn := fmt.Sprintf("runlore-%d", m.txn.Add(1))
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		m.homeserver, url.PathEscape(room), url.PathEscape(txn))

	body, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("matrix send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("matrix status %d", resp.StatusCode)
	}

	var sent struct {
		EventID string `json:"event_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sent)
	return sent.EventID, nil
}

// Deliver sends the formatted investigation as an m.notice message. It carries
// both a plaintext body (the fallback Matrix renders literally) and a rich
// formatted_body (org.matrix.custom.html) so the message's mrkdwn renders as
// bold/links/code instead of leaking raw *asterisks* to Matrix clients.
//
// EVERY field this function writes is bounded, and the ceilings are chosen to
// sum: two bodies at matrixDeliverBodyBytes (boundMatrixBodies) plus two stamps
// at matrixStampBytes (boundStamp) is 40,960 bytes, leaving 24,576 of the spec's
// 65,536 for the federation envelope, the content hash and the signatures. That
// arithmetic is the point. Format(inv) is model-authored end to end (the title,
// every hypothesis, every evidence line) and buildInvestigation caps none of it,
// so the size of this event is whatever the model wrote unless something here
// says otherwise — and a homeserver rejects the whole event past the ceiling,
// which is an investigation that ran, spent its model budget and reached a
// verdict, that nobody is ever told about.
//
// Bounding each field on its own was not enough, and this function is where that
// was learned: the bodies took 16 KiB each and the stamp beside them took 78, so
// three ceilings existed and none of them was a ceiling on the EVENT.
func (m *Matrix) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, formatted := boundMatrixBodies(Format(inv))
	content := map[string]any{
		"msgtype": "m.notice",
		// escapeMatrixReply on the PLAIN body, not just on thread replies: this
		// message interpolates model-authored text (the title, each root cause's
		// rationale, the evidence), and .m.rule.roomnotif matches "@room" in
		// content.body to notify every member of the room. The formatted_body is
		// not the vector — the push rule reads body — but the body is, and it
		// carries exactly the same untrusted prose a reply does.
		"body":           body,
		"format":         "org.matrix.custom.html",
		"formatted_body": formatted,
	}
	// Stamp the thread context into the event content (a custom field — legal in
	// Matrix events, invisible in clients) so a threaded reply can be attributed
	// back to this investigation. Unconditional: the field is inert data; the
	// LISTENER is the opt-in (notify.matrix.thread_capture).
	//
	// stampFor bounds it — see boundStamp. That the bound lives inside the builder
	// rather than out here is the fix for this defect and not a stylistic choice:
	// the bodies were bounded on the first line of this function and the stamp
	// assigned unbounded twenty-two lines below, so the event honoured two
	// ceilings and blew straight through the one that actually applies to it.
	stamp := stampFor(inv)
	content[threadContentField] = stamp
	// The legacy trigger field the opt-in reaction listener reads to join a 👍/👎
	// on this message back to the incident (notify.matrix.feedback_reactions), and
	// contextFromContent's fallback when a decoded stamp carries no trigger_key of
	// its own.
	//
	// It is taken FROM the stamp rather than recomputed off inv, which does two
	// things. It keeps the two from contradicting each other: a fallback naming an
	// identity the stamp had just dropped would be worse than no fallback at all.
	// And it puts this field inside the same bound, instead of leaving the event
	// unbounded through the one field nobody was looking at. The value is
	// byte-identical for every finding that fits — stampFor applies the same
	// cmp.Or(TriggerKey, Fingerprint) this line used to.
	if stamp.TriggerKey != "" {
		content[triggerKeyContentField] = stamp.TriggerKey
	}

	eventID, err := m.send(ctx, m.roomID, content)
	if err != nil {
		return err
	}

	// Registration is best-effort: a send that succeeded stays succeeded no
	// matter what happens below. eventID is "" when the homeserver's response
	// didn't decode to one, in which case the root just stays unregistered.
	if m.Threads != nil && eventID != "" {
		m.Threads.Register(m.Transport(), eventID, m.roomID, inv)
	}
	return nil
}

// roomPingRe matches the room-wide mention token the .m.rule.roomnotif push
// rule keys on. Matched case-insensitively because the rule's own
// content-match is: "@ROOM" notifies exactly as "@room" does.
var roomPingRe = regexp.MustCompile(`(?i)@room`)

// escapeMatrixReply neutralises untrusted text for a Matrix m.notice BODY —
// the transport-specific half of thread.RenderReply's contract, and
// deliberately not the same job escapeMrkdwn does for Slack.
//
// A reply body is plain text with no format/formatted_body, so there is no
// markup to inject: an HTML tag or a Slack "<https://evil|text>" link renders
// literally. What a body CAN still carry is "@room", which the
// .m.rule.roomnotif push rule turns into a notification for every member when
// the sender holds the room notification power level — the direct analogue of
// Slack's "<!channel>", and reachable the same way, through model prose
// composing RunLore's outgoing message.
//
// A word joiner is inserted rather than any character being removed: the token
// stops matching the push rule while the words a human reads are unchanged,
// which is the same "mark, never censor" rule thread.modelVoice follows.
func escapeMatrixReply(s string) string {
	return roomPingRe.ReplaceAllStringFunc(s, func(tok string) string { return tok[:1] + "\u2060" + tok[1:] })
}

// ReplyInThread posts text as an m.notice reply into the Matrix thread rooted
// at root (providers.ThreadNotifier), via the MSC3440 m.thread relation. Text
// is plain — no formatted_body — because these are RunLore's own short
// acknowledgements ("📝 Noted on…") plus, when model.chat is configured, the
// model's answer. It targets the passed channel (room) when non-empty, falling
// back to the notifier's configured room, mirroring SlackBot.ReplyInThread: a
// reply can only go where the thread root it answers was sent.
//
// The body goes through thread.RenderReply so the untrusted spans in it are
// neutralised for THIS transport and RunLore's own framing is posted verbatim
// — see escapeMatrixReply for what that means on a plain-text body, and
// SlackBot.ReplyInThread for the same boundary drawn against mrkdwn. Calling
// it is not optional even for a transport with little to escape: RenderReply
// is also what removes the in-band span marks, which must never reach a room.
//
// The rendered body is then bounded at matrixReplyBytes. A Matrix event has a
// hard 65,536-byte ceiling, so an oversized body is a rejected event and a
// human left in silence about a note that was already written; see
// boundPostedReply for why the bound sits after rendering and what it drops.
func (m *Matrix) ReplyInThread(ctx context.Context, root, channel, text string) error {
	room := cmp.Or(channel, m.roomID)
	content := map[string]any{
		"msgtype": "m.notice",
		"body":    boundPostedReply(thread.RenderReply(text, escapeMatrixReply), matrixReplyBytes),
		"m.relates_to": map[string]any{
			"rel_type": "m.thread",
			"event_id": root,
		},
	}
	_, err := m.send(ctx, room, content)
	return err
}

// Transport identifies this notifier's chat system (providers.ThreadNotifier),
// the key Multi.ThreadRepliers scopes thread replies by.
func (m *Matrix) Transport() string { return "matrix" }

// boldRe / codeRe match the small mrkdwn subset that Format emits (plus inline
// code for robustness): *bold* and `code`.  URL auto-linkification has been
// intentionally removed: the investigation body is assembled from untrusted
// content (LLM output, alert labels, log evidence) and an attacker-influenced
// URL rendered as a live <a href> is a phishing vector inside Matrix clients.
// Slack takes the same stance (escapeMrkdwn neutralises < > |), so both
// notifiers now present URLs as inert plain text.
var (
	boldRe = regexp.MustCompile(`\*([^*\n]+)\*`)
	codeRe = regexp.MustCompile("`([^`\n]+)`")
)

// mrkdwnToHTML converts the message's Slack-mrkdwn subset to the minimal HTML
// Matrix accepts in formatted_body. Order matters: HTML-escape first so user
// content (evidence strings, change refs) can never inject live markup, then
// apply the markup transforms, which only ever emit a fixed, safe tag set.
// URLs are left as plain (HTML-escaped) text — see the boldRe/codeRe comment.
func mrkdwnToHTML(s string) string {
	s = html.EscapeString(s)
	s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = codeRe.ReplaceAllString(s, "<code>$1</code>")
	return strings.ReplaceAll(s, "\n", "<br/>")
}

// plainFallback strips the mrkdwn emphasis markers so the plaintext body Matrix
// renders literally doesn't show raw *asterisks*/backticks.
func plainFallback(s string) string {
	s = boldRe.ReplaceAllString(s, "$1")
	s = codeRe.ReplaceAllString(s, "$1")
	return s
}
