package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
