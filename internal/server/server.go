// Package server exposes RunLore's HTTP endpoints (incident webhooks).
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine    *trigger.Engine
	enqueuer  investigate.Enqueuer
	ready     func() bool
	approvals *action.Approvals // nil unless action mode "approve" is configured
	token     string            // optional shared secret for the approval endpoints
	log       *slog.Logger
	handler   http.Handler
}

// New builds a Server. ready reports whether this replica should serve (leadership);
// nil = always ready. approvals (optional) enables the human-approval endpoints for
// rung-2 actions; token (optional) is required as X-Approval-Token to use them.
// /healthz is liveness; /readyz is readiness (gated by ready, leader-only).
func New(cfg *config.Config, enq investigate.Enqueuer, ready func() bool, approvals *action.Approvals, token string, log *slog.Logger) *Server {
	s := &Server{engine: trigger.NewEngine(cfg.Triggers.Incidents), enqueuer: enq, ready: ready, approvals: approvals, token: token, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.handleAlertmanager)
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
