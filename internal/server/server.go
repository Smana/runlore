// Package server exposes RunLore's HTTP endpoints (incident webhooks).
package server

import (
	"log/slog"
	"net/http"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/trigger"
)

// Server handles incoming incident webhooks and applies the trigger policy.
type Server struct {
	engine *trigger.Engine
	log    *slog.Logger
}

// New builds a Server from config.
func New(cfg *config.Config, log *slog.Logger) *Server {
	return &Server{engine: trigger.NewEngine(cfg.Triggers.Incidents), log: log}
}

// Handler returns the HTTP mux (Go 1.22+ method routing).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/alertmanager", s.handleAlertmanager)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
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
		// Phase 1+ (later plan): if d.Investigate, enqueue an investigation here.
	}
	w.WriteHeader(http.StatusAccepted)
}
