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
	"strconv"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/coalesce"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/trigger"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine       *trigger.Engine
	enqueuer     investigate.Enqueuer
	ready        func() bool
	approvals    *action.Approvals // nil unless action mode "approve" is configured
	pauser       Pauser            // nil unless action mode "auto" is configured (kill-switch)
	token        string            // shared secret for the approval/control endpoints (required when actions enabled)
	slackSecret  string            // Slack signing secret; verifies interactive button clicks
	webhookToken string            // optional bearer token required on POST /webhook/alertmanager
	approvers    map[string]bool   // Slack user IDs permitted to approve actions (empty = none)

	coalescer     *coalesce.Coalescer // optional; folds correlated alerts before enqueueing
	otelMetrics   *telemetry.Metrics  // optional; nil-safe OTel counters
	outcomeLedger *outcome.Ledger     // optional; records investigation outcomes (resolved alerts)

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

// Actions bundles the optional rung-2/rung-3 wiring: the approval queue, the auto
// kill-switch, the shared control token, and the Slack signing secret.
type Actions struct {
	Approvals    *action.Approvals
	Pauser       Pauser
	Token        string
	SlackSecret  string
	WebhookToken string   // optional bearer token required on POST /webhook/alertmanager
	ApproverIDs  []string // Slack user IDs permitted to approve actions
}

// New builds a Server. ready reports whether this replica should serve; nil =
// always ready. The caller composes what readiness means (e.g. leadership AND a
// warm catalog). acts (optional) enables the rung-2 approval endpoints + the
// rung-3 kill-switch, gated by acts.Token (X-Approval-Token). /healthz is liveness;
// /readyz is readiness (gated by the caller-supplied ready func). metricsHandler
// (optional) serves OTel Prometheus metrics on GET /metrics when non-nil.
func New(cfg *config.Config, enq investigate.Enqueuer, ready func() bool, acts Actions, metricsHandler http.Handler, log *slog.Logger) *Server {
	approvers := make(map[string]bool, len(acts.ApproverIDs))
	for _, id := range acts.ApproverIDs {
		approvers[id] = true
	}
	s := &Server{
		engine: trigger.NewEngine(cfg.Triggers.Incidents), enqueuer: enq, ready: ready,
		approvals: acts.Approvals, pauser: acts.Pauser, token: acts.Token, slackSecret: acts.SlackSecret,
		webhookToken: acts.WebhookToken, approvers: approvers, metrics: metricsHandler, log: log,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("POST /slack/interactions", s.handleSlackInteraction)
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
	mux.HandleFunc("GET /actions", s.handleListActions)
	mux.HandleFunc("POST /actions/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /actions/{id}/reject", s.handleReject)
	mux.HandleFunc("POST /actions/pause", s.handlePause)
	mux.HandleFunc("POST /actions/resume", s.handleResume)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	s.handler = mux
	return s
}

// SetCoalescer attaches a Coalescer after construction. Call before accepting
// requests; nil cz disables coalescing (direct enqueue).
func (s *Server) SetCoalescer(cz *coalesce.Coalescer) {
	s.coalescer = cz
}

// SetMetrics attaches the OTel metrics instance independently of coalescing, so
// ingress counters (alerts_received) emit even when coalescing is disabled.
func (s *Server) SetMetrics(m *telemetry.Metrics) {
	s.otelMetrics = m
}

// SetOutcomeLedger attaches the outcome ledger; resolved alerts are recorded into it.
func (s *Server) SetOutcomeLedger(l *outcome.Ledger) {
	s.outcomeLedger = l
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
// The URL is attacker-influenceable (it arrives in the interaction payload), so
// it is restricted to https *.slack.com and posted with a bounded client — no
// SSRF to arbitrary internal services, no unbounded hang on http.DefaultClient.
func (s *Server) updateSlack(ctx context.Context, responseURL, text string) {
	if responseURL == "" {
		return
	}
	if u, err := url.Parse(responseURL); err != nil || u.Scheme != "https" ||
		(u.Hostname() != "slack.com" && !strings.HasSuffix(u.Hostname(), ".slack.com")) {
		s.log.Warn("refusing slack response_url: not an https *.slack.com host", "url", responseURL)
		return
	}
	body, _ := json.Marshal(map[string]any{"replace_original": true, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	if resp, err := client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
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

func (s *Server) handleAlertmanager(w http.ResponseWriter, r *http.Request) {
	// Authenticate the webhook: its payload (labels, annotations) flows verbatim
	// into the investigator's LLM prompt, so an anonymous caller must not reach it.
	if !s.webhookAuthorized(r) {
		s.log.Warn("rejected alert webhook: missing/invalid bearer token")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Cap the body: its labels/annotations flow into the LLM prompt, so an
	// unbounded POST must not force unbounded allocation (the Slack handler caps
	// the same way). Past the cap, decode fails with *http.MaxBytesError → 413;
	// other decode errors are malformed JSON → 400.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	incidents, err := trigger.ParseAlertmanager(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	for _, inc := range incidents {
		if inc.Status == "resolved" {
			if s.outcomeLedger != nil {
				if ep, ok, err := s.outcomeLedger.Resolve(inc.Fingerprint, time.Now()); err != nil {
					s.log.Warn("outcome ledger resolve failed", "fingerprint", inc.Fingerprint, "err", err)
				} else if ok && s.otelMetrics != nil {
					s.otelMetrics.IncidentsResolved.Add(r.Context(), 1)
					s.otelMetrics.IncidentResolutionSeconds.Record(r.Context(), ep.Duration.Seconds())
					if ep.Kind == "recall" {
						s.otelMetrics.RecallOutcome.Add(r.Context(), 1, metric.WithAttributes(attribute.String("result", "resolved")))
					}
				}
			}
			continue
		}
		d := s.engine.Decide(inc)
		s.log.Info("incident",
			"alert", inc.AlertName, "severity", inc.Severity, "namespace", inc.Namespace,
			"investigate", d.Investigate, "reason", d.Reason)
		if !d.Investigate {
			continue
		}
		if s.otelMetrics != nil {
			s.otelMetrics.AlertsReceived.Add(r.Context(), 1)
		}
		if s.coalescer != nil {
			s.coalescer.Add(inc)
		} else {
			s.enqueuer.Enqueue(investigate.FromIncident(inc))
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
