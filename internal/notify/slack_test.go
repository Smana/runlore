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
)

func TestSlackDeliver(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewSlack(srv.URL).Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	text, _ := got["text"].(string)
	if text == "" || !contains(text, "flux rollback hr/harbor") {
		t.Fatalf("unexpected slack payload: %v", got)
	}
}

func TestSlackBotDeliver(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/chat.postMessage" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	if err := bot.Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if auth != "Bearer xoxb-test" {
		t.Fatalf("auth header = %q, want Bearer xoxb-test", auth)
	}
	if got["channel"] != "C123" {
		t.Fatalf("channel = %v, want C123", got["channel"])
	}
	if text, _ := got["text"].(string); !contains(text, "flux rollback hr/harbor") {
		t.Fatalf("unexpected payload: %v", got)
	}
}

func TestSlackBotAPIError(t *testing.T) {
	// chat.postMessage returns HTTP 200 with ok:false on logical failures
	// (e.g. not_in_channel) — Deliver must surface that as an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
	}))
	defer srv.Close()

	bot := &SlackBot{token: "xoxb-test", channel: "C123", baseURL: srv.URL, http: srv.Client()}
	err := bot.Deliver(context.Background(), sampleInvestigation())
	if err == nil || !contains(err.Error(), "not_in_channel") {
		t.Fatalf("expected not_in_channel error, got %v", err)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// failingNotifier always errors.
type failingNotifier struct{}

func (failingNotifier) Deliver(context.Context, providers.Investigation) error {
	return io.ErrUnexpectedEOF
}

func TestSlackMessageButtons(t *testing.T) {
	// No ApprovalID → rich blocks, but no interactive Approve/Reject elements.
	m := slackMessage(providers.Investigation{Confidence: 0.5, RootCauses: []providers.Hypothesis{{Summary: "x"}}, Actions: []providers.Action{{Description: "x"}}})
	raw, _ := json.Marshal(m)
	if contains(string(raw), "runlore_approve") {
		t.Fatalf("did not expect Approve/Reject buttons without an ApprovalID:\n%s", raw)
	}
	// With ApprovalID → Block Kit Approve/Reject buttons carrying the id.
	m = slackMessage(providers.Investigation{Confidence: 0.9, Actions: []providers.Action{{Description: "suspend ks/apps", ApprovalID: "a7"}}})
	if _, ok := m["blocks"]; !ok {
		t.Fatal("expected interactive blocks for a pending action")
	}
	raw, _ = json.Marshal(m)
	for _, want := range []string{"runlore_approve", "runlore_reject", `"value":"a7"`, "Approve", "Reject"} {
		if !contains(string(raw), want) {
			t.Fatalf("rendered message missing %q:\n%s", want, raw)
		}
	}
}

func TestSlackBlocksLayout(t *testing.T) {
	inv := providers.Investigation{
		Title:      "VictoriaTracesDown",
		Confidence: 0, // model left top-level at 0 …
		RootCauses: []providers.Hypothesis{{
			Summary: "crds sync broke victoria-traces", Confidence: 0.8, // … but a root cause is 80%
			ChangeRef: "crds@abc123", Evidence: []string{"reconcile failed", "stalled resources"},
			SuggestedAction: "flux rollback hr/victoria-traces", Reversible: true,
		}},
		Unresolved: []string{"why the migration stalled"},
		CuratedURL: "https://github.com/o/r/issues/9",
	}
	blocks := slackBlocks(inv)
	if blocks[0]["type"] != "header" {
		t.Fatalf("first block must be a header, got %v", blocks[0]["type"])
	}
	raw, _ := json.Marshal(blocks)
	s := string(raw)
	// Headline confidence falls back to the top root cause (80%, High), not the 0% top-level.
	for _, want := range []string{"VictoriaTracesDown", "High confidence", "80%", "What changed", "crds@abc123",
		"Suggested next steps", "flux rollback hr/victoria-traces", "(reversible)", "Open questions", "view entry"} {
		if !contains(s, want) {
			t.Fatalf("blocks missing %q:\n%s", want, s)
		}
	}
}

func TestEscapeMrkdwn(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "no specials here", "no specials here"},
		{"link_injection", "<https://evil.example|click here to remediate>", "&lt;https://evil.example|click here to remediate&gt;"},
		{"amp_first", "a & b < c > d", "a &amp; b &lt; c &gt; d"},
		{"pre_escaped_stays_literal", "&lt;", "&amp;lt;"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeMrkdwn(tc.in); got != tc.want {
				t.Errorf("escapeMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// mrkdwnTexts collects every mrkdwn text the blocks would send to Slack, so
// tests assert on what Slack will actually parse (json.Marshal would obscure
// this by encoding < and > as </>).
func mrkdwnTexts(blocks []map[string]any) []string {
	var out []string
	grab := func(v any) {
		txt, _ := v.(map[string]any)
		if txt["type"] == "mrkdwn" {
			if s, _ := txt["text"].(string); s != "" {
				out = append(out, s)
			}
		}
	}
	for _, b := range blocks {
		grab(b["text"])
		if els, _ := b["elements"].([]map[string]any); els != nil {
			for _, el := range els {
				grab(el)
			}
		}
	}
	return out
}

// TestSlackBlocksEscapeUntrustedText proves that model/tool-derived fields
// (summaries, evidence quoting cluster logs, change refs, action descriptions,
// unresolved items) cannot inject Slack mrkdwn — most importantly the
// <url|text> link form, a phishing vector in incident notifications — while the
// formatter's own markup (bold, code, the KB link it constructs) keeps working.
func TestSlackBlocksEscapeUntrustedText(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{
			Summary:         "summary with <b> tag",
			Confidence:      0.9,
			ChangeRef:       "chart@<v2>",
			Evidence:        []string{"error: <https://evil.example|click here to remediate>"},
			SuggestedAction: "restart & verify",
			Reversible:      true,
		}},
		Unresolved: []string{"why <img> appears in logs"},
		Actions:    []providers.Action{{Description: "scale down <deploy>", ApprovalID: "a1"}},
		CuratedURL: "https://github.com/o/r/issues/9",
	}
	joined := strings.Join(mrkdwnTexts(slackBlocks(inv)), "\n")

	// The hostile log line must render inert, never as a clickable link.
	if strings.Contains(joined, "<https://evil.example") {
		t.Fatalf("hostile evidence rendered as live mrkdwn link:\n%s", joined)
	}
	for _, want := range []string{
		"&lt;https://evil.example|click here to remediate&gt;",
		"summary with &lt;b&gt; tag",
		"chart@&lt;v2&gt;",
		"restart &amp; verify",
		"why &lt;img&gt; appears in logs",
		"scale down &lt;deploy&gt;",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("blocks missing escaped untrusted text %q:\n%s", want, joined)
		}
	}
	// Formatter-emitted markup must keep working: bold rank, code confidence,
	// reversibility italics, and the KB link the formatter constructs itself.
	for _, want := range []string{"*1. ", "`90%`", "_(reversible)_", "<https://github.com/o/r/issues/9|view entry>"} {
		if !strings.Contains(joined, want) {
			t.Errorf("blocks missing formatter-emitted markup %q:\n%s", want, joined)
		}
	}
}

// TestSlackMessageFallbackEscaped proves the plain-text fallback (Slack parses
// it as mrkdwn for notifications) escapes untrusted content too, while the
// scaffolding Format emits (bold headers) survives untouched.
func TestSlackMessageFallbackEscaped(t *testing.T) {
	inv := sampleInvestigation()
	inv.RootCauses[0].Evidence = append(inv.RootCauses[0].Evidence, "<https://evil.example|click here to remediate>")
	text, _ := slackMessage(inv)["text"].(string)
	if strings.Contains(text, "<https://evil.example") {
		t.Fatalf("fallback text carries live mrkdwn link:\n%s", text)
	}
	if !strings.Contains(text, "&lt;https://evil.example|click here to remediate&gt;") {
		t.Fatalf("fallback text missing escaped evidence:\n%s", text)
	}
	if !strings.Contains(text, "*Investigation*") {
		t.Fatalf("fallback text lost formatter scaffolding:\n%s", text)
	}
}

func TestMultiBestEffort(t *testing.T) {
	var delivered int
	ok := notifierFunc(func(context.Context, providers.Investigation) error { delivered++; return nil })
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), failingNotifier{}, ok)
	// Best-effort: a failing notifier must not stop the good one — but the failure IS
	// surfaced to the caller (joined), so partial delivery is detectable.
	if err := m.Deliver(context.Background(), sampleInvestigation()); err == nil {
		t.Fatal("Multi.Deliver should surface the failing sink's error")
	}
	if delivered != 1 {
		t.Fatalf("good notifier called %d times, want 1", delivered)
	}
}

// notifierFunc adapts a func to providers.Notifier.
type notifierFunc func(context.Context, providers.Investigation) error

func (f notifierFunc) Deliver(ctx context.Context, inv providers.Investigation) error {
	return f(ctx, inv)
}
