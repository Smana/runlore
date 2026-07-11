// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestMatrixDeliver(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), sampleInvestigation())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.Contains(gotPath, "/_matrix/client/v3/rooms/") || !strings.Contains(gotPath, "/send/m.room.message/") {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if mt, _ := gotBody["msgtype"].(string); mt != "m.notice" {
		t.Fatalf("msgtype = %v", gotBody["msgtype"])
	}
	if body, _ := gotBody["body"].(string); !strings.Contains(body, "flux rollback hr/harbor") {
		t.Fatalf("body missing content: %v", gotBody["body"])
	}
}

// TestMatrixDeliverMatchedKnowledge confirms the existing-KB match reaches
// Matrix (via the shared Format): the plaintext body carries the runbook line,
// and the URL is present as plain (defanged) text — NOT a live <a href> — so a
// model-authored attacker-influenced URL cannot render as a phishing link.
func TestMatrixDeliverMatchedKnowledge(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	inv := sampleInvestigation()
	inv.MatchedKnowledge = &providers.MatchedEntry{Title: "Harbor probe runbook", Path: "runbooks/harbor.md", URL: "https://kb.example/runbooks/harbor.md", Score: 6}
	if err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if body, _ := gotBody["body"].(string); !strings.Contains(body, "Matches known runbook: Harbor probe runbook") {
		t.Fatalf("plaintext body missing matched-runbook line: %v", gotBody["body"])
	}
	fb, _ := gotBody["formatted_body"].(string)
	// URL must appear as plain text (HTML-escaped), never as a live anchor.
	if strings.Contains(fb, `<a href=`) {
		t.Errorf("formatted_body contains live <a href> — URLs must not be auto-linkified: %s", fb)
	}
	if !strings.Contains(fb, "https://kb.example/runbooks/harbor.md") {
		t.Errorf("formatted_body missing the plain-text URL: %s", fb)
	}
}

// TestMatrixDeliverHTML asserts the sent event carries a rich HTML
// formatted_body alongside a clean plaintext body fallback. Matrix renders the
// plain body literally, so without formatted_body users would see raw *markup*.
func TestMatrixDeliverHTML(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	inv := sampleInvestigation()
	// Inject raw HTML to prove user content is escaped, not rendered.
	inv.RootCauses[0].Evidence = append(inv.RootCauses[0].Evidence, "<script>alert(1)</script>")
	inv.CuratedURL = "https://kb.example/entry/42"

	if err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if f, _ := gotBody["format"].(string); f != "org.matrix.custom.html" {
		t.Fatalf("format = %v, want org.matrix.custom.html", gotBody["format"])
	}
	fb, _ := gotBody["formatted_body"].(string)
	if fb == "" {
		t.Fatal("formatted_body is empty")
	}
	if !strings.Contains(fb, "<strong>") {
		t.Errorf("formatted_body missing <strong> for bold: %s", fb)
	}
	// URLs must NOT be auto-linkified (anti-phishing; see S1 fix). The URL must
	// appear as plain HTML-escaped text, never as a live <a href> anchor.
	if strings.Contains(fb, `<a href=`) {
		t.Errorf("formatted_body contains live <a href> — URLs must not be auto-linkified: %s", fb)
	}
	if !strings.Contains(fb, "https://kb.example/entry/42") {
		t.Errorf("formatted_body missing plain-text URL: %s", fb)
	}
	if !strings.Contains(fb, "<br/>") {
		t.Errorf("formatted_body missing <br/> for newlines: %s", fb)
	}
	// User content must be escaped, never rendered as live markup.
	if strings.Contains(fb, "<script>") {
		t.Errorf("formatted_body did not escape user HTML: %s", fb)
	}
	if !strings.Contains(fb, "&lt;script&gt;") {
		t.Errorf("formatted_body missing escaped user HTML: %s", fb)
	}

	// Plaintext fallback: no raw mrkdwn asterisks/backticks.
	body, _ := gotBody["body"].(string)
	if strings.Contains(body, "*") || strings.Contains(body, "`") {
		t.Errorf("plaintext body still carries raw markup: %q", body)
	}
	if !strings.Contains(body, "flux rollback hr/harbor") {
		t.Errorf("plaintext body missing content: %q", body)
	}
}

func TestMrkdwnToHTML(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "hello world", "hello world"},
		{"bold", "a *bold* b", "a <strong>bold</strong> b"},
		{"code", "run `kubectl get` now", "run <code>kubectl get</code> now"},
		// URLs stay as plain HTML-escaped text — no auto-linkification (S1: anti-phishing).
		{"link", "see https://x.io/p done", "see https://x.io/p done"},
		{"newline", "line1\nline2", "line1<br/>line2"},
		{"escape", "a < b & c > d", "a &lt; b &amp; c &gt; d"},
		{"escape_in_bold", "*<b>*", "<strong>&lt;b&gt;</strong>"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mrkdwnToHTML(tc.in); got != tc.want {
				t.Errorf("mrkdwnToHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestMrkdwnToHTMLNoLiveURLs is the security regression test for S1: URLs that
// appear in untrusted fields (LLM output, evidence, alert labels) must never be
// emitted as live <a href> anchors in the Matrix formatted_body. A live link
// would let an attacker influence a future investigation to carry a phishing URL
// that Matrix clients render as a clickable hyperlink.
func TestMrkdwnToHTMLNoLiveURLs(t *testing.T) {
	untrustedInputs := []string{
		"https://attacker.example/phish",
		"evidence: see http://evil.io/x?data=leak",
		"check https://kb.internal/good AND https://bad.actor/steal",
	}
	for _, input := range untrustedInputs {
		got := mrkdwnToHTML(input)
		if strings.Contains(got, "<a href") {
			t.Errorf("mrkdwnToHTML(%q) emitted a live anchor: %s", input, got)
		}
	}
}

// TestMatrixTxnSurvivesRestart proves txn ids don't collide after a process
// restart: a fresh notifier (simulated restart) starts above the prior one's
// last-used id, so the homeserver won't dedupe a post-crash message.
func TestMatrixTxnSurvivesRestart(t *testing.T) {
	capture := func(m *Matrix, n int) []string {
		var ids []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// txn id is the last path segment: .../send/m.room.message/{txn}
			parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
			ids = append(ids, parts[len(parts)-1])
			_, _ = w.Write([]byte(`{"event_id":"$x"}`))
		}))
		defer srv.Close()
		m.homeserver = srv.URL
		for i := 0; i < n; i++ {
			if err := m.Deliver(context.Background(), sampleInvestigation()); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
		}
		return ids
	}

	first := capture(NewMatrix("http://placeholder", "!room:hs", "tok"), 3)
	second := capture(NewMatrix("http://placeholder", "!room:hs", "tok"), 3)

	for _, id := range append(append([]string{}, first...), second...) {
		if !strings.HasPrefix(id, "runlore-") {
			t.Fatalf("unexpected txn id format: %q", id)
		}
	}
	// No id from the second run may equal any id from the first run.
	seen := map[string]bool{}
	for _, id := range first {
		seen[id] = true
	}
	for _, id := range second {
		if seen[id] {
			t.Fatalf("txn id collision across restart: %q (first=%v second=%v)", id, first, second)
		}
	}
}

// TestMatrixDeliverEmbedsTriggerKey: the event content carries the trigger
// identity (custom field, invisible in clients) so the reaction listener can
// join a 👍/👎 back to the incident — TriggerKey first, fingerprint fallback,
// omitted when the investigation carries neither.
func TestMatrixDeliverEmbedsTriggerKey(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	m := NewMatrix(srv.URL, "!r:hs", "tok")

	inv := sampleInvestigation()
	inv.TriggerKey = "k1"
	if err := m.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got, _ := gotBody[triggerKeyContentField].(string); got != "k1" {
		t.Fatalf("content[%s] = %v, want k1", triggerKeyContentField, gotBody[triggerKeyContentField])
	}

	inv.TriggerKey = ""
	inv.Fingerprint = "fp9"
	if err := m.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got, _ := gotBody[triggerKeyContentField].(string); got != "fp9" {
		t.Fatalf("fingerprint fallback = %v, want fp9", gotBody[triggerKeyContentField])
	}

	inv.Fingerprint = ""
	gotBody = nil // json.Decode merges into an existing map; reset to observe omission
	if err := m.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, ok := gotBody[triggerKeyContentField]; ok {
		t.Fatalf("no trigger identity ⇒ field must be omitted, got %v", gotBody[triggerKeyContentField])
	}
}
