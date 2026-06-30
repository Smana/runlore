package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// PodLogsTool reads recent logs from a workload's pods over the Kubernetes API,
// optionally the PREVIOUS (last-terminated) container — the crash output of a
// CrashLoopBackOff pod that the freshly-restarted container no longer has. This is
// the general workload-log reader (controller_logs is Flux-controller-only).
//
// Because pod logs carry secrets/PII and are streamed to the external LLM, the
// model is constrained at the application layer to a fixed allowed set of
// namespaces — the incident's own namespace plus an operator-configured allowlist
// — rather than trusting Kubernetes RBAC alone. A request for any other namespace
// is rejected before the cluster is queried.
type PodLogsTool struct {
	Logs providers.LogReader

	// IncidentNamespace is the namespace of the workload under investigation; it is
	// always permitted. Set per-investigation by the loop from req.Workload.Namespace.
	IncidentNamespace string

	// AllowedNamespaces is the operator-configured allowlist of extra namespaces
	// pod_logs may read (config.investigation.pod_log_namespaces). Empty ⇒ the
	// incident namespace is the only permitted namespace (secure by default).
	AllowedNamespaces []string
}

// withIncidentNamespace returns a copy of the tool bound to this investigation's
// incident namespace. It implements incidentScoped so the loop can scope the shared
// tool per request without mutating it.
func (t PodLogsTool) withIncidentNamespace(ns string) Tool {
	t.IncidentNamespace = ns // t is a value receiver: mutating the copy is safe
	return t
}

// namespaceAllowed reports whether ns is in the allowed set: the incident's own
// namespace plus the configured allowlist. An empty incident namespace (on-demand
// runs without a workload) does not widen the set.
func (t PodLogsTool) namespaceAllowed(ns string) bool {
	if ns != "" && ns == t.IncidentNamespace {
		return true
	}
	return slices.Contains(t.AllowedNamespaces, ns)
}

// Name returns the tool name.
func (t PodLogsTool) Name() string { return "pod_logs" }

// Description returns the tool description.
func (t PodLogsTool) Description() string {
	return "Read recent logs from a workload's pods in a namespace (optional label selector, e.g. app=web), over a window (default 30m). Set previous=true to read the LAST TERMINATED container's logs — the crash output of a CrashLoopBackOff/RunContainerError pod, which the freshly-restarted container no longer has. Reach for this when pod_status shows a container crash-looping and you need the actual panic/stack/error that killed it."
}

// Schema returns the JSON schema for the arguments.
func (t PodLogsTool) Schema() string {
	return `{"type":"object","properties":` +
		`{"namespace":{"type":"string"},` +
		`"selector":{"type":"string","description":"optional label selector, e.g. app=web"},` +
		`"previous":{"type":"boolean","description":"read the last-terminated container (crash output) instead of the running one"},` +
		`"since_minutes":{"type":"integer"}},"required":["namespace"]}`
}

// Call fetches and renders the workload's recent pod logs.
func (t PodLogsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace    string `json:"namespace"`
		Selector     string `json:"selector"`
		Previous     bool   `json:"previous"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	if in.Namespace == "" {
		return "", fmt.Errorf("namespace is required")
	}
	// Application-layer guard: pod logs leak secrets/PII to the external LLM, so the
	// model may only read the incident namespace plus the configured allowlist.
	// Return an explanatory, NON-fatal string (so the model can retry an allowed
	// namespace) and do NOT touch the cluster.
	if !t.namespaceAllowed(in.Namespace) {
		return fmt.Sprintf("namespace %q is not permitted for pod_logs (allowed: the incident namespace plus the configured pod_log_namespaces allowlist)", in.Namespace), nil
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 30
	}
	lines, err := t.Logs.PodLogs(ctx, providers.PodLogQuery{Namespace: in.Namespace, LabelSelector: in.Selector, SinceMinutes: since, Previous: in.Previous})
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "no log lines matched", nil
	}
	var b strings.Builder
	renderRows(&b, len(lines), "more lines", func(i int) {
		fmt.Fprintln(&b, lines[i].Message)
	})
	return b.String(), nil
}
