// SPDX-License-Identifier: Apache-2.0

// Package server exposes RunLore's HTTP endpoints (incident webhooks).
package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/source"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	ready        func() bool
	approvals    *action.Approvals // nil unless action mode "approve" is configured
	pauser       Pauser            // nil unless action mode "auto" is configured (kill-switch)
	feedback     FeedbackRecorder  // nil unless notify.slack.feedback_buttons is on (with an enabled ledger)
	token        string            // shared secret for the approval/control endpoints (required when actions enabled)
	slackSecret  string            // Slack signing secret; verifies interactive button clicks
	webhookToken string            // optional bearer token required on POST /webhook/alertmanager
	approvers    map[string]bool   // Slack user IDs permitted to approve actions (empty = none)

	metrics http.Handler // optional; GET /metrics (OTel Prometheus exposition)
	log     *slog.Logger
	handler http.Handler
}

// Pauser is the rung-3 auto-execution kill-switch.
type Pauser interface {
	Pause()
	Resume()
	Paused() bool
}

// FeedbackRecorder persists a human 👍/👎 rating on a delivered investigation —
// the ground-truth signal the learning loop weighs recalled knowledge by
// (implemented by *outcome.Ledger).
type FeedbackRecorder interface {
	Feedback(triggerKey, rating, user string, at time.Time) error
}

// Actions bundles the optional rung-2/rung-3 wiring: the approval queue, the auto
// kill-switch, the shared control token, the Slack signing secret, and the opt-in
// feedback recorder.
type Actions struct {
	Approvals    *action.Approvals
	Pauser       Pauser
	Feedback     FeedbackRecorder // opt-in 👍/👎 recording (notify.slack.feedback_buttons)
	Token        string
	SlackSecret  string
	WebhookToken string   // optional bearer token required on POST /webhook/alertmanager
	ApproverIDs  []string // Slack user IDs permitted to approve actions
}

// New builds a Server. ready reports whether this replica should serve; nil =
// always ready. The caller composes what readiness means (e.g. a warm catalog —
// since #264 deliberately NOT leadership, so every warm replica is Ready and
// Helm --wait/kstatus succeeds with replicaCount>1). acts (optional) enables the
// rung-2 approval endpoints + the rung-3 kill-switch, gated by acts.Token
// (X-Approval-Token). /healthz is liveness; /readyz is readiness (gated by the
// caller-supplied ready func). built + pipe wire the registered event sources:
// webhook sources are mounted at their paths and feed the ingest pipeline.
// metricsHandler (optional) serves OTel Prometheus metrics on GET /metrics when
// non-nil. fwd (optional) is the leader-forwarding policy: every WORK-BEARING
// route (source webhooks, slack interactions, action control) goes through it
// so a follower proxies the request to the leader; nil serves everything
// locally.
func New(ready func() bool, acts Actions, built []source.Built, pipe *source.Pipeline, metricsHandler http.Handler, fwd *Forward, log *slog.Logger) *Server {
	approvers := make(map[string]bool, len(acts.ApproverIDs))
	for _, id := range acts.ApproverIDs {
		approvers[id] = true
	}
	s := &Server{
		ready:     ready,
		approvals: acts.Approvals, pauser: acts.Pauser, feedback: acts.Feedback,
		token: acts.Token, slackSecret: acts.SlackSecret,
		webhookToken: acts.WebhookToken, approvers: approvers, metrics: metricsHandler, log: log,
	}
	mux := http.NewServeMux()
	// work marks a route as work-bearing: on a follower the request is proxied
	// to the leader (single hop, see Forward). Authentication happens on the
	// leader — headers and body are relayed byte-identical, so the bearer token
	// and HMAC signatures verify there exactly as they would here. The
	// local-only endpoints below (/healthz, /readyz, /metrics) are NEVER
	// wrapped: probes and scrapes are always about THIS replica.
	work := fwd.middleware // nil-receiver safe: identity when fwd == nil
	mux.Handle("POST /slack/interactions", work(http.HandlerFunc(s.handleSlackInteraction)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready != nil && !s.ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// The action-control endpoints are work-bearing too: the approval queue and
	// the auto kill-switch live in the LEADER process (registered by its
	// investigation loop), so a follower answering locally would list an empty
	// queue or pause a pauser nothing consults.
	mux.Handle("GET /actions", work(http.HandlerFunc(s.handleListActions)))
	mux.Handle("POST /actions/{id}/approve", work(http.HandlerFunc(s.handleApprove)))
	mux.Handle("POST /actions/{id}/reject", work(http.HandlerFunc(s.handleReject)))
	mux.Handle("POST /actions/pause", work(http.HandlerFunc(s.handlePause)))
	mux.Handle("POST /actions/resume", work(http.HandlerFunc(s.handleResume)))
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	source.MountWebhooks(mux, built, s.webhookAuthorized, pipe, work)
	// Wrap the whole mux so a panic in any handler (current or future) returns a
	// structured 500 and a logged stack instead of net/http's silent connection drop.
	s.handler = recoverPanic(mux, log)
	return s
}

// recoverPanic wraps next so a panic in any handler is recovered: it logs the
// panic value, request method/path/remote-addr, and a stack trace at error level,
// then writes a 500 if nothing has been written yet. Without this, net/http's
// per-connection recover closes the connection with no response and no log.
func recoverPanic(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if log != nil {
					log.Error("recovered from handler panic",
						"panic", rec,
						"method", r.Method,
						"path", r.URL.Path,
						"remote_addr", r.RemoteAddr,
						"stack", string(debug.Stack()),
					)
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if s.pauser == nil {
		http.Error(w, "auto-execution not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.pauser.Pause()
	s.log.Warn("auto-execution paused via kill-switch")
	_, _ = fmt.Fprintln(w, "auto-execution paused")
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if s.pauser == nil {
		http.Error(w, "auto-execution not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	s.pauser.Resume()
	s.log.Info("auto-execution resumed")
	_, _ = fmt.Fprintln(w, "auto-execution resumed")
}

// authorized enforces the approval token (constant-time compare). It FAILS
// CLOSED: with no token configured the control endpoints are denied, never open.
// (main refuses to start with actions enabled and an empty token, so a running
// rung-2/3 server always has one.)
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Approval-Token")), []byte(s.token)) == 1
}

func (s *Server) handleListActions(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "actions not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.approvals.List())
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "actions not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	act, err := s.approvals.Approve(r.Context(), id, "approve:http")
	if err != nil {
		s.log.Warn("action approval failed", "id", id, "err", err)
		code := http.StatusInternalServerError
		if errors.Is(err, action.ErrNoPending) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	s.log.Info("action approved and executed", "id", id, "op", act.Op)
	_, _ = fmt.Fprintf(w, "executed: %s %s\n", act.Op, act.Description)
}

func (s *Server) handleReject(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil {
		http.Error(w, "actions not enabled", http.StatusNotFound)
		return
	}
	if !s.authorized(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	if err := s.approvals.Reject(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, _ = fmt.Fprintln(w, "rejected")
}

// slackInteraction is the subset of the Block Kit interaction payload we need.
type slackInteraction struct {
	ResponseURL string `json:"response_url"`
	User        struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// handleSlackInteraction processes Block Kit button clicks: it verifies the Slack
// request signature, then approves (executing) / rejects the referenced action
// (privileged — approver allowlist) or records a 👍/👎 feedback rating
// (unprivileged — an opinion, not a cluster mutation), updating the message via
// response_url.
func (s *Server) handleSlackInteraction(w http.ResponseWriter, r *http.Request) {
	// The endpoint drives Approve/Reject on queued actions and the opt-in 👍/👎
	// feedback; it stays 404 unless at least one is wired, so a deployment that
	// enabled neither exposes no interactive callback at all. The signing secret
	// stays mandatory: signature verification is never optional.
	if (s.approvals == nil && s.feedback == nil) || s.slackSecret == "" {
		http.Error(w, "slack interactions not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !s.verifySlack(r.Header, body) {
		s.log.Warn("rejected slack interaction: bad signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	var p slackInteraction
	if err := json.Unmarshal([]byte(form.Get("payload")), &p); err != nil || len(p.Actions) == 0 {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	act := p.Actions[0]
	var msg string
	switch act.ActionID {
	case "runlore_approve":
		if s.approvals == nil {
			msg = "❌ approvals not enabled"
			break
		}
		// Authorize the *user*, not just the Slack workspace: the signature proves
		// origin, the allowlist proves the clicker may approve cluster mutations.
		if !s.approvers[p.User.ID] {
			msg = "❌ not authorized to approve (user not in approver allowlist)"
			s.log.Warn("slack approve denied: user not in allowlist", "user_id", p.User.ID, "user", p.User.Username)
			break
		}
		executed, aerr := s.approvals.Approve(r.Context(), act.Value, "approve:slack:"+p.User.ID)
		if aerr != nil {
			msg = "❌ approval failed: " + aerr.Error()
			s.log.Warn("slack approve failed", "id", act.Value, "user", p.User.Username, "err", aerr)
		} else {
			msg = fmt.Sprintf("✅ approved by @%s — executed: %s %s", p.User.Username, executed.Op, executed.Description)
			s.log.Info("slack approval executed", "id", act.Value, "user_id", p.User.ID, "user", p.User.Username, "op", executed.Op)
		}
	case "runlore_reject":
		if s.approvals == nil {
			msg = "❌ approvals not enabled"
			break
		}
		// Gate reject on the same approver allowlist as approve: a signature-valid but
		// unlisted user must not be able to cancel a pending remediation (denial-of-
		// remediation). Rejecting is the safe direction, but it's still a privileged
		// decision over a queued cluster action.
		if !s.approvers[p.User.ID] {
			msg = "❌ not authorized to reject (user not in approver allowlist)"
			s.log.Warn("slack reject denied: user not in allowlist", "user_id", p.User.ID, "user", p.User.Username)
			break
		}
		if rerr := s.approvals.Reject(act.Value); rerr != nil {
			msg = "⚠️ " + rerr.Error()
		} else {
			msg = fmt.Sprintf("🚫 rejected by @%s", p.User.Username)
		}
	case "runlore_feedback_up", "runlore_feedback_down":
		// Feedback is deliberately unprivileged (no approver allowlist): the
		// signature proves the workspace, and a rating is an opinion feeding the
		// learning loop, not a cluster mutation. Anti-gaming lives in the ledger —
		// one live vote per (TriggerKey, user), latest wins.
		if s.feedback == nil {
			msg = "⚠️ feedback recording not enabled (notify.slack.feedback_buttons is off)"
			break
		}
		rating := "up"
		if act.ActionID == "runlore_feedback_down" {
			rating = "down"
		}
		if ferr := s.feedback.Feedback(act.Value, rating, p.User.ID, time.Now()); ferr != nil {
			msg = "⚠️ recording feedback failed: " + ferr.Error()
			s.log.Warn("slack feedback failed", "key", act.Value, "err", ferr)
		} else {
			msg = fmt.Sprintf("🙏 feedback recorded (%s) — thanks @%s", rating, p.User.Username)
			s.log.Info("slack feedback recorded", "key", act.Value, "rating", rating, "user_id", p.User.ID, "user", p.User.Username)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK) // ack the click; update the message best-effort
	// Approve/reject replace the interaction message with the outcome; a feedback
	// ack must NOT — replacing would wipe the investigation the rating is about.
	replace := act.ActionID == "runlore_approve" || act.ActionID == "runlore_reject"
	s.updateSlack(r.Context(), p.ResponseURL, msg, replace)
}

// verifySlack validates the Slack request signature (HMAC-SHA256 over
// "v0:{ts}:{body}") and rejects stale (replayable) requests.
func (s *Server) verifySlack(h http.Header, body []byte) bool {
	tsStr := h.Get("X-Slack-Request-Timestamp")
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Since(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.slackSecret))
	_, _ = mac.Write([]byte("v0:" + tsStr + ":" + string(body)))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(h.Get("X-Slack-Signature")))
}

// updateSlack answers the interaction via its response_url: approve/reject
// overwrite the interaction message with the outcome (replaceOriginal=true),
// feedback appends an ephemeral note instead. The URL is attacker-influenceable
// (it arrives in the interaction payload), so it is restricted to https
// *.slack.com and posted with a bounded client — no SSRF to arbitrary internal
// services, no unbounded hang on http.DefaultClient.
func (s *Server) updateSlack(ctx context.Context, responseURL, text string, replaceOriginal bool) {
	if responseURL == "" {
		return
	}
	if u, err := url.Parse(responseURL); err != nil || u.Scheme != "https" ||
		(u.Hostname() != "slack.com" && !strings.HasSuffix(u.Hostname(), ".slack.com")) {
		s.log.Warn("refusing slack response_url: not an https *.slack.com host", "url", responseURL)
		return
	}
	body := slackResponseBody(text, replaceOriginal)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := httpx.SecureClient(10 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		s.log.Warn("slack response_url: request failed (best-effort)", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		s.log.Warn("slack response_url: non-2xx status (best-effort)", "status", resp.StatusCode)
	}
}

// slackResponseBody builds the response_url payload. A feedback ack
// (replaceOriginal=false) is additionally marked ephemeral so the thanks note is
// visible to the clicker only — the channel keeps the untouched investigation.
func slackResponseBody(text string, replaceOriginal bool) []byte {
	m := map[string]any{"replace_original": replaceOriginal, "text": text}
	if !replaceOriginal {
		m["response_type"] = "ephemeral"
	}
	b, _ := json.Marshal(m)
	return b
}

// Handler returns the HTTP handler (built once at construction; Go 1.22+ method routing).
func (s *Server) Handler() http.Handler {
	return s.handler
}

// webhookAuthorized checks the optional alert-webhook bearer token (constant-time).
// When no token is configured the webhook is open — Validate forbids that once
// actions.mode=auto, so an auto-executing server always authenticates it.
func (s *Server) webhookAuthorized(r *http.Request) bool {
	if s.webhookToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.webhookToken)) == 1
}
