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
	// No ApprovalID → plain text, no interactive blocks.
	m := slackMessage(providers.Investigation{Confidence: 0.5, Actions: []providers.Action{{Description: "x"}}})
	if _, ok := m["blocks"]; ok {
		t.Fatal("did not expect blocks without an ApprovalID")
	}
	// With ApprovalID → Block Kit Approve/Reject buttons carrying the id.
	m = slackMessage(providers.Investigation{Confidence: 0.9, Actions: []providers.Action{{Description: "suspend ks/apps", ApprovalID: "a7"}}})
	if _, ok := m["blocks"]; !ok {
		t.Fatal("expected interactive blocks for a pending action")
	}
	raw, _ := json.Marshal(m)
	for _, want := range []string{"runlore_approve", "runlore_reject", `"value":"a7"`, "Approve", "Reject"} {
		if !contains(string(raw), want) {
			t.Fatalf("rendered message missing %q:\n%s", want, raw)
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
