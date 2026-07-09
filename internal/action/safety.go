// SPDX-License-Identifier: Apache-2.0

package action

import (
	"github.com/Smana/runlore/internal/providers"
)

// builtinProtectedNamespaces are never valid action targets, regardless of
// config: mutating them (e.g. suspending flux-system) is a platform-wide
// availability/integrity attack. Operators add their own (security, etc.) via
// ActionAllow.ProtectedNamespaces.
var builtinProtectedNamespaces = []string{"flux-system", "kube-system"}

// deriveSafety returns a copy of a with Reversible and BlastRadius set from the
// canonical op registry (providers.Ops) — the server-authoritative source, never
// model output — for EXECUTABLE ops. A suggestion (empty op) is advisory only and
// left untouched; an unknown executable op is marked not-reversible.
func deriveSafety(a providers.Action) providers.Action {
	if a.Op == "" {
		return a // advisory suggestion; never executed
	}
	if meta, ok := providers.Ops[a.Op]; ok {
		a.Reversible = meta.Reversible
		a.BlastRadius = meta.Blast
		return a
	}
	a.Reversible = false // unknown executable op: treat as unsafe
	return a
}

// knownOp reports whether op is a server-recognized executable operation.
func knownOp(op string) bool {
	_, ok := providers.Ops[op]
	return ok
}
