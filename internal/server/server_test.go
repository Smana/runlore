package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

func slackSign(secret, ts, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	_, _ = m.Write([]byte("v0:" + ts + ":" + body))
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

func TestSlackInteraction(t *testing.T) {
	exec := &recordExec{}
	pol := action.New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"apps"}}})
	ap := action.NewApprovals(exec, pol, audit.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := ap.Register(providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "web", Namespace: "apps"}})

	const secret = "shh"
	srv := New(&config.Config{}, &spyEnqueuer{}, nil, Actions{Approvals: ap, SlackSecret: secret, ApproverIDs: []string{"U1"}}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	payload := `{"user":{"id":"U1","username":"alice"},"actions":[{"action_id":"runlore_approve","value":"` + id + `"}]}`
	body := "payload=" + url.QueryEscape(payload)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	// Bad signature → 401, nothing executes.
	bad := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	bad.Header.Set("X-Slack-Request-Timestamp", ts)
	bad.Header.Set("X-Slack-Signature", "v0=deadbeef")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, bad)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", rr.Code)
	}
	if len(exec.ran) != 0 {
		t.Fatal("executed on an unverified request")
	}

	// Valid signature → 200, executes.
	good := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
	good.Header.Set("X-Slack-Request-Timestamp", ts)
	good.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, good)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid interaction = %d, want 200", rr.Code)
	}
	if len(exec.ran) != 1 || exec.ran[0].Op != "suspend" {
		t.Fatalf("approved action not executed: %+v", exec.ran)
	}
}

type spyEnqueuer struct{ reqs []investigate.Request }

func (s *spyEnqueuer) Enqueue(r investigate.Request) { s.reqs = append(s.reqs, r) }

type recordExec struct{ ran []providers.Action }

func (r *recordExec) Execute(_ context.Context, a providers.Action) error {
	r.ran = append(r.ran, a)
	return nil
}

func TestActionsApprove(t *testing.T) {
	exec := &recordExec{}
	pol := action.New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"apps"}}})
	ap := action.NewApprovals(exec, pol, audit.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := ap.Register(providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "web", Namespace: "apps"}})

	srv := New(&config.Config{}, &spyEnqueuer{}, nil, Actions{Approvals: ap, Token: "secret"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

type fakePauser struct{ paused bool }

func (f *fakePauser) Pause()       { f.paused = true }
func (f *fakePauser) Resume()      { f.paused = false }
func (f *fakePauser) Paused() bool { return f.paused }

func TestKillSwitch(t *testing.T) {
	p := &fakePauser{}
	srv := New(&config.Config{}, &spyEnqueuer{}, nil, Actions{Pauser: p, Token: "t"}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Without the token → 403, kill-switch untouched.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/actions/pause", nil))
	if rr.Code != http.StatusForbidden || p.paused {
		t.Fatalf("no-token pause = %d paused=%v, want 403 + untouched", rr.Code, p.paused)
	}
	// With the token → paused.
	req := httptest.NewRequest(http.MethodPost, "/actions/pause", nil)
	req.Header.Set("X-Approval-Token", "t")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !p.paused {
		t.Fatalf("pause = %d paused=%v, want 200 + paused", rr.Code, p.paused)
	}
	// Resume clears it.
	req = httptest.NewRequest(http.MethodPost, "/actions/resume", nil)
	req.Header.Set("X-Approval-Token", "t")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || p.paused {
		t.Fatalf("resume = %d paused=%v, want 200 + resumed", rr.Code, p.paused)
	}
}

func testServerWith(enq investigate.Enqueuer) *Server {
	cfg := &config.Config{}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Enabled: true,
		Match:   config.IncidentMatch{Severity: []string{"critical"}},
	}
	return New(cfg, enq, nil, Actions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testServer() *Server { return testServerWith(&spyEnqueuer{}) }

func TestReadyz(t *testing.T) {
	cfg := &config.Config{}
	leader := false
	srv := New(cfg, &spyEnqueuer{}, func() bool { return leader }, Actions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
