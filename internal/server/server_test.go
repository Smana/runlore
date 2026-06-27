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
	"github.com/Smana/runlore/internal/source"
	_ "github.com/Smana/runlore/internal/source/alertmanager" // self-registers the alertmanager webhook source
	"gopkg.in/yaml.v3"
)

// discardLog is the shared no-op logger for server tests.
var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// newAlertServer builds a Server whose alertmanager webhook feeds a pipeline with
// the given enqueuer + resolve callback. cfg.Sources["alertmanager"] must be present
// for the alertmanager source to be built and mounted.
func newAlertServer(cfg *config.Config, enq investigate.Enqueuer, resolve source.ResolveFunc) *Server {
	built, err := source.BuildEnabled(source.Deps{Cfg: cfg, Raw: cfg.Sources})
	if err != nil {
		panic(err)
	}
	pipe := source.NewPipeline(cfg, enq, resolve, discardLog)
	return New(cfg, nil, Actions{}, built, pipe, nil, discardLog)
}

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
	srv := New(&config.Config{}, nil, Actions{Approvals: ap, SlackSecret: secret, ApproverIDs: []string{"U1"}}, nil, nil, nil, discardLog)

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

// TestSlackRejectRequiresApprover locks down F3: a signature-valid but unlisted
// user must not be able to cancel a pending remediation (denial-of-remediation).
func TestSlackRejectRequiresApprover(t *testing.T) {
	exec := &recordExec{}
	pol := action.New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"apps"}}})
	ap := action.NewApprovals(exec, pol, audit.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id := ap.Register(providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "web", Namespace: "apps"}})

	const secret = "shh"
	srv := New(&config.Config{}, nil, Actions{Approvals: ap, SlackSecret: secret, ApproverIDs: []string{"U1"}}, nil, nil, nil, discardLog)

	reject := func(userID string) {
		payload := `{"user":{"id":"` + userID + `","username":"x"},"actions":[{"action_id":"runlore_reject","value":"` + id + `"}]}`
		body := "payload=" + url.QueryEscape(payload)
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("reject by %s = %d, want 200 (click ack)", userID, rr.Code)
		}
	}

	// Unlisted user: the click is acked (200) but the action must remain pending.
	reject("U2")
	if len(ap.List()) != 1 {
		t.Fatalf("unlisted user cancelled a pending action (denial-of-remediation); pending=%d, want 1", len(ap.List()))
	}
	// The approver can reject — the action is dropped.
	reject("U1")
	if len(ap.List()) != 0 {
		t.Fatalf("approver reject did not drop the pending action; pending=%d, want 0", len(ap.List()))
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

	srv := New(&config.Config{}, nil, Actions{Approvals: ap, Token: "secret"}, nil, nil, nil, discardLog)

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
	srv := New(&config.Config{}, nil, Actions{Pauser: p, Token: "t"}, nil, nil, nil, discardLog)

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
	cfg.Sources = map[string]yaml.Node{"alertmanager": {}}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Match: config.IncidentMatch{Severity: []string{"critical"}},
	}
	return newAlertServer(cfg, enq, nil)
}

func testServer() *Server { return testServerWith(&spyEnqueuer{}) }

func TestReadyz(t *testing.T) {
	cfg := &config.Config{}
	leader := false
	srv := New(cfg, func() bool { return leader }, Actions{}, nil, nil, nil, discardLog)

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

func TestHandleAlertmanagerOversizedBody(t *testing.T) {
	// A body past the 1 MiB cap must be rejected with 413 before it is fully
	// decoded — the alert payload flows into the LLM prompt, so an unbounded POST
	// must not force unbounded allocation.
	big := strings.Repeat("a", (1<<20)+1)
	body := `{"alerts":[{"status":"firing","labels":{"alertname":"` + big + `"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(body))
	rr := httptest.NewRecorder()
	testServer().Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", rr.Code)
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
