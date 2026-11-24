// SPDX-License-Identifier: Apache-2.0

package config

// ActionMode controls how far RunLore may go when acting on the cluster.
// Default (zero value) is read-only.
type ActionMode string

// Action modes, from read-only to autonomous.
const (
	ActionOff     ActionMode = "off"     // read-only (default): no action tools registered
	ActionSuggest ActionMode = "suggest" // propose a command/PR; never execute
	ActionApprove ActionMode = "approve" // execute only after explicit human approval
	ActionAuto    ActionMode = "auto"    // EXPERIMENTAL/frozen (FEAT-1): execute in-envelope, no click — not for prod
)

// ActionPolicy gates cluster-mutating actions — the upper rungs of the autonomy
// ladder. v1 ships ActionOff; the type exists so active tools can be added later
// behind a gate without re-architecting (see docs/design.md §9, "Act").
type ActionPolicy struct {
	Mode             ActionMode  `yaml:"mode"`               // off | suggest | approve | auto (experimental, frozen)
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
