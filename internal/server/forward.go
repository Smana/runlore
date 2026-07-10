// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/httpx"
)

// ForwardedHeader marks a request a follower has already proxied once to the
// replica it believed was the leader. A request carrying it is NEVER forwarded
// again: if the receiver isn't the leader either (a stale holder view during a
// failover, or two followers briefly pointing at each other), it answers 421
// instead of hopping on — forwarding is strictly single-hop, loops are
// structurally impossible.
const ForwardedHeader = "X-Runlore-Forwarded"

const (
	// forwardBodyCap bounds a forwarded request body to the same 1 MiB every
	// local work handler enforces (webhook body cap, slack-interaction cap) —
	// the proxy hop must not become the way around the intake bound.
	forwardBodyCap = 1 << 20
	// forwardTimeout bounds the proxied round trip. Kept under the serving
	// WriteTimeout (30s, NewHTTPServer) so a follower stuck on a slow leader
	// still answers its own client instead of having the connection cut.
	forwardTimeout = 25 * time.Second
	// forwardRetryAfter is the Retry-After hint (seconds) on the 503 answers
	// (no leader known / tracked leader unreachable): a leader handoff settles
	// within the lease window (15s lease / 2s retry), so a short client retry
	// usually lands on an elected leader.
	forwardRetryAfter = "5"
)

// hopHeaders are the hop-by-hop headers (RFC 9110 §7.6.1) a proxy must not
// pass along; everything else — notably Authorization and the Slack/PagerDuty
// signature headers — is preserved so the leader authenticates the forwarded
// request exactly as if it had arrived there directly.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Forward routes work-bearing requests from a non-leader replica to the
// current leader. Since #264 /readyz no longer reflects leadership — every
// warm replica is Ready and the Service may route to any of them — so "only
// the leader's queue processes work" is preserved here instead of by
// readiness-based routing: a follower that receives a work-bearing request
// (source webhooks, slack interactions, action control) proxies it to the
// leader it learned from the leader-election Lease. A nil *Forward (CLI,
// tests, single-replica without leader election) serves everything locally.
type Forward struct {
	// IsLeader reports whether THIS replica currently leads. With leader
	// election disabled the caller pins it true, so everything serves locally.
	IsLeader func() bool
	// LeaderAddr returns the "host:port" of the current lease holder, or ""
	// when no holder is known yet or the holder's identity carries no routable
	// IP (an old-format identity from a pre-#264 replica during a
	// mixed-version rollout) — the request is then shed with 503 + Retry-After,
	// matching the pre-#264 behavior of a standby that received traffic.
	LeaderAddr func() string
	// SelfName is THIS replica's pod name (POD_NAME / hostname), i.e. the
	// name half of its own lease identity. Paired with LeaderName it guards the
	// takeover self-race: see the check in middleware. Empty disables the guard
	// (single-replica, tests) — safe, since without a name to match on there is
	// no way to mistake the tracked holder for ourselves.
	SelfName string
	// LeaderName returns the pod-name part of the tracked holder identity (""
	// before the first OnNewLeader). Compared against SelfName to detect the
	// tracker briefly pointing at THIS pod (or a dead predecessor sharing our
	// stable StatefulSet name) so we serve locally instead of proxying to
	// ourselves.
	LeaderName func() string
	// Client posts the proxied request. nil defaults to a bounded
	// httpx.SecureClient — never http.DefaultClient (unbounded hang).
	Client *http.Client
	Log    *slog.Logger
}

// middleware wraps a work-bearing handler with the leader/follower routing
// decision. Nil-receiver safe: with no Forward configured the handler serves
// locally, exactly the pre-#264 behavior. Local-only endpoints (/healthz,
// /readyz, /metrics) are deliberately NOT wrapped by the caller — probes and
// scrapes are always about THIS replica.
func (f *Forward) middleware(next http.Handler) http.Handler {
	if f == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.IsLeader != nil && f.IsLeader() {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(ForwardedHeader) != "" {
			// Already proxied once and this replica STILL isn't the leader:
			// the sender's holder view is stale (mid-failover). 421 instead of
			// re-forwarding keeps routing single-hop and loop-free; the
			// original sender retries against the Service and lands on the
			// settled leader.
			http.Error(w, "not the leader (request already forwarded once)", http.StatusMisdirectedRequest)
			return
		}
		if f.SelfName != "" && f.LeaderName != nil && f.LeaderName() == f.SelfName {
			// Takeover self-race: IsLeader() is still false, yet the tracked
			// holder's pod NAME is our own. Either this replica just won the
			// Lease and OnStartedLeading hasn't flipped IsLeader() yet, or (a
			// StatefulSet) the Lease still names a dead predecessor that reused
			// our stable ordinal with a now-unroutable IP. Proxying would dial a
			// stale IP or loop a request onto our own serve port; serve locally
			// instead — by stable identity WE are the pod that owns this work.
			next.ServeHTTP(w, r)
			return
		}
		addr := ""
		if f.LeaderAddr != nil {
			addr = f.LeaderAddr()
		}
		if addr == "" {
			// No routable leader: none elected yet, or the holder runs a
			// pre-#264 build whose lease identity has no IP. Shed with a retry
			// hint — every work-bearing sender (Alertmanager, PagerDuty,
			// Slack) retries on 5xx.
			w.Header().Set("Retry-After", forwardRetryAfter)
			http.Error(w, "no leader known; retry", http.StatusServiceUnavailable)
			return
		}
		f.proxy(w, r, addr)
	})
}

// proxy relays r to the leader at addr (plain http: pod-to-pod inside the
// cluster on the shared serve port) and copies the response back verbatim.
func (f *Forward) proxy(w http.ResponseWriter, r *http.Request, addr string) {
	// Read the body up front, bounded like local handling, so the proxied
	// request carries a Content-Length and an oversized payload surfaces as a
	// clean 413 here instead of a mid-stream break on the leader.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, forwardBodyCap))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u := *r.URL
	u.Scheme = "http"
	u.Host = addr
	// gosec G704 (SSRF) is a false positive here: this is a deliberate reverse
	// proxy whose DESTINATION is never request-controlled — addr comes from the
	// leader-election Lease identity (a net.ParseIP-validated pod IP, written
	// via authenticated API-server access) plus this replica's own serve port.
	// Only the path/query are relayed, which is the entire point of forwarding.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body)) //nolint:gosec // G704: fixed in-cluster host from the Lease, see above
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Preserve every end-to-end header: the bearer token and the Slack /
	// PagerDuty HMAC signatures (computed over the byte-identical body relayed
	// above) must verify on the leader exactly as they would have locally.
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	req.Header.Set(ForwardedHeader, "1")
	client := f.Client
	if client == nil {
		client = httpx.SecureClient(forwardTimeout)
	}
	resp, err := client.Do(req) //nolint:gosec // G704: same request as above — leader-only destination, never request-controlled
	if err != nil {
		// A client.Do error means NO HTTP response arrived — a dial/connect/
		// timeout failure to REACH the tracked leader, never a status returned
		// by a live one. The holder view is briefly stale (the leader just died
		// and the tracker hasn't observed a successor yet), so this is
		// indistinguishable from "no leader known": shed with 503 + Retry-After,
		// the documented retryable contract that every work-bearing sender
		// (Alertmanager, PagerDuty, Slack) honors. A 502 here would be WRONG —
		// those senders treat a bad-gateway as the upstream's own answer and do
		// NOT retry, so the alert would be lost during the takeover window.
		if f.Log != nil {
			f.Log.Warn("leader forward failed", "leader", addr,
				"method", r.Method, "path", r.URL.Path, "err", err)
		}
		w.Header().Set("Retry-After", forwardRetryAfter)
		http.Error(w, "leader unreachable; retry", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if f.Log != nil {
		f.Log.Debug("forwarded to leader", "leader", addr,
			"method", r.Method, "path", r.URL.Path, "status", resp.StatusCode)
	}
	hdr := w.Header()
	for k, vv := range resp.Header {
		for _, v := range vv {
			hdr.Add(k, v)
		}
	}
	for _, h := range hopHeaders {
		hdr.Del(h)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
