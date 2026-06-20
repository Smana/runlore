package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
)

type spyEnqueuer struct{ reqs []investigate.Request }

func (s *spyEnqueuer) Enqueue(r investigate.Request) { s.reqs = append(s.reqs, r) }

func testServerWith(enq investigate.Enqueuer) *Server {
	cfg := &config.Config{}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}},
	}
	return New(cfg, enq, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServer() *Server { return testServerWith(&spyEnqueuer{}) }

func TestReadyz(t *testing.T) {
	cfg := &config.Config{}
	leader := false
	srv := New(cfg, &spyEnqueuer{}, func() bool { return leader }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("standby readyz = %d, want 503", rr.Code)
	}
	leader = true
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("leader readyz = %d, want 200", rr.Code)
	}
	// liveness is always OK regardless of leadership
	leader = false
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rr.Code)
	}
}

func TestHandleAlertmanager(t *testing.T) {
	body := `{"alerts":[{"status":"firing","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rr.Code)
	}
}

func TestHandleAlertmanagerBadBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader("not json"))
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rr.Code)
	}
}

func TestHealthz(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestHandleAlertmanagerEnqueues(t *testing.T) {
	enq := &spyEnqueuer{}
	body := `{"alerts":[
	  {"status":"firing","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"},
	  {"status":"firing","labels":{"alertname":"B","severity":"warning","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp2"}
	]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	rr := httptest.NewRecorder()
	testServerWith(enq).Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rr.Code)
	}
	if len(enq.reqs) != 1 || enq.reqs[0].Title != "A" {
		t.Fatalf("want 1 enqueued (only critical A), got %v", enq.reqs)
	}
}
