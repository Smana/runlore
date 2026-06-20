// Package server exposes RunLore's HTTP endpoints (incident webhooks).
package server

import (
	"log/slog"
	"net/http"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/trigger"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine   *trigger.Engine
	enqueuer investigate.Enqueuer
	ready    func() bool
	log      *slog.Logger
	handler  http.Handler
}

// New builds a Server from config and an investigation enqueuer. ready reports
// whether this replica should serve traffic (e.g. it holds leadership); when nil,
// the replica is always ready. /healthz is liveness (always OK); /readyz is
// readiness (gated by ready) so the Service routes webhooks only to the leader.
func New(cfg *config.Config, enq investigate.Enqueuer, ready func() bool, log *slog.Logger) *Server {
	s := &Server{engine: trigger.NewEngine(cfg.Triggers.Incidents), enqueuer: enq, ready: ready, log: log}
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
	s.handler = mux
	return s
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
