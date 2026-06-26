// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Smana/runlore/internal/providers"
)

// observedResources is the set of resources an investigation confirmed SERVER-SIDE:
// the originating workload, the GitOps changes what_changed detected, and the
// resources the gitops_resource_status / gitops_tree inspector tools actually read.
// It backs F2 — an action targeting a resource the investigation never observed is
// treated as possibly hallucinated/prompt-injected: never auto-executed, and flagged
// for the human approver (see guardUnobservedTargets). Only server-sourced identities
// go in here; validating against the model-supplied finding would be false security.
//
// Context-scoped (one per investigation) and concurrency-safe.
type observedResources struct {
	mu    sync.Mutex
	byKey map[wkey]struct{}   // exact (namespace, name)
	names map[string]struct{} // name-only (namespace-tolerant fallback)
}

type wkey struct{ ns, name string }

type observedCtxKey struct{}

// WithObservedResources derives a context carrying an observed-resource collector
// seeded with the given workloads (typically the originating alert/failure workload
// — the trigger's subject is by definition a legitimate thing to act on, so it
// ALWAYS counts as observed even when no tool re-reads it). Read tools that confirm
// resources server-side call recordObserved; reviewActions consults the set via
// guardUnobservedTargets.
func WithObservedResources(ctx context.Context, seed ...providers.Workload) context.Context {
	o := &observedResources{byKey: map[wkey]struct{}{}, names: map[string]struct{}{}}
	for _, w := range seed {
		o.add(w)
	}
	return context.WithValue(ctx, observedCtxKey{}, o)
}

func observedFrom(ctx context.Context) *observedResources {
	o, _ := ctx.Value(observedCtxKey{}).(*observedResources)
	return o
}

func (o *observedResources) add(w providers.Workload) {
	if w.Name == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.byKey[wkey{w.Namespace, w.Name}] = struct{}{}
	o.names[w.Name] = struct{}{}
}

// matches reports whether target names a resource the investigation observed. It
// prefers an exact (namespace, name) hit, then falls back to name-only: a GitOps
// object's namespace varies by view (the object's own namespace vs the workload's),
// and the action gate's namespace allowlist is the authoritative namespace check —
// so observing a resource of this NAME server-side is sufficient corroboration.
func (o *observedResources) matches(t providers.Workload) bool {
	if t.Name == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if t.Namespace != "" {
		if _, ok := o.byKey[wkey{t.Namespace, t.Name}]; ok {
			return true
		}
	}
	_, ok := o.names[t.Name]
	return ok
}

// recordObserved adds server-confirmed resources to the investigation's observed set.
// A no-op when ctx carries no collector (e.g. unit tests / the recall path).
func recordObserved(ctx context.Context, ws ...providers.Workload) {
	if o := observedFrom(ctx); o != nil {
		for _, w := range ws {
			o.add(w)
		}
	}
}

// unobservedTargetWarning is prepended to an unobserved-target action's description
// under a human-gated mode, so the flag travels with the action into the approval
// queue, the Slack/Matrix message, and the audit trail.
const unobservedTargetWarning = "[WARNING: target was never observed server-side during this investigation — possible hallucinated or injected resource; verify it exists and is the right one before approving]"

// guardUnobservedTargets (F2) handles proposed actions whose target the
// investigation never corroborated server-side (not the trigger's subject, and not
// surfaced by what_changed / gitops_resource_status / gitops_tree). The failure
// mode is deliberately per-rung:
//
//   - auto (no human in the loop): the executable Op is STRIPPED — the action
//     degrades to a suggestion that is still delivered but can never execute
//     unattended. Fail safe: a hallucinated/injected target must not reach the
//     cluster on the strength of model output alone.
//   - approve/suggest (a human gates every execution): the action stays executable
//     but its description gains an explicit unobserved-target warning. Downgrading
//     here would either starve the approval queue of legitimate remediations or —
//     worse — queue non-executable entries a human can "approve" into an error
//     (both regressions the earlier strict-downgrade attempts actually caused; see
//     the PR). The observed set is a heuristic with false negatives — an incomplete
//     investigation may propose a perfectly correct target it never re-read — so
//     under a human gate its verdict is advisory: it arms the approver, it does not
//     override them.
//
// The server-authoritative action gate still validates op, kind, and namespace at
// review AND at the exec boundary; this guard closes the "which named resource"
// residual ahead of it. Suggestions and target-less actions pass through untouched,
// and the whole guard is a no-op when ctx carries no collector.
func guardUnobservedTargets(ctx context.Context, actions []providers.Action, autoMode bool, log *slog.Logger) []providers.Action {
	o := observedFrom(ctx)
	if o == nil {
		return actions
	}
	for i := range actions {
		a := &actions[i]
		if a.Op == "" || a.Target.Name == "" {
			continue // already a suggestion, or no named target to validate
		}
		if o.matches(a.Target) {
			continue
		}
		if autoMode {
			if log != nil {
				log.Warn("action target not observed during investigation; downgrading to a non-executable suggestion (possible prompt injection)",
					"op", a.Op, "target", a.Target.Namespace+"/"+a.Target.Name)
			}
			a.Op = ""
			a.Mutating = false
			continue
		}
		if log != nil {
			log.Warn("action target not observed during investigation; flagging for the human approver (possible prompt injection)",
				"op", a.Op, "target", a.Target.Namespace+"/"+a.Target.Name)
		}
		a.Description = unobservedTargetWarning + " " + a.Description
	}
	return actions
}
