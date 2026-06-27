package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// PodLogsTool reads recent logs from a workload's pods over the Kubernetes API,
// optionally the PREVIOUS (last-terminated) container — the crash output of a
// CrashLoopBackOff pod that the freshly-restarted container no longer has. This is
// the general workload-log reader (controller_logs is Flux-controller-only).
type PodLogsTool struct {
	Logs providers.LogReader
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
	since := in.SinceMinutes
	if since <= 0 {
		since = 30
	}
	lines, err := t.Logs.PodLogs(ctx, in.Namespace, in.Selector, since, in.Previous)
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
