package action

import (
	"github.com/Smana/runlore/internal/providers"
)

// opSafety is the SERVER-AUTHORITATIVE safety metadata for each executable op.
// Reversibility and blast radius are derived here, from the operation itself —
// never trusted from the model-authored fields in submit_findings, which an
// adversarial or prompt-injected LLM can forge to slip past the gates. Keep in
// sync with the executor (internal/executor/flux): only ops listed here execute.
var opSafety = map[string]struct {
	reversible bool
	blast      int
}{
	"suspend":   {reversible: true, blast: 1},
	"resume":    {reversible: true, blast: 1},
	"reconcile": {reversible: true, blast: 1},
}

// builtinProtectedNamespaces are never valid action targets, regardless of
// config: mutating them (e.g. suspending flux-system) is a platform-wide
// availability/integrity attack. Operators add their own (security, etc.) via
// ActionAllow.ProtectedNamespaces.
var builtinProtectedNamespaces = []string{"flux-system", "kube-system"}

// deriveSafety returns a copy of a with Reversible and BlastRadius set from the
// server-side op table, discarding any model-supplied values for EXECUTABLE ops.
// A suggestion (empty op) is advisory only — never executed — so its display
// flags are left untouched. An unknown executable op is marked not-reversible.
func deriveSafety(a providers.Action) providers.Action {
	if a.Op == "" {
		return a // advisory suggestion; never executed
	}
	if meta, ok := opSafety[a.Op]; ok {
		a.Reversible = meta.reversible
		a.BlastRadius = meta.blast
		return a
	}
	a.Reversible = false // unknown executable op: treat as unsafe
	return a
}

// knownOp reports whether op is a server-recognized executable operation.
func knownOp(op string) bool {
	_, ok := opSafety[op]
	return ok
}
