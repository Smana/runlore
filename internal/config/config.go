// SPDX-License-Identifier: Apache-2.0

// Package config defines RunLore's configuration, including provider wiring and
// the trigger policy that decides which incidents start an investigation.
//
// The trigger policy is what keeps RunLore from firing on every alert: it filters
// incidents by environment, severity, namespace/team/label, and dedups still-firing
// alerts — controlling noise, relevance, and LLM cost.
package config

import (
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level RunLore configuration (loaded from YAML).
type Config struct {
	GitOps   GitOps        `yaml:"gitops"` // engine selection (flux default | argocd)
	Triggers TriggerPolicy `yaml:"triggers"`

	// Sources is the per-source enablement map: a key under `sources.<name>`
	// enables that source adapter, and its value is the adapter's own raw config
	// (decoded lazily by each adapter's Build). Presence is enablement — e.g.
	// `sources.alertmanager: {}` turns on the Alertmanager webhook source. This
	// keeps adding a source from requiring a central-struct edit. The webhook
	// auth token stays server-level (server.webhook_token_env).
	Sources map[string]yaml.Node `yaml:"sources"`

	Actions       ActionPolicy  `yaml:"actions"`        // read-only by default; the upper rungs of the autonomy ladder
	Forge         Forge         `yaml:"forge"`          // git-forge auth (GitHub App) for diff access + curation
	Curate        Curate        `yaml:"curate"`         // Phase-2 backlog groomer settings
	Model         Model         `yaml:"model"`          // optional; when BaseURL is set, serve uses the LLM investigator
	DecisionModel DecisionModel `yaml:"decision_model"` // System One decision model (jev)
	Notify        Notify        `yaml:"notify"`         // chat delivery for findings
	Catalog       Catalog       `yaml:"catalog"`        // OKF knowledge catalog
	Outcome       Outcome       `yaml:"outcome"`        // learning-loop outcome ledger
	Factory       Factory       `yaml:"factory"`        // agent factory client and webhook receiver; empty url disables it

	LeaderElection LeaderElection `yaml:"leader_election"` // HA: only the leader investigates

	Metrics     MetricsConfig `yaml:"metrics"`      // PromQL backend (VictoriaMetrics/Prometheus) for query_metrics
	Logs        LogsConfig    `yaml:"logs"`         // LogsQL backend (VictoriaLogs) for query_logs
	Network     Network       `yaml:"network"`      // network-flow data source (pluggable, CNI-agnostic); empty Provider disables it
	Cloud       Cloud         `yaml:"cloud"`        // cloud-side context (AWS); empty Provider disables it
	MCP         MCP           `yaml:"mcp"`          // external MCP servers whose tools the agent may call (opt-in)
	SourceRepos SourceRepos   `yaml:"source_repos"` // source_diff repo allowlist; empty (default) disables the tool

	Server ServerConfig `yaml:"server"` // HTTP ingress (webhook authentication)

	Investigation Investigation `yaml:"investigation"` // coalescing + rate-limit + per-investigation token controls
	Telemetry     Telemetry     `yaml:"telemetry"`     // OpenTelemetry metrics
	Logging       Logging       `yaml:"logging"`       // structured-logging format + verbosity
}

// Logging configures the structured logger. Format selects human-readable text
// (default, for local CLI) or JSON (for in-cluster log aggregation). Level sets
// verbosity. Both are overridable at startup via RUNLORE_LOG_FORMAT /
// RUNLORE_LOG_LEVEL (see internal/logging).
type Logging struct {
	Format string `yaml:"format"` // "text" (default) | "json"
	Level  string `yaml:"level"`  // "debug" | "info" (default) | "warn" | "error"
}

// ServerConfig configures the HTTP ingress.
type ServerConfig struct {
	// WebhookTokenEnv names an env var holding a shared secret required on
	// POST /webhook/alertmanager as "Authorization: Bearer <token>". Empty leaves
	// the webhook unauthenticated (rejected by Validate when actions.mode=auto).
	WebhookTokenEnv string `yaml:"webhook_token_env"`
	// MCP serves kb_search and kb_get over StreamableHTTP at POST /mcp on the serve port.
	MCP ServerMCP `yaml:"mcp"`
	// KBUI serves a read-only embedded HTML explorer for the OKF catalog and outcome
	// ledger at /kb on the serve port.
	KBUI ServerKBUI `yaml:"kb_ui"`
}

// ServerMCP configures the read-only /mcp endpoint. Off by default.
type ServerMCP struct {
	Enabled bool `yaml:"enabled"`
	// TokenEnv names an env var holding a bearer token required on /mcp. Empty leaves it
	// open to any caller the serve port admits; set but empty, every request gets 401.
	TokenEnv string `yaml:"token_env"`
}

// ServerKBUI configures the read-only /kb HTML explorer endpoint. Off by default.
type ServerKBUI struct {
	Enabled bool `yaml:"enabled"`
	// TokenEnv names an env var holding a bearer token required on /kb. Empty leaves it
	// open to any caller the serve port admits; set but empty, every request gets 401.
	TokenEnv string `yaml:"token_env"`
}

// Telemetry configures OpenTelemetry metrics export.
type Telemetry struct {
	MetricsEnabled bool   `yaml:"metrics_enabled"` // serve OTel metrics on GET /metrics (Prometheus exposition)
	OTLPEndpoint   string `yaml:"otlp_endpoint"`   // optional OTLP push (phase-2); empty ⇒ scrape-only
}

// LeaderElection configures high availability. When enabled, replicas elect a
// leader via a Lease; only the leader runs the informer watch + investigation
// queue and reports ready (so the Service routes webhooks to it). Run >1 replica
// for failover. Disabled by default (single-replica / local).
type LeaderElection struct {
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"` // Lease name (default "runlore-leader")
}

// Duration is a time.Duration that unmarshals from a Go duration string ("30m"),
// with a leading whole-day unit: "7d", "7d12h".
type Duration time.Duration

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string such as "30m" or "1h30m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := parseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// parseDuration is time.ParseDuration plus a leading "<n>d" of whole days. Anything
// else with a "d" (1.5d, a bare d, -1d) stays an error from time.ParseDuration.
func parseDuration(s string) (time.Duration, error) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(s) || s[i] != 'd' {
		return time.ParseDuration(s)
	}
	days, err := strconv.Atoi(s[:i])
	if err != nil || days > 36500 {
		return time.ParseDuration(s)
	}
	var rest time.Duration
	if tail := s[i+1:]; tail != "" {
		if rest, err = time.ParseDuration(tail); err != nil {
			return 0, err
		}
		if tail[0] < '0' || tail[0] > '9' {
			return time.ParseDuration(s)
		}
	}
	return time.Duration(days)*24*time.Hour + rest, nil
}
