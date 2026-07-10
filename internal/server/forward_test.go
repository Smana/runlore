// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
	"gopkg.in/yaml.v3"
)

// alertBody is a minimal firing alert that passes the severity=critical trigger
// policy, so a locally-served webhook actually enqueues (proving "served here").
const alertBody = `{"alerts":[{"status":"firing","labels":{"alertname":"A","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"}]}`

// recordedReq captures what the fake leader received, so tests can assert the
// proxied request preserved method/path/query/headers/body.
type recordedReq struct {
	method, path, query, body string
	header                    http.Header
}

// fakeLeader is an httptest server standing in for the leader replica. It
// records every request and answers a recognizable status + body + header so
// the follower's passthrough of the leader's response is observable.
type fakeLeader struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []recordedReq
}

func newFakeLeader(t *testing.T) *fakeLeader {
	t.Helper()
	fl := &fakeLeader{}
	fl.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fl.mu.Lock()
		fl.got = append(fl.got, recordedReq{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			body: string(b), header: r.Header.Clone(),
		})
		fl.mu.Unlock()
		w.Header().Set("X-Answered-By", "leader")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted-by-leader"))
	}))
	t.Cleanup(fl.srv.Close)
	return fl
}

func (fl *fakeLeader) addr() string { return fl.srv.Listener.Addr().String() }

func (fl *fakeLeader) requests() []recordedReq {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return append([]recordedReq(nil), fl.got...)
}

// newForwardServer builds a full Server (alertmanager webhook mounted, metrics
// stub wired) with the given Forward — the follower/leader under test.
func newForwardServer(t *testing.T, fwd *Forward, enq investigate.Enqueuer) *Server {
	t.Helper()
	cfg := &config.Config{}
	cfg.Sources = map[string]yaml.Node{"alertmanager": {}}
	cfg.Triggers.Incidents = config.IncidentTrigger{
		Match: config.IncidentMatch{Severity: []string{"critical"}},
	}
	built, err := source.BuildEnabled(source.Deps{Cfg: cfg, Raw: cfg.Sources})
	if err != nil {
		t.Fatalf("build sources: %v", err)
	}
	pipe := source.NewPipeline(cfg, enq, nil, discardLog)
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("metrics-of-this-replica"))
	})
	return New(nil, Actions{}, built, pipe, metrics, fwd, discardLog)
}

func follower(addr func() string) *Forward {
	return &Forward{IsLeader: func() bool { return false }, LeaderAddr: addr, Log: discardLog}
}

// TestForwardFollowerProxiesToLeader is the core #264 behavior: a non-leader
// replica that receives a work-bearing request proxies it — method, path,
// query, headers, and byte-identical body — to the leader, marks the hop with
// X-Runlore-Forwarded, and relays the leader's response verbatim.
func TestForwardFollowerProxiesToLeader(t *testing.T) {
	leader := newFakeLeader(t)
	enq := &spyEnqueuer{}
	srv := newForwardServer(t, follower(leader.addr), enq)

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?src=am", strings.NewReader(alertBody))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("follower relayed status = %d, want 202", rr.Code)
	}
	if got := rr.Body.String(); got != "accepted-by-leader" {
		t.Errorf("relayed body = %q, want the leader's body", got)
	}
	if got := rr.Header().Get("X-Answered-By"); got != "leader" {
		t.Errorf("relayed header X-Answered-By = %q, want leader", got)
	}
	if len(enq.reqs) != 0 {
		t.Errorf("follower enqueued locally (%d) — work must only run on the leader", len(enq.reqs))
	}
	got := leader.requests()
	if len(got) != 1 {
		t.Fatalf("leader received %d requests, want 1", len(got))
	}
	r := got[0]
	if r.method != http.MethodPost || r.path != "/webhook/alertmanager" || r.query != "src=am" {
		t.Errorf("proxied request = %s %s?%s, want POST /webhook/alertmanager?src=am", r.method, r.path, r.query)
	}
	if r.body != alertBody {
		t.Errorf("proxied body not byte-identical:\n got %q\nwant %q", r.body, alertBody)
	}
	// Auth material must survive the hop: the leader verifies the bearer token /
	// HMAC signatures exactly as if the request had arrived there directly.
	if got := r.header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("proxied Authorization = %q, want Bearer tok", got)
	}
	if got := r.header.Get(ForwardedHeader); got != "1" {
		t.Errorf("proxied %s = %q, want 1 (single-hop marker)", ForwardedHeader, got)
	}
}

// TestForwardActionRoutesProxied: the action control endpoints are work-bearing
// (the approval queue and the auto kill-switch live in the leader process), so a
// follower must proxy them too.
func TestForwardActionRoutesProxied(t *testing.T) {
	leader := newFakeLeader(t)
	srv := newForwardServer(t, follower(leader.addr), &spyEnqueuer{})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/actions", nil))
	if rr.Code != http.StatusAccepted { // the fake leader answers 202 to everything
		t.Fatalf("GET /actions on follower = %d, want the leader's 202", rr.Code)
	}
	got := leader.requests()
	if len(got) != 1 || got[0].path != "/actions" {
		t.Fatalf("leader saw %+v, want one GET /actions", got)
	}
}

// TestForwardLoopHeaderRejected: forwarding is strictly single-hop. A request
// that already carries the forwarded marker but lands on a non-leader (stale
// holder view mid-failover) is answered 421 — never forwarded again, so two
// followers can never bounce a request between each other.
func TestForwardLoopHeaderRejected(t *testing.T) {
	leader := newFakeLeader(t)
	srv := newForwardServer(t, follower(leader.addr), &spyEnqueuer{})

	req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(alertBody))
	req.Header.Set(ForwardedHeader, "1")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("already-forwarded request on non-leader = %d, want 421", rr.Code)
	}
	if n := len(leader.requests()); n != 0 {
		t.Errorf("leader was contacted %d time(s) for a loop-marked request, want 0", n)
	}
}

// TestForwardUnknownLeader503: no holder known (none elected yet, or the holder
// runs an old build whose lease identity carries no IP) → shed with 503 +
// Retry-After. The webhook senders (Alertmanager, PagerDuty, Slack) retry.
func TestForwardUnknownLeader503(t *testing.T) {
	srv := newForwardServer(t, follower(func() string { return "" }), &spyEnqueuer{})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(alertBody)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unknown leader = %d, want 503", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("503 for unknown leader must carry Retry-After (senders retry)")
	}
}

// TestForwardDeadLeaderUnreachable503: the tracker can briefly point at a dead
// leader (mid-failover / takeover window) whose IP no longer answers. A dial
// failure means we never REACHED a live leader, so it must be shed exactly like
// "no leader known": 503 + Retry-After (which Alertmanager & co. retry on), NOT
// a 502 (which those senders treat as the upstream's answer and do not retry).
// It must also never serve the leader-only work locally on a non-leader.
func TestForwardDeadLeaderUnreachable503(t *testing.T) {
	enq := &spyEnqueuer{}
	// 127.0.0.1:1 — reserved port, connection refused immediately.
	srv := newForwardServer(t, follower(func() string { return "127.0.0.1:1" }), enq)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(alertBody)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreachable leader = %d, want 503 (retryable, not 502)", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("503 for unreachable leader must carry Retry-After (senders retry)")
	}
	if len(enq.reqs) != 0 {
		t.Error("follower served work locally when the leader was unreachable")
	}
}

// TestForwardStaleSelfServesLocally: during a takeover the tracker can still
// hold an identity whose pod-NAME is THIS pod's own — this pod just won the
// Lease but IsLeader() hasn't flipped, or (StatefulSet) the Lease still names a
// dead predecessor that reused our stable ordinal with a now-stale IP. The
// follower must NOT proxy (dialing a stale IP, or looping onto its own port):
// it serves the work locally, since by stable identity it owns it.
func TestForwardStaleSelfServesLocally(t *testing.T) {
	// A LIVE server at the tracked address, so a proxy WOULD succeed if wrongly
	// attempted — the guard is proven by work landing locally, not by a dial
	// error masking a missing guard.
	tracked := newFakeLeader(t)
	enq := &spyEnqueuer{}
	fwd := &Forward{
		IsLeader:   func() bool { return false }, // takeover window: not yet leader
		LeaderAddr: tracked.addr,
		LeaderName: func() string { return "runlore-0" },
		SelfName:   "runlore-0", // tracked holder's name == our own
		Log:        discardLog,
	}
	srv := newForwardServer(t, fwd, enq)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(alertBody)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("stale-self forward = %d, want 202 (served locally)", rr.Code)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("stale-self enqueued %d locally, want 1 (own work served here)", len(enq.reqs))
	}
	if n := len(tracked.requests()); n != 0 {
		t.Errorf("stale-self proxied %d request(s) to itself, want 0", n)
	}
}

// TestForwardLeaderServesLocally: the leader (or a replica with leader election
// disabled — same IsLeader()=true) never proxies; work runs in-process.
func TestForwardLeaderServesLocally(t *testing.T) {
	other := newFakeLeader(t) // would record if the leader wrongly proxied
	enq := &spyEnqueuer{}
	fwd := &Forward{IsLeader: func() bool { return true }, LeaderAddr: other.addr, Log: discardLog}
	srv := newForwardServer(t, fwd, enq)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(alertBody)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("leader local serve = %d, want 202", rr.Code)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("leader enqueued %d locally, want 1", len(enq.reqs))
	}
	if n := len(other.requests()); n != 0 {
		t.Errorf("leader proxied %d request(s), want 0", n)
	}
}

// TestForwardLocalEndpointsNeverProxied: /healthz, /readyz, and /metrics are
// about THIS replica (kubelet probes, Prometheus scrapes) — a follower must
// answer them itself, never proxy them to the leader.
func TestForwardLocalEndpointsNeverProxied(t *testing.T) {
	leader := newFakeLeader(t)
	srv := newForwardServer(t, follower(leader.addr), &spyEnqueuer{})

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s on follower = %d, want 200 (served locally)", path, rr.Code)
		}
		if rr.Header().Get("X-Answered-By") == "leader" {
			t.Errorf("GET %s was proxied to the leader", path)
		}
	}
	if got := len(leader.requests()); got != 0 {
		t.Fatalf("leader received %d request(s) for local-only endpoints, want 0", got)
	}
}

// TestForwardOversizedBodyRejected: the proxy bounds the forwarded body with
// the same 1 MiB cap local handlers enforce — forwarding must not become the
// way around the intake bound.
func TestForwardOversizedBodyRejected(t *testing.T) {
	leader := newFakeLeader(t)
	srv := newForwardServer(t, follower(leader.addr), &spyEnqueuer{})

	big := strings.NewReader(strings.Repeat("a", (1<<20)+1))
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", big))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized forwarded body = %d, want 413", rr.Code)
	}
	if n := len(leader.requests()); n != 0 {
		t.Errorf("oversized body reached the leader (%d request(s)), want 0", n)
	}
}

// TestForwardNilServesLocally: no Forward configured (CLI paths, tests) —
// everything serves locally, exactly the pre-#264 behavior.
func TestForwardNilServesLocally(t *testing.T) {
	enq := &spyEnqueuer{}
	srv := newForwardServer(t, nil, enq)

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/webhook/alertmanager", strings.NewReader(alertBody)))
	if rr.Code != http.StatusAccepted {
		t.Fatalf("nil Forward = %d, want 202", rr.Code)
	}
	if len(enq.reqs) != 1 {
		t.Fatalf("nil Forward enqueued %d, want 1 (local)", len(enq.reqs))
	}
}
