package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// captureServer starts a test HTTP server that captures the last request's body
// and Content-Type header, then responds with the given status.
func captureServer(t *testing.T, status int) (ts *httptest.Server, body func() []byte, ct func() string) {
	t.Helper()
	var captured []byte
	var capturedCT string
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		captured = b
		capturedCT = r.Header.Get("Content-Type")
		w.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)
	return ts, func() []byte { return captured }, func() string { return capturedCT }
}

func TestDeliverPOSTsJSON(t *testing.T) {
	ts, body, ct := captureServer(t, http.StatusOK)

	inv := providers.Investigation{
		Title:      "X",
		Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "ns", Name: "web"},
	}

	n := New(ts.URL)
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}

	if got := ct(); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var p struct {
		Title      string  `json:"title"`
		Confidence float64 `json:"confidence"`
		Namespace  string  `json:"namespace"`
		Resource   string  `json:"resource"`
		Text       string  `json:"text"`
	}
	if err := json.Unmarshal(body(), &p); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if p.Title != "X" {
		t.Errorf("title = %q, want X", p.Title)
	}
	if p.Confidence != 0.9 {
		t.Errorf("confidence = %f, want 0.9", p.Confidence)
	}
	if p.Text == "" {
		t.Error("text field must be non-empty")
	}
}

func TestDeliverErrorsOnNon2xx(t *testing.T) {
	ts, _, _ := captureServer(t, http.StatusInternalServerError)

	n := New(ts.URL)
	err := n.Deliver(context.Background(), providers.Investigation{Title: "fail"})
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestBuildRegisteredFromExtra(t *testing.T) {
	const envVar = "TEST_WH_URL"
	const testURL = "http://127.0.0.1:9999/hook" // unreachable; we only test Build, not Deliver

	// Build a yaml.Node from a YAML string — mirrors what the inline map produces
	// when the operator writes `webhook: {url_env: TEST_WH_URL}` under notify:.
	raw := []byte("url_env: " + envVar)
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	// doc is a DocumentNode; its first child is the MappingNode we want.
	node := doc.Content[0]

	cfg := &config.Config{}
	cfg.Notify.Extra = map[string]yaml.Node{
		"webhook": *node,
	}
	deps := notify.Deps{Cfg: cfg, Log: discardLog}

	// With the env var set — webhook Build should return a non-nil notifier.
	t.Setenv(envVar, testURL)
	m, err := notify.BuildEnabled(deps)
	if err != nil {
		t.Fatalf("BuildEnabled (env set): %v", err)
	}
	// Slack and Matrix are not configured in cfg → their Build returns nil.
	// Only webhook contributes.
	if m.Len() == 0 {
		t.Error("expected at least one notifier when env is set, got 0")
	}

	// With the env var unset — webhook Build returns nil; Multi must have length 0.
	os.Unsetenv(envVar)
	m2, err := notify.BuildEnabled(deps)
	if err != nil {
		t.Fatalf("BuildEnabled (env unset): %v", err)
	}
	if m2.Len() != 0 {
		t.Errorf("expected 0 notifiers when env is unset, got %d", m2.Len())
	}
}
