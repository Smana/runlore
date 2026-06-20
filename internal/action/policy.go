// Package action gates proposed remediations against the autonomy-ladder policy
// (config.actions: off | suggest | approve | auto). The gate is server-authoritative:
// reversibility and blast radius are derived from the operation (deriveSafety), not
// trusted from model-authored fields, and executable targets are checked against a
// namespace allowlist with a built-in protected-namespace deny. suggest surfaces
// proposals only; approve executes after an authenticated human approval; auto
// executes reversible, in-envelope actions unattended (rate-limited, kill-switchable,
// audited). The gate is the load-bearing safety logic for every rung above read-only.
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

// IsAuto reports whether unattended (rung-3) execution is configured.
func (p *Policy) IsAuto() bool { return p.cfg.Mode == config.ActionAuto }

// Review filters proposed actions to those within the envelope (the ones safe to
// surface as suggestions), returning the kept actions plus a reason for each
// withheld one. When actions are disabled (mode off), nothing is surfaced.
func (p *Policy) Review(actions []providers.Action) (kept []providers.Action, withheld []string) {
	if !p.Enabled() {
		return nil, nil
	}
	for _, a := range actions {
		a = deriveSafety(a) // server-authoritative reversibility/blast; discard model-supplied values
		if reason := p.violation(a); reason != "" {
			withheld = append(withheld, fmt.Sprintf("%s (%s)", actionLabel(a), reason))
			continue
		}
		kept = append(kept, a)
	}
	return kept, withheld
}

// violation returns why an action is outside the envelope, or "" if compliant.
// Callers pass a deriveSafety-sanitized action so reversibility/blast are
// server-derived, not model-claimed.
func (p *Policy) violation(a providers.Action) string {
	allow := p.cfg.Allow
	executable := a.Op != ""
	if executable && !knownOp(a.Op) {
		return "unknown op " + a.Op
	}
	if allow.ReversibleOnly && !a.Reversible {
		return "irreversible; reversible_only is set"
	}
	if allow.MaxBlastRadius > 0 && a.BlastRadius > allow.MaxBlastRadius {
		return fmt.Sprintf("blast radius %d exceeds max %d", a.BlastRadius, allow.MaxBlastRadius)
	}
	if len(allow.Kinds) > 0 && a.Target.Kind != "" && !slices.Contains(allow.Kinds, a.Target.Kind) {
		return "kind " + a.Target.Kind + " not in allowed kinds"
	}
	if !executable {
		return "" // suggestions are advisory; no execution-target checks beyond the envelope above
	}
	// Executable actions must name a concrete allowed kind in a permitted namespace.
	if a.Target.Kind == "" {
		return "executable action needs a target kind"
	}
	return p.namespaceViolation(a.Target.Namespace)
}

// namespaceViolation enforces the target-namespace allow/deny lists. A protected
// namespace is always denied; otherwise the namespace must appear in the
// operator's allowlist (an empty allowlist permits no executable target).
//
// TODO: when a second target dimension is needed (environment, labels), extract a
// composable TargetPolicy.Evaluate(target) instead of adding inline branches here.
func (p *Policy) namespaceViolation(ns string) string {
	if ns == "" {
		return "target namespace required"
	}
	if slices.Contains(builtinProtectedNamespaces, ns) || slices.Contains(p.cfg.Allow.ProtectedNamespaces, ns) {
		return "namespace " + ns + " is protected (never an action target)"
	}
	if !slices.Contains(p.cfg.Allow.Namespaces, ns) {
		return "namespace " + ns + " not in the action allowlist"
	}
	return ""
}

func actionLabel(a providers.Action) string {
	if a.Description != "" {
		return a.Description
	}
	return a.Name
}
