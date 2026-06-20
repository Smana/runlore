// Package config defines RunLore's configuration, including provider wiring and
// the trigger policy that decides which incidents start an investigation.
//
// The trigger policy is what keeps RunLore from firing on every alert: it filters
// incidents by environment, severity, namespace/team/label, and dedups still-firing
// alerts — controlling noise, relevance, and LLM cost.
package config

import (
	"path"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level RunLore configuration (loaded from YAML).
type Config struct {
	Triggers TriggerPolicy `yaml:"triggers"`
	Actions  ActionPolicy  `yaml:"actions"` // read-only by default; the upper rungs of the autonomy ladder
	Forge    Forge         `yaml:"forge"`   // git-forge auth (GitHub App) for diff access + curation
	Model    Model         `yaml:"model"`   // optional; when BaseURL is set, serve uses the LLM investigator
	Notify   Notify        `yaml:"notify"`  // chat delivery for findings
	Catalog  Catalog       `yaml:"catalog"` // OKF knowledge catalog
}

// Catalog configures the OKF knowledge catalog read by the agent.
type Catalog struct {
	Dir string `yaml:"dir"` // path to the OKF bundle (a local dir / mounted mirror)
}

// Model configures the OpenAI-compatible LLM endpoint used for investigation.
// When BaseURL is empty, serve falls back to the log-only investigator.
type Model struct {
	BaseURL   string `yaml:"base_url"`    // e.g. https://vllm.svc/v1
	Model     string `yaml:"model"`       // model name
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
}

// Notify configures where investigation findings are delivered.
type Notify struct {
	Slack  SlackNotify  `yaml:"slack"`
	Matrix MatrixNotify `yaml:"matrix"`
}

// SlackNotify configures Slack incoming-webhook delivery.
type SlackNotify struct {
	WebhookURLEnv string `yaml:"webhook_url_env"` // env var holding the webhook URL
}

// MatrixNotify configures Matrix delivery.
type MatrixNotify struct {
	Homeserver     string `yaml:"homeserver"`
	RoomID         string `yaml:"room_id"`
	AccessTokenEnv string `yaml:"access_token_env"` // env var holding the access token
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
	Mode            ActionMode  `yaml:"mode"`             // off | suggest | approve | auto
	Allow           ActionAllow `yaml:"allow"`            // envelope, enforced even in approve/auto
	RequireApproval bool        `yaml:"require_approval"` // force a human click for gated actions
}

// ActionAllow bounds what may be acted on, even in approve/auto modes.
// Irreversibility is the trip-wire for mandatory human approval.
type ActionAllow struct {
	ReversibleOnly bool     `yaml:"reversible_only"`  // never auto-apply irreversible actions
	Environments   []string `yaml:"environments"`     // e.g. [staging] — never prod by default
	MaxBlastRadius int      `yaml:"max_blast_radius"` // cap on affected workloads
	Kinds          []string `yaml:"kinds"`            // resource kinds that may be acted on
}

// Enabled reports whether any cluster-mutating action is permitted.
func (a ActionPolicy) Enabled() bool {
	return a.Mode != "" && a.Mode != ActionOff
}

// Forge holds git-forge authentication. A GitHub App is the v1 default: one
// fine-grained, short-lived identity used for both git access (clone/diff for the
// what-changed spine) and forge operations (issues/PRs for the Curator).
type Forge struct {
	GitHubApp GitHubApp `yaml:"github_app"`
}

// GitHubApp holds GitHub App credentials. The private key mints 1-hour
// installation tokens (no long-lived PAT); it is referenced from a Secret, never
// inlined. Required permissions: contents:read (diff), and on the KB repo
// issues:write + pull_requests:write + contents:write (curation).
type GitHubApp struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyRef  string `yaml:"private_key_ref"` // Secret name/key (e.g. via External Secrets)
}
