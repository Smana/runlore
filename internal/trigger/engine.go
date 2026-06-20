package trigger

import "github.com/Smana/runlore/internal/config"

// Decision is the outcome of evaluating an incident against the trigger policy.
type Decision struct {
	Investigate bool
	Reason      string
}

// Engine evaluates incidents against a policy, with dedup.
type Engine struct {
	policy config.IncidentTrigger
	dedup  *Deduper
}

// NewEngine builds an Engine from the incident trigger policy.
func NewEngine(p config.IncidentTrigger) *Engine {
	return &Engine{policy: p, dedup: NewDeduper(p.Dedup.Window.Std())}
}

// Decide returns whether the incident should start an investigation.
func (e *Engine) Decide(inc config.Incident) Decision {
	if !e.policy.Matches(inc) {
		return Decision{false, "filtered by trigger policy"}
	}
	if e.dedup.Seen(dedupKey(inc)) {
		return Decision{false, "deduplicated (still-firing)"}
	}
	return Decision{true, "matched trigger policy"}
}

func dedupKey(inc config.Incident) string {
	if inc.Fingerprint != "" {
		return inc.Fingerprint
	}
	return inc.AlertName + "/" + inc.Namespace
}
