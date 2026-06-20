// Package action gates proposed remediations against the autonomy-ladder policy
// (config.actions). v1 implements rung 1 ("suggest"): it filters proposals to the
// allowed envelope and surfaces them — it never executes anything (RunLore has no
// cluster-mutating tools). The gate is the load-bearing logic for higher rungs.
package action

import (
	"fmt"
	"slices"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// Policy evaluates proposed actions against the configured envelope.
type Policy struct {
	cfg config.ActionPolicy
}

// New builds a Policy from config.
func New(cfg config.ActionPolicy) *Policy { return &Policy{cfg: cfg} }

// Enabled reports whether any non-off action mode is configured.
func (p *Policy) Enabled() bool { return p.cfg.Enabled() }

// Mode returns the configured action mode.
func (p *Policy) Mode() config.ActionMode { return p.cfg.Mode }

// Review filters proposed actions to those within the envelope (the ones safe to
// surface as suggestions), returning the kept actions plus a reason for each
// withheld one. When actions are disabled (mode off), nothing is surfaced.
func (p *Policy) Review(actions []providers.Action) (kept []providers.Action, withheld []string) {
	if !p.Enabled() {
		return nil, nil
	}
	for _, a := range actions {
		if reason := p.violation(a); reason != "" {
			withheld = append(withheld, fmt.Sprintf("%s (%s)", actionLabel(a), reason))
			continue
		}
		kept = append(kept, a)
	}
	return kept, withheld
}

// violation returns why an action is outside the envelope, or "" if compliant.
func (p *Policy) violation(a providers.Action) string {
	allow := p.cfg.Allow
	if allow.ReversibleOnly && !a.Reversible {
		return "irreversible; reversible_only is set"
	}
	if allow.MaxBlastRadius > 0 && a.BlastRadius > allow.MaxBlastRadius {
		return fmt.Sprintf("blast radius %d exceeds max %d", a.BlastRadius, allow.MaxBlastRadius)
	}
	if len(allow.Kinds) > 0 && a.Target.Kind != "" && !slices.Contains(allow.Kinds, a.Target.Kind) {
		return "kind " + a.Target.Kind + " not in allowed kinds"
	}
	return ""
}

func actionLabel(a providers.Action) string {
	if a.Description != "" {
		return a.Description
	}
	return a.Name
}
