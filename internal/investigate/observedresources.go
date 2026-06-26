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
// It backs F2 — an executable action may only target a resource the investigation
// observed, so a prompt-injected action naming an arbitrary resource cannot reach the
// approve/auto path. Only server-sourced identities go in here; validating against
// the model-supplied finding would be false security.
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
// seeded with the given workloads (typically the originating alert/failure workload).
// Read tools that confirm resources server-side call recordObserved; reviewActions
// consults it via downgradeUnobservedTargets.
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

// downgradeUnobservedTargets (F2) strips the executable Op from any proposed action
// whose target the investigation never observed server-side — turning a possible
// prompt-injected target into a non-executable suggestion that can't reach the
// approve/auto path. The server-authoritative action gate still re-validates op,
// kind, and namespace; this closes the "which named resource" residual ahead of it.
// A no-op when ctx carries no collector (behaviour unchanged).
func downgradeUnobservedTargets(ctx context.Context, actions []providers.Action, log *slog.Logger) []providers.Action {
	o := observedFrom(ctx)
	if o == nil {
		return actions
	}
	for i := range actions {
		a := &actions[i]
		if a.Op == "" || a.Target.Name == "" {
			continue // already a suggestion, or no named target to validate
		}
		if !o.matches(a.Target) {
			if log != nil {
				log.Warn("action target not observed during investigation; downgrading to a non-executable suggestion (possible prompt injection)",
					"op", a.Op, "target", a.Target.Namespace+"/"+a.Target.Name)
			}
			a.Op = ""
			a.Mutating = false
		}
	}
	return actions
}
