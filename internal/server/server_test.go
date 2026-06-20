package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

type spyEnqueuer struct{ reqs []investigate.Request }

func (s *spyEnqueuer) Enqueue(r investigate.Request) { s.reqs = append(s.reqs, r) }

type recordExec struct{ ran []providers.Action }

func (r *recordExec) Execute(_ context.Context, a providers.Action) error {
	r.ran = append(r.ran, a)
	return nil
}

func TestActionsApprove(t *testing.T) {
	exec := &recordExec{}
	pol := action.New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{ReversibleOnly: true}})
	ap := action.NewApprovals(exec, pol, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := ap.Register(providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}})

	srv := New(&config.Config{}, &spyEnqueuer{}, nil, ap, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Missing token → 403, nothing executes.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/actions/"+id+"/approve", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no-token approve = %d, want 403", rr.Code)
	}
	if len(exec.ran) != 0 {
		t.Fatal("executor ran without a valid token")
	}

	// With token → executes.
	req := httptest.NewRequest(http.MethodPost, "/actions/"+id+"/approve", nil)
	req.Header.Set("X-Approval-Token", "secret")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", rr.Code)
	}
	if len(exec.ran) != 1 || exec.ran[0].Op != "suspend" {
		t.Fatalf("executor not run as expected: %+v", exec.ran)
	}
}

func testServerWith(enq investigate.Enqueuer) *Server {
	cfg := &config.Config{}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}},
	}
	return New(cfg, enq, nil, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServer() *Server { return testServerWith(&spyEnqueuer{}) }

func TestReadyz(t *testing.T) {
	cfg := &config.Config{}
	leader := false
	srv := New(cfg, &spyEnqueuer{}, func() bool { return leader }, nil, "", slog.New(slog.NewTextHandler(io.Discard, nil)))

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
