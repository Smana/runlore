// Package config defines RunLore's configuration, including provider wiring and
// the trigger policy that decides which incidents start an investigation.
//
// The trigger policy is what keeps RunLore from firing on every alert: it filters
// incidents by environment, severity, namespace/team/label, and dedups still-firing
// alerts — controlling noise, relevance, and LLM cost.
package config

import (
	"fmt"
	"path"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level RunLore configuration (loaded from YAML).
type Config struct {
	GitOps   GitOps        `yaml:"gitops"` // engine selection (flux default | argocd)
	Triggers TriggerPolicy `yaml:"triggers"`
	Actions  ActionPolicy  `yaml:"actions"` // read-only by default; the upper rungs of the autonomy ladder
	Forge    Forge         `yaml:"forge"`   // git-forge auth (GitHub App) for diff access + curation
	Model    Model         `yaml:"model"`   // optional; when BaseURL is set, serve uses the LLM investigator
	Notify   Notify        `yaml:"notify"`  // chat delivery for findings
	Catalog  Catalog       `yaml:"catalog"` // OKF knowledge catalog

	LeaderElection LeaderElection `yaml:"leader_election"` // HA: only the leader investigates

	Metrics Endpoint `yaml:"metrics"` // PromQL backend (VictoriaMetrics/Prometheus) for query_metrics
	Logs    Endpoint `yaml:"logs"`    // LogsQL backend (VictoriaLogs) for query_logs
	Network Endpoint `yaml:"network"` // Hubble Relay gRPC address (host:port) for network_drops

	Server ServerConfig `yaml:"server"` // HTTP ingress (webhook authentication)
}

// Endpoint is a backend base URL; empty disables the corresponding tool.
type Endpoint struct {
	URL string `yaml:"url"`
}

// ServerConfig configures the HTTP ingress.
type ServerConfig struct {
	// WebhookTokenEnv names an env var holding a shared secret required on
	// POST /webhook/alertmanager as "Authorization: Bearer <token>". Empty leaves
	// the webhook unauthenticated (rejected by Validate when actions.mode=auto).
	WebhookTokenEnv string `yaml:"webhook_token_env"`
}

// LeaderElection configures high availability. When enabled, replicas elect a
// leader via a Lease; only the leader runs the informer watch + investigation
// queue and reports ready (so the Service routes webhooks to it). Run >1 replica
// for failover. Disabled by default (single-replica / local).
type LeaderElection struct {
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"` // Lease name (default "runlore-leader")
}

// Catalog configures the OKF knowledge catalog read by the agent. Provide either
// a mounted Dir (e.g. a ConfigMap) or a Git repo to sync (which closes the
// read/write loop — the curator's merged PRs flow back into what the agent reads).
type Catalog struct {
	Dir           string        `yaml:"dir"` // OKF bundle path (mounted ConfigMap, or the git-sync mirror)
	Git           CatalogGit    `yaml:"git"`
	InstantRecall InstantRecall `yaml:"instant_recall"`
}

// InstantRecall short-circuits the investigation loop when the catalog has a
// high-confidence match for the symptom. Off by default; MinScore is the BM25
// relevance floor (tune for your catalog).
type InstantRecall struct {
	Enabled  bool    `yaml:"enabled"`
	MinScore float64 `yaml:"min_score"`
}

// CatalogGit configures periodic Git sync of the catalog into Dir.
type CatalogGit struct {
	URL      string   `yaml:"url"`       // repo to clone/pull; empty disables git-sync
	Branch   string   `yaml:"branch"`    // default "main"
	Interval Duration `yaml:"interval"`  // re-sync period (default 5m)
	TokenEnv string   `yaml:"token_env"` // env var with a read token (empty = anonymous/public)
}

// Model configures the LLM used for investigation. Provider selects the wire
// protocol: "openai" (default, OpenAI-compatible: vLLM/Ollama/OpenAI) or
// "anthropic" (native Messages API). When unconfigured, serve uses the log-only
// investigator.
type Model struct {
	Provider  string `yaml:"provider"`    // "openai" (default) | "anthropic" | "gemini"
	BaseURL   string `yaml:"base_url"`    // OpenAI: required; Anthropic/Gemini: optional (built-in default endpoint)
	Model     string `yaml:"model"`       // model name
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
}

// Notify configures where investigation findings are delivered.
type Notify struct {
	Slack  SlackNotify  `yaml:"slack"`
	Matrix MatrixNotify `yaml:"matrix"`
}

// SlackNotify configures Slack delivery and (for rung-2 actions) interactive
// approve/reject buttons. Delivery uses either an incoming webhook (WebhookURLEnv)
// or a bot token posting to a channel via chat.postMessage (BotTokenEnv+Channel);
// if both are set, the bot token wins.
type SlackNotify struct {
	WebhookURLEnv    string   `yaml:"webhook_url_env"`    // env var holding the incoming-webhook URL
	BotTokenEnv      string   `yaml:"bot_token_env"`      // env var holding a bot token (xoxb-…) for chat.postMessage
	Channel          string   `yaml:"channel"`            // channel ID or name to post to (required with bot_token_env)
	SigningSecretEnv string   `yaml:"signing_secret_env"` // env var with the Slack signing secret (verifies button clicks)
	ApproverIDs      []string `yaml:"approver_ids"`       // Slack user IDs allowed to approve actions (empty = no Slack approvals)
}

// MatrixNotify configures Matrix delivery.
type MatrixNotify struct {
	Homeserver     string `yaml:"homeserver"`
	RoomID         string `yaml:"room_id"`
	AccessTokenEnv string `yaml:"access_token_env"` // env var holding the access token
}

// GitOps selects the GitOps engine RunLore reads (what-changed + failure watch).
type GitOps struct {
	Engine string `yaml:"engine"` // "flux" (default) | "argocd"
}

// TriggerPolicy decides what RunLore reacts to.
type TriggerPolicy struct {
	Incidents      IncidentTrigger `yaml:"incidents"`       // primary trigger
	GitOpsFailures Toggle          `yaml:"gitops_failures"` // secondary trigger
}

// IncidentTrigger gates incident/alert-driven investigations.
type IncidentTrigger struct {
	Enabled bool          `yaml:"enabled"`
	Match   IncidentMatch `yaml:"match"`  // must match to investigate
	Ignore  IncidentMatch `yaml:"ignore"` // excludes even if Match passes
	Dedup   Dedup         `yaml:"dedup"`
}

// IncidentMatch is a set of matchers ANDed together; empty fields match anything.
type IncidentMatch struct {
	Severity    []string          `yaml:"severity"`    // e.g. [critical]
	Environment []string          `yaml:"environment"` // e.g. [prod]
	Namespaces  []string          `yaml:"namespaces"`  // glob patterns
	AlertNames  []string          `yaml:"alertnames"`  // glob patterns
	Labels      map[string]string `yaml:"labels"`      // arbitrary label matchers
}

// Dedup suppresses re-investigation of a still-firing alert within Window.
type Dedup struct {
	Window Duration `yaml:"window"`
}

// Toggle is a simple on/off switch.
type Toggle struct {
	Enabled bool `yaml:"enabled"`
}

// Incident is the normalized trigger input (from Alertmanager/VMAlert).
type Incident struct {
	AlertName   string
	Severity    string
	Environment string
	Namespace   string
	Labels      map[string]string
	StartsAt    time.Time
	Fingerprint string // stable alert identity, used for dedup
}

// Matches reports whether an incident passes this trigger policy: enabled,
// matched by Match, and not excluded by a non-empty Ignore.
func (t IncidentTrigger) Matches(inc Incident) bool {
	if !t.Enabled {
		return false
	}
	if !t.Match.matches(inc) {
		return false
	}
	if !t.Ignore.isEmpty() && t.Ignore.matches(inc) {
		return false
	}
	return true
}

// matches reports whether the incident satisfies every non-empty criterion.
func (m IncidentMatch) matches(inc Incident) bool {
	if len(m.Severity) > 0 && !slices.Contains(m.Severity, inc.Severity) {
		return false
	}
	if len(m.Environment) > 0 && !slices.Contains(m.Environment, inc.Environment) {
		return false
	}
	if len(m.Namespaces) > 0 && !globAny(m.Namespaces, inc.Namespace) {
		return false
	}
	if len(m.AlertNames) > 0 && !globAny(m.AlertNames, inc.AlertName) {
		return false
	}
	for k, v := range m.Labels {
		if inc.Labels[k] != v {
			return false
		}
	}
	return true
}

// isEmpty reports whether no criteria are set.
func (m IncidentMatch) isEmpty() bool {
	return len(m.Severity) == 0 && len(m.Environment) == 0 &&
		len(m.Namespaces) == 0 && len(m.AlertNames) == 0 && len(m.Labels) == 0
}

func globAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
	}
	return false
}

// Duration is a time.Duration that unmarshals from a Go duration string ("30m").
type Duration time.Duration

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a duration string such as "30m" or "1h30m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// ActionMode controls how far RunLore may go when acting on the cluster.
// Default (zero value) is read-only.
type ActionMode string

// Action modes, from read-only to autonomous.
const (
	ActionOff     ActionMode = "off"     // read-only (default): no action tools registered
	ActionSuggest ActionMode = "suggest" // propose a command/PR; never execute
	ActionApprove ActionMode = "approve" // execute only after explicit human approval
	ActionAuto    ActionMode = "auto"    // execute within the allowed envelope, no click
)

// ActionPolicy gates cluster-mutating actions — the upper rungs of the autonomy
// ladder. v1 ships ActionOff; the type exists so active tools can be added later
// behind a gate without re-architecting (see docs/design.md §9, "Act").
type ActionPolicy struct {
	Mode             ActionMode  `yaml:"mode"`               // off | suggest | approve | auto
	Allow            ActionAllow `yaml:"allow"`              // envelope, enforced even in approve/auto
	RequireApproval  bool        `yaml:"require_approval"`   // force a human click for gated actions
	ApprovalTokenEnv string      `yaml:"approval_token_env"` // env var with a shared secret for the approval endpoints
	AuditLogPath     string      `yaml:"audit_log_path"`     // append-only, hash-chained action audit log (required for auto)
	Auto             AutoPolicy  `yaml:"auto"`               // rung-3 unattended-execution safety controls
}

// AutoPolicy bounds unattended execution (mode "auto"). Even within these, auto
// only ever runs REVERSIBLE actions, and every decision is audited + delivered.
type AutoPolicy struct {
	DryRun        bool     `yaml:"dry_run"`        // log "would execute" without executing
	MinConfidence float64  `yaml:"min_confidence"` // only auto-execute when the investigation is at least this confident
	MaxPerWindow  int      `yaml:"max_per_window"` // rate limit; 0 = unlimited (not recommended)
	Window        Duration `yaml:"window"`         // rate-limit window (default 1h)
}

// ActionAllow bounds what may be acted on, even in approve/auto modes.
// Irreversibility is the trip-wire for mandatory human approval.
type ActionAllow struct {
	ReversibleOnly      bool     `yaml:"reversible_only"`      // never auto-apply irreversible actions
	Namespaces          []string `yaml:"namespaces"`           // allowlist of target namespaces; empty = no executable target permitted
	ProtectedNamespaces []string `yaml:"protected_namespaces"` // never an action target (added to the built-ins flux-system, kube-system)
	MaxBlastRadius      int      `yaml:"max_blast_radius"`     // cap on affected workloads
	Kinds               []string `yaml:"kinds"`                // resource kinds that may be acted on
}

// Enabled reports whether any cluster-mutating action is permitted.
func (a ActionPolicy) Enabled() bool {
	return a.Mode != "" && a.Mode != ActionOff
}

// Validate enforces cross-field invariants after loading — fail-closed defaults
// for the autonomy ladder: enabling execution requires the controls that bound
// it. Returns an error that should abort startup.
func (c *Config) Validate() error {
	switch c.Actions.Mode {
	case "", ActionOff, ActionSuggest:
		return nil // read-only-ish: nothing to execute
	case ActionApprove, ActionAuto:
		// Both executing rungs require the control/kill-switch token (fail closed).
		if c.Actions.ApprovalTokenEnv == "" {
			return fmt.Errorf("actions.mode=%s requires actions.approval_token_env (control/kill-switch endpoints fail closed without it)", c.Actions.Mode)
		}
		if c.Actions.Mode == ActionApprove {
			return nil
		}
		// auto-only: unattended execution additionally needs audit, an authenticated
		// webhook, and bounded gates.
		if c.Actions.AuditLogPath == "" {
			return fmt.Errorf("actions.mode=auto requires actions.audit_log_path (auto-execution must be audited)")
		}
		if c.Server.WebhookTokenEnv == "" {
			return fmt.Errorf("actions.mode=auto requires server.webhook_token_env (the alert webhook must be authenticated)")
		}
		if c.Actions.Auto.MinConfidence <= 0 {
			return fmt.Errorf("actions.mode=auto requires actions.auto.min_confidence > 0")
		}
		if c.Actions.Auto.MaxPerWindow <= 0 {
			return fmt.Errorf("actions.mode=auto requires actions.auto.max_per_window > 0 (unbounded auto-execution is unsafe)")
		}
		if len(c.Actions.Allow.Namespaces) == 0 {
			return fmt.Errorf("actions.mode=auto requires actions.allow.namespaces (target-namespace allowlist)")
		}
		return nil
	default:
		return fmt.Errorf("unknown actions.mode %q (want off|suggest|approve|auto)", c.Actions.Mode)
	}
}

// Forge holds git-forge authentication and the curation target repo.
type Forge struct {
	GitHubApp    GitHubApp `yaml:"github_app"`
	KBRepo       string    `yaml:"kb_repo"`        // "owner/name" — the catalog repo for curation
	BaseBranch   string    `yaml:"base_branch"`    // PR target branch (default "main")
	GitHubAPIURL string    `yaml:"github_api_url"` // override for GHES/tests (default https://api.github.com)
}

// GitHubApp holds GitHub App credentials. The private key mints 1-hour
// installation tokens (no long-lived PAT); it is referenced from a Secret, never
// inlined. Required permissions: contents:read (diff), and on the KB repo
// issues:write + pull_requests:write + contents:write (curation).
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyRef  string `yaml:"private_key_ref"` // Secret name/key (e.g. via External Secrets)
	PrivateKeyEnv  string `yaml:"private_key_env"` // v1: env var holding the PEM private key
}
