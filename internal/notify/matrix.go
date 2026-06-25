package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// Matrix delivers via the Matrix client-server send API.
type Matrix struct {
	homeserver string
	roomID     string
	token      string
	http       *http.Client
	txn        atomic.Int64
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

// Deliver sends the formatted investigation as an m.notice message. It carries
// both a plaintext body (the fallback Matrix renders literally) and a rich
// formatted_body (org.matrix.custom.html) so the message's mrkdwn renders as
// bold/links/code instead of leaking raw *asterisks* to Matrix clients.
func (m *Matrix) Deliver(ctx context.Context, inv providers.Investigation) error {
	txn := fmt.Sprintf("runlore-%d", m.txn.Add(1))
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		m.homeserver, url.PathEscape(m.roomID), url.PathEscape(txn))

	msg := Format(inv)
	body, err := json.Marshal(map[string]string{
		"msgtype":        "m.notice",
		"body":           plainFallback(msg),
		"format":         "org.matrix.custom.html",
		"formatted_body": mrkdwnToHTML(msg),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("matrix send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("matrix status %d", resp.StatusCode)
	}
	return nil
}

// boldRe / codeRe / linkRe match the small mrkdwn subset that Format emits
// (plus inline code for robustness): *bold*, `code`, and bare http(s) URLs.
var (
	boldRe = regexp.MustCompile(`\*([^*\n]+)\*`)
	codeRe = regexp.MustCompile("`([^`\n]+)`")
	linkRe = regexp.MustCompile(`https?://[^\s<]+`)
)

// mrkdwnToHTML converts the message's Slack-mrkdwn subset to the minimal HTML
// Matrix accepts in formatted_body. Order matters: HTML-escape first so user
// content (evidence strings, change refs) can never inject live markup, then
// apply the markup transforms, which only ever emit a fixed, safe tag set.
func mrkdwnToHTML(s string) string {
	s = html.EscapeString(s)
	s = boldRe.ReplaceAllString(s, "<strong>$1</strong>")
	s = codeRe.ReplaceAllString(s, "<code>$1</code>")
	s = linkRe.ReplaceAllStringFunc(s, func(u string) string {
		// u is already HTML-escaped; safe to embed in both attribute and text.
		return fmt.Sprintf(`<a href=%q>%s</a>`, u, u)
	})
	return strings.ReplaceAll(s, "\n", "<br/>")
}

// plainFallback strips the mrkdwn emphasis markers so the plaintext body Matrix
// renders literally doesn't show raw *asterisks*/backticks.
func plainFallback(s string) string {
	s = boldRe.ReplaceAllString(s, "$1")
	s = codeRe.ReplaceAllString(s, "$1")
	return s
}
