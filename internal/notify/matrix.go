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
func (m *Matrix) Deliver(ctx context.Context, inv providers.Investigation) error {
	msg := Format(inv)
	content := map[string]any{
		"msgtype":        "m.notice",
		"body":           plainFallback(msg),
		"format":         "org.matrix.custom.html",
		"formatted_body": mrkdwnToHTML(msg),
	}
	// Stamp the trigger identity into the event content (a custom field — legal in
	// Matrix events, invisible in clients) so the opt-in reaction listener can join
	// a 👍/👎 on this message back to the incident. Unconditional: the field is
	// inert data; the LISTENER is the opt-in (notify.matrix.feedback_reactions).
	if key := cmp.Or(inv.TriggerKey, inv.Fingerprint); key != "" {
		content[triggerKeyContentField] = key
	}
	// Stamp the thread context alongside it — additive, same rationale: inert
	// data, unconditional, the eventual reply listener is the opt-in.
	content[threadContentField] = stampFor(inv)

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

// ReplyInThread posts text as an m.notice reply into the Matrix thread rooted
// at root (providers.ThreadNotifier), via the MSC3440 m.thread relation. Text
// is plain — no formatted_body — because these are RunLore's own short
// acknowledgements ("📝 Noted on…"), not investigation prose. It targets the
// passed channel (room) when non-empty, falling back to the notifier's
// configured room, mirroring SlackBot.ReplyInThread: a reply can only go where
// the thread root it answers was sent.
func (m *Matrix) ReplyInThread(ctx context.Context, root, channel, text string) error {
	room := cmp.Or(channel, m.roomID)
	content := map[string]any{
		"msgtype": "m.notice",
		"body":    text,
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
