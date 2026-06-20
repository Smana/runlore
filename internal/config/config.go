// Package config defines RunLore's configuration, including provider wiring and
// the trigger policy that decides which incidents start an investigation.
//
// The trigger policy is what keeps RunLore from firing on every alert: it filters
// incidents by environment, severity, namespace/team/label, and dedups still-firing
// alerts — controlling noise, relevance, and LLM cost.
package config

import "time"

// Config is the top-level RunLore configuration (loaded from YAML).
type Config struct {
	Triggers TriggerPolicy `yaml:"triggers"`
	// Providers, Catalog, Model, Notify, etc. are added as packages land.
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
	Window time.Duration `yaml:"window"`
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
}

// Matches reports whether an incident passes this trigger policy: matched by
// Match and not excluded by Ignore. Matcher evaluation is implemented in Phase 1.
func (t IncidentTrigger) Matches(_ Incident) bool {
	// TODO(phase1): evaluate Match/Ignore (severity, environment, namespace globs,
	// alertname globs, label matchers) instead of only the enabled flag.
	return t.Enabled
}
