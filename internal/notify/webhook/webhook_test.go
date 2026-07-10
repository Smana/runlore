// SPDX-License-Identifier: Apache-2.0

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
	"time"

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
		Title:          "X",
		Confidence:     0.9,
		Resource:       providers.Workload{Namespace: "ns", Name: "web"},
		Verdict:        providers.VerdictActionRequired,
		Severity:       "critical",
		Cluster:        "eu-west-1",
		Environment:    "prod",
		Tenant:         "platform",
		AlertName:      "HarborDown",
		StartedAt:      time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
		Occurrences:    3,
		PrevCuratedURL: "https://kb.example/entry/prev",
		RuledOut:       []string{"network partition disproven"},
		DataGaps:       []string{"disk metrics unavailable"},
	}

	n := New(ts.URL)
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver returned error: %v", err)
	}

	if got := ct(); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var p struct {
		Title          string   `json:"title"`
		Confidence     float64  `json:"confidence"`
		Namespace      string   `json:"namespace"`
		Resource       string   `json:"resource"`
		Text           string   `json:"text"`
		Verdict        string   `json:"verdict"`
		Severity       string   `json:"severity"`
		Cluster        string   `json:"cluster"`
		Environment    string   `json:"environment"`
		Tenant         string   `json:"tenant"`
		AlertName      string   `json:"alert_name"`
		StartedAt      string   `json:"started_at"`
		Occurrences    int      `json:"occurrences"`
		PrevCuratedURL string   `json:"prev_curated_url"`
		RuledOut       []string `json:"ruled_out"`
		DataGaps       []string `json:"data_gaps"`
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
	if p.Verdict != string(providers.VerdictActionRequired) {
		t.Errorf("verdict = %q, want %q", p.Verdict, providers.VerdictActionRequired)
	}
	if p.Severity != "critical" || p.Cluster != "eu-west-1" || p.Tenant != "platform" || p.AlertName != "HarborDown" {
		t.Errorf("metadata mismatch: severity=%q cluster=%q tenant=%q alert=%q", p.Severity, p.Cluster, p.Tenant, p.AlertName)
	}
	if p.Environment != "prod" {
		t.Errorf("environment = %q, want prod", p.Environment)
	}
	if p.StartedAt != "2026-07-03T10:00:00Z" {
		t.Errorf("started_at = %q, want RFC3339 UTC", p.StartedAt)
	}
	if p.Occurrences != 3 {
		t.Errorf("occurrences = %d, want 3", p.Occurrences)
	}
	if p.PrevCuratedURL != "https://kb.example/entry/prev" {
		t.Errorf("prev_curated_url = %q", p.PrevCuratedURL)
	}
	if len(p.RuledOut) != 1 || len(p.DataGaps) != 1 {
		t.Errorf("ruled_out=%v data_gaps=%v, want one each", p.RuledOut, p.DataGaps)
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

func TestWebhookDeliverPriorKnowledge(t *testing.T) {
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
	}))
	defer srv.Close()
	n := New(srv.URL)
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8, Occurrences: 3,
		Prior: &providers.PriorKnowledge{Cause: "c", Resolution: "r", EntryPath: "incidents/e.md", Recalls: 3, Resolved: 2},
	}
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got.Prior == nil || got.Prior.Cause != "c" || got.Prior.Resolution != "r" ||
		got.Prior.EntryPath != "incidents/e.md" || got.Prior.Recalls != 3 || got.Prior.Resolved != 2 {
		t.Errorf("prior payload = %+v", got.Prior)
	}
}

func TestWebhookDeliverMatchedKnowledge(t *testing.T) {
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
	}))
	defer srv.Close()
	n := New(srv.URL)
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8,
		MatchedKnowledge: &providers.MatchedEntry{Path: "runbooks/harbor.md", Title: "Harbor probe runbook", URL: "https://kb/runbooks/harbor.md", Score: 6.2},
	}
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got.MatchedKnowledge == nil {
		t.Fatal("matched_knowledge payload missing")
	}
	if got.MatchedKnowledge.Path != "runbooks/harbor.md" || got.MatchedKnowledge.Title != "Harbor probe runbook" ||
		got.MatchedKnowledge.URL != "https://kb/runbooks/harbor.md" || got.MatchedKnowledge.Score != 6.2 {
		t.Errorf("matched payload = %+v", got.MatchedKnowledge)
	}
}

// TestWebhookMatchedKnowledgeSuppressedByPrior mirrors the shared-text guard: when
// Prior is set, matched_knowledge is omitted so the structured field never disagrees
// with the payload's `text` (recurrence already covers the "seen before" case).
func TestWebhookMatchedKnowledgeSuppressedByPrior(t *testing.T) {
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
	}))
	defer srv.Close()
	n := New(srv.URL)
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8, Occurrences: 2,
		Prior:            &providers.PriorKnowledge{Cause: "c"},
		MatchedKnowledge: &providers.MatchedEntry{Path: "p.md", Title: "R", Score: 6},
	}
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got.MatchedKnowledge != nil {
		t.Errorf("matched_knowledge must be omitted when Prior is set, got %+v", got.MatchedKnowledge)
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
	_ = os.Unsetenv(envVar)
	m2, err := notify.BuildEnabled(deps)
	if err != nil {
		t.Fatalf("BuildEnabled (env unset): %v", err)
	}
	if m2.Len() != 0 {
		t.Errorf("expected 0 notifiers when env is unset, got %d", m2.Len())
	}
}
