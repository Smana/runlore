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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine      *trigger.Engine
	enqueuer    investigate.Enqueuer
	ready       func() bool
	approvals   *action.Approvals // nil unless action mode "approve" is configured
	token       string            // optional shared secret for the approval endpoints
	slackSecret string            // Slack signing secret; verifies interactive button clicks
	log         *slog.Logger
	handler     http.Handler
}

// New builds a Server. ready reports whether this replica should serve (leadership);
// nil = always ready. approvals (optional) enables the human-approval endpoints for
// rung-2 actions; token (optional) is required as X-Approval-Token to use them.
// /healthz is liveness; /readyz is readiness (gated by ready, leader-only).
func New(cfg *config.Config, enq investigate.Enqueuer, ready func() bool, approvals *action.Approvals, token, slackSecret string, log *slog.Logger) *Server {
	s := &Server{engine: trigger.NewEngine(cfg.Triggers.Incidents), enqueuer: enq, ready: ready, approvals: approvals, token: token, slackSecret: slackSecret, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("POST /slack/interactions", s.handleSlackInteraction)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready != nil && !s.ready() {
			http.Error(w, "not ready (standby)", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /actions", s.handleListActions)
	mux.HandleFunc("POST /actions/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /actions/{id}/reject", s.handleReject)
	s.handler = mux
	return s
}

// authorized enforces the optional approval token (constant-time compare).
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return true
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
	act, err := s.approvals.Approve(r.Context(), id)
	if err != nil {
		s.log.Warn("action approval failed", "id", id, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
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
		Username string `json:"username"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

// handleSlackInteraction processes Block Kit approve/reject button clicks: it
// verifies the Slack request signature, approves (executing) or rejects the
// referenced action, and updates the message via response_url.
func (s *Server) handleSlackInteraction(w http.ResponseWriter, r *http.Request) {
	if s.approvals == nil || s.slackSecret == "" {
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
		executed, aerr := s.approvals.Approve(r.Context(), act.Value)
		if aerr != nil {
			msg = "❌ approval failed: " + aerr.Error()
			s.log.Warn("slack approve failed", "id", act.Value, "user", p.User.Username, "err", aerr)
		} else {
			msg = fmt.Sprintf("✅ approved by @%s — executed: %s %s", p.User.Username, executed.Op, executed.Description)
			s.log.Info("slack approval executed", "id", act.Value, "user", p.User.Username, "op", executed.Op)
		}
	case "runlore_reject":
		if rerr := s.approvals.Reject(act.Value); rerr != nil {
			msg = "⚠️ " + rerr.Error()
		} else {
			msg = fmt.Sprintf("🚫 rejected by @%s", p.User.Username)
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK) // ack the click; update the message best-effort
	s.updateSlack(r.Context(), p.ResponseURL, msg)
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

// updateSlack replaces the original message via the interaction response_url.
func (s *Server) updateSlack(ctx context.Context, responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"replace_original": true, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// Handler returns the HTTP handler (built once at construction; Go 1.22+ method routing).
func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	incidents, err := trigger.ParseAlertmanager(r.Body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	for _, inc := range incidents {
		d := s.engine.Decide(inc)
		s.log.Info("incident",
			"alert", inc.AlertName, "severity", inc.Severity, "namespace", inc.Namespace,
			"investigate", d.Investigate, "reason", d.Reason)
		if d.Investigate {
			s.enqueuer.Enqueue(investigate.FromIncident(inc))
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
