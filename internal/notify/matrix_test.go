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
	"github.com/Smana/runlore/internal/thread"
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

func TestMatrixDeliverRegistersTheEventAsThreadRoot(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"event_id":"$evt123"}`))
	}))
	defer srv.Close()

	sink := &recordingThreadSink{}
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	m.Threads = sink

	inv := providers.Investigation{Title: "OOMKilled", TriggerKey: "tk-1", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := m.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if sink.calls != 1 {
		t.Fatalf("Register calls = %d, want 1", sink.calls)
	}
	if sink.root != "$evt123" {
		t.Errorf("root = %q, want the sent event id $evt123", sink.root)
	}
	if sink.channel != "!room:example.org" {
		t.Errorf("channel = %q, want the room id", sink.channel)
	}
	if sink.transport != "matrix" {
		t.Errorf("transport = %q, want matrix", sink.transport)
	}
	if body[threadContentField] == nil {
		t.Fatalf("the event content must carry %s: %v", threadContentField, body)
	}
	if body[triggerKeyContentField] != "tk-1" {
		t.Errorf("the pre-existing trigger-key field must be unchanged, got %v", body[triggerKeyContentField])
	}
}

func TestMatrixDeliverSucceedsWithNoThreadSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"event_id":"$evt123"}`))
	}))
	defer srv.Close()
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	if err := m.Deliver(context.Background(), providers.Investigation{Title: "OOMKilled"}); err != nil {
		t.Fatalf("Deliver with a nil thread sink: %v", err)
	}
}

func TestMatrixDeliverSucceedsWhenTheResponseHasNoEventID(t *testing.T) {
	// A homeserver that returns 2xx with an unexpected body must not fail
	// delivery — the message was sent; only the thread root is unknown.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	sink := &recordingThreadSink{}
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	m.Threads = sink
	if err := m.Deliver(context.Background(), providers.Investigation{Title: "OOMKilled"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if sink.calls != 0 {
		t.Fatal("an empty event id must never be registered")
	}
}

// TestContextFromStamp proves the reconstructed thread.Context takes its Root
// and Channel from the fetch parameters (the event itself), never from the
// stamp — threadStamp carries no root/channel field, so there is nothing in a
// forged stamp that could redirect where a note is written. stampFor and
// contextFromStamp round-trip the rest of the identifiers unchanged; a later
// task (the reaction/thread listener) builds its event lookup on this pair.
func TestContextFromStamp(t *testing.T) {
	inv := providers.Investigation{
		TriggerKey:    "tk-1",
		Title:         "OOMKilled",
		Resource:      providers.Workload{Namespace: "prod", Name: "harbor-registry"},
		Verdict:       providers.VerdictActionRequired,
		CuratedURL:    "https://github.com/o/r/pull/42",
		RecalledEntry: "catalog/oom.md",
	}

	got := contextFromStamp(stampFor(inv), "$evt123", "!room:example.org")

	want := thread.Context{
		Transport:     "matrix",
		Root:          "$evt123",
		Channel:       "!room:example.org",
		TriggerKey:    "tk-1",
		Title:         "OOMKilled",
		Resource:      "prod/harbor-registry",
		Verdict:       providers.VerdictActionRequired,
		CuratedURL:    "https://github.com/o/r/pull/42",
		RecalledEntry: "catalog/oom.md",
	}
	if got != want {
		t.Fatalf("contextFromStamp = %+v, want %+v", got, want)
	}
}

// TestMatrixEventStampTriggerKeyThroughDeliver pins Fix 2: Deliver writes the
// legacy triggerKeyContentField as cmp.Or(TriggerKey, Fingerprint), but (pre-fix)
// stamped the new threadContentField's trigger_key from TriggerKey alone — and
// contextFromContent decodes threadContentField FIRST, returning as soon as it
// decodes, so the legacy field was never even looked at on any event this
// notifier produces. A re-investigation (Request built with a Fingerprint but no
// TriggerKey — see internal/investigate/reinvestigate.go) would resolve an empty
// TriggerKey, silently breaking notify.matrix.feedback_reactions for exactly the
// operators who enabled nothing else on this branch.
//
// Driven through Matrix.Deliver's REAL posted content (round-tripped through
// JSON exactly as fetchEvent would decode it), not a hand-built fixture — the
// pre-existing "legacy trigger-key-only" fixture in TestMatrixContextFor carries
// a field combination Deliver never actually emits (threadContentField absent),
// which is why the shadowing was invisible.
func TestMatrixEventStampTriggerKeyThroughDeliver(t *testing.T) {
	const room = "!r:hs"
	tests := []struct {
		name        string
		inv         providers.Investigation
		wantTrigKey string
	}{
		{
			name:        "alert: TriggerKey set",
			inv:         providers.Investigation{Title: "OOMKilled", TriggerKey: "tk-1"},
			wantTrigKey: "tk-1",
		},
		{
			name:        "re-investigation: Fingerprint only, no TriggerKey",
			inv:         providers.Investigation{Title: "OOMKilled", Fingerprint: "fp9"},
			wantTrigKey: "fp9",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				_, _ = w.Write([]byte(`{"event_id":"$evt1"}`))
			}))
			defer srv.Close()

			m := NewMatrix(srv.URL, room, "tok")
			if err := m.Deliver(context.Background(), tc.inv); err != nil {
				t.Fatalf("Deliver: %v", err)
			}

			gotCtx, ok := contextFromContent(gotBody, "$evt1", room)
			if !ok {
				t.Fatalf("contextFromContent: not recognised as one of RunLore's own investigation messages: %v", gotBody)
			}
			if gotCtx.TriggerKey != tc.wantTrigKey {
				t.Errorf("TriggerKey = %q, want %q — the legacy trigger key is shadowed by the (fingerprint-blind) thread stamp", gotCtx.TriggerKey, tc.wantTrigKey)
			}
		})
	}
}

// TestStampForFallsBackToFingerprint pins the write-side half of Fix 2:
// stampFor must embed the same identity Deliver already writes into the legacy
// field (cmp.Or(TriggerKey, Fingerprint)), not TriggerKey alone.
func TestStampForFallsBackToFingerprint(t *testing.T) {
	got := stampFor(providers.Investigation{Fingerprint: "fp9"})
	if got.TriggerKey != "fp9" {
		t.Fatalf("stampFor TriggerKey = %q, want %q (fingerprint fallback)", got.TriggerKey, "fp9")
	}
}

// TestContextFromContentFallsBackToLegacyTriggerKey pins the read-side half of
// Fix 2, in isolation from stampFor: even a threadContentField stamp whose own
// trigger_key is empty must not shadow a non-empty legacy triggerKeyContentField
// sitting in the same content map. Defense in depth alongside the stampFor fix —
// covers a stamp built some other way than stampFor, now or later.
func TestContextFromContentFallsBackToLegacyTriggerKey(t *testing.T) {
	content := map[string]any{
		threadContentField:     map[string]any{"title": "OOMKilled"}, // trigger_key deliberately absent
		triggerKeyContentField: "tk-legacy",
	}
	got, ok := contextFromContent(content, "$evt1", "!r:hs")
	if !ok {
		t.Fatal("contextFromContent: want ok=true")
	}
	if got.TriggerKey != "tk-legacy" {
		t.Errorf("TriggerKey = %q, want %q", got.TriggerKey, "tk-legacy")
	}
	if got.Title != "OOMKilled" {
		t.Errorf("Title = %q, want the rest of the stamp preserved", got.Title)
	}
}

func TestMatrixReplyInThread(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"event_id":"$reply1"}`))
	}))
	defer srv.Close()

	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	if err := m.ReplyInThread(context.Background(), "$evt123", "!room:example.org", "📝 Noted"); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	rel, ok := body["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatalf("reply must carry m.relates_to: %v", body)
	}
	if rel["rel_type"] != "m.thread" {
		t.Errorf("rel_type = %v, want m.thread", rel["rel_type"])
	}
	if rel["event_id"] != "$evt123" {
		t.Errorf("event_id = %v, want the thread root $evt123", rel["event_id"])
	}
	if body["body"] != "📝 Noted" {
		t.Errorf("body = %v, want the reply text", body["body"])
	}
	if mt, _ := body["msgtype"].(string); mt != "m.notice" {
		t.Errorf("msgtype = %v, want m.notice (so clients don't render it as a human message)", body["msgtype"])
	}
	// Replies are plain text — no rich formatting, unlike Deliver's content. A
	// copy-paste from Deliver's content-building would silently carry these
	// over; catch it here.
	if _, ok := body["format"]; ok {
		t.Errorf("reply content must not carry format: %v", body)
	}
	if _, ok := body["formatted_body"]; ok {
		t.Errorf("reply content must not carry formatted_body: %v", body)
	}
}

// TestMatrixReplyInThreadRoutesToThePassedChannel pins the routing in
// ReplyInThread: room := cmp.Or(channel, m.roomID) must resolve to the passed
// channel when it's non-empty, even though the notifier was configured with a
// different room. TestMatrixReplyInThread alone can't catch a regression that
// drops the channel parameter (e.g. "room := m.roomID") because it constructs
// the notifier and calls ReplyInThread with the SAME room — this test uses
// deliberately divergent values and checks the request's URL path, which is
// where the Matrix send endpoint encodes the target room
// (/_matrix/client/v3/rooms/{roomID}/send/...).
func TestMatrixReplyInThreadRoutesToThePassedChannel(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"event_id":"$reply1"}`))
	}))
	defer srv.Close()

	m := NewMatrix(srv.URL, "!configured:example.org", "tok")
	if err := m.ReplyInThread(context.Background(), "$evt123", "!live:example.org", "📝 Noted"); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	if !strings.Contains(gotPath, "/rooms/!live:example.org/send/") {
		t.Fatalf("path = %q, want the reply routed to the passed channel !live:example.org", gotPath)
	}
	if strings.Contains(gotPath, "!configured") {
		t.Fatalf("path = %q, must not target the configured room when a different channel was passed", gotPath)
	}
}

// TestMatrixReplyInThreadFallsBackToConfiguredRoom proves an empty channel
// argument falls back to the notifier's configured room — the other half of
// cmp.Or(channel, m.roomID).
func TestMatrixReplyInThreadFallsBackToConfiguredRoom(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"event_id":"$reply1"}`))
	}))
	defer srv.Close()

	m := NewMatrix(srv.URL, "!configured:example.org", "tok")
	if err := m.ReplyInThread(context.Background(), "$evt123", "", "📝 Noted"); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	if !strings.Contains(gotPath, "/rooms/!configured:example.org/send/") {
		t.Fatalf("path = %q, want the reply to fall back to the configured room !configured:example.org", gotPath)
	}
}

func TestMatrixReplyInThreadReportsSendFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	m := NewMatrix(srv.URL, "!room:example.org", "tok")
	if err := m.ReplyInThread(context.Background(), "$evt123", "!room:example.org", "x"); err == nil {
		t.Fatal("a non-2xx send must be reported")
	}
}
