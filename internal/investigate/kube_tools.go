package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// PodStatusTool surfaces pod-level failures (CreateContainerConfigError,
// ImagePullBackOff, CrashLoopBackOff, …) that never reach logs because the
// container never started — the gap the GitOps/logs/metrics tools can't fill.
type PodStatusTool struct {
	Kube providers.KubeReader
}

// Name returns the tool name.
func (t PodStatusTool) Name() string { return "pod_status" }

// Description returns the tool description.
func (t PodStatusTool) Description() string {
	return "List pod health in a namespace with per-container waiting/terminated reasons + messages — " +
		"the FIRST tool to reach for when a workload won't start (CreateContainerConfigError, " +
		"ImagePullBackOff, CrashLoopBackOff, RunContainerError). The message names the exact cause " +
		"(e.g. a missing Secret/ConfigMap key). Unhealthy pods are listed first. Optional label selector."
}

// Schema returns the JSON schema for the arguments.
func (t PodStatusTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"selector":{"type":"string","description":"optional label selector, e.g. app=harbor"}},"required":["namespace"]}`
}

// Call lists pod statuses and renders them.
func (t PodStatusTool) Call(ctx context.Context, args string) (string, error) {
	var in struct{ Namespace, Selector string }
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	pods, err := t.Kube.PodStatuses(ctx, in.Namespace, in.Selector)
	if err != nil {
		return "", err
	}
	// A caller-supplied selector that matches nothing is indistinguishable from a
	// genuinely empty namespace — and that false negative has produced confident-
	// but-wrong "workload not deployed" findings (the model tends to guess
	// app=<name>, but workloads commonly label with app.kubernetes.io/name). Fall
	// back to the whole namespace so a non-matching selector can't read as "no pods
	// exist"; the model still sees the real (e.g. CrashLoopBackOff) pods.
	var note string
	if len(pods) == 0 && in.Selector != "" {
		all, err := t.Kube.PodStatuses(ctx, in.Namespace, "")
		if err != nil {
			return "", err
		}
		if len(all) > 0 {
			note = fmt.Sprintf("selector %s matched no pods; showing all %d pod(s) in namespace %q instead:\n",
				in.Selector, len(all), in.Namespace)
			pods = all
		}
	}
	if len(pods) == 0 {
		return fmt.Sprintf("no pods in namespace %q%s", in.Namespace, selectorSuffix(in.Selector)), nil
	}
	var b strings.Builder
	b.WriteString(note)
	for i, p := range pods {
		if i >= maxToolRows {
			fmt.Fprintf(&b, "… (%d more)\n", len(pods)-i)
			break
		}
		fmt.Fprintf(&b, "%s  %s ready=%s\n", p.Name, p.Phase, p.Ready)
		for _, r := range p.Reasons {
			fmt.Fprintf(&b, "  ⚠ %s\n", r)
		}
	}
	return b.String(), nil
}

// KubeEventsTool surfaces causes that live in the Kubernetes event stream rather
// than logs or status — FailedScheduling (Insufficient cpu/memory), FailedMount,
// FailedAttachVolume, BackOff, etc.
type KubeEventsTool struct {
	Kube providers.KubeReader
}

// Name returns the tool name.
func (t KubeEventsTool) Name() string { return "kube_events" }

// Description returns the tool description.
func (t KubeEventsTool) Description() string {
	return "List recent Kubernetes Events in a namespace (Warning-only by default) — causes that live " +
		"in the event stream, not logs or status: FailedScheduling (Insufficient cpu/memory), " +
		"FailedMount, FailedAttachVolume, BackOff, Unhealthy probes. Optionally scope to one object's name."
}

// Schema returns the JSON schema for the arguments.
func (t KubeEventsTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"object":{"type":"string","description":"optional involvedObject name to scope to"},"all_types":{"type":"boolean","description":"include Normal events too (default: Warning only)"}},"required":["namespace"]}`
}

// Call lists events and renders them.
func (t KubeEventsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace string `json:"namespace"`
		Object    string `json:"object"`
		AllTypes  bool   `json:"all_types"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	events, err := t.Kube.Events(ctx, in.Namespace, in.Object, !in.AllTypes)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return fmt.Sprintf("no %s events in namespace %q", warnLabel(in.AllTypes), in.Namespace), nil
	}
	var b strings.Builder
	for i, e := range events {
		if i >= maxToolRows {
			fmt.Fprintf(&b, "… (%d more)\n", len(events)-i)
			break
		}
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf(" (x%d)", e.Count)
		}
		fmt.Fprintf(&b, "%s %s %s%s: %s\n", e.Type, e.Object, e.Reason, count, e.Message)
	}
	return b.String(), nil
}

func selectorSuffix(sel string) string {
	if sel == "" {
		return ""
	}
	return " matching " + sel
}

func warnLabel(allTypes bool) string {
	if allTypes {
		return ""
	}
	return "Warning"
}
