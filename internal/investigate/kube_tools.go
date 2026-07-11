// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
		fmt.Fprintf(&b, "%s  %s ready=%s%s%s\n", p.Name, p.Phase, p.Ready, podTimes(p), podNet(p))
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
		"FailedMount, FailedAttachVolume, BackOff, Unhealthy probes. Events are most-recent-first " +
		"with last-seen timestamps and repeat counts — use the times to correlate with change/deploy " +
		"times. Optionally scope to one object's name."
}

// Schema returns the JSON schema for the arguments.
func (t KubeEventsTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"object":{"type":"string","description":"optional involvedObject name to scope to"},"all_types":{"type":"boolean","description":"include Normal events too (default: Warning only)"},"since_minutes":{"type":"integer","description":"only events from the last N minutes; in a busy namespace this ensures the newest in-window events are returned (0 or omitted = no time bound). Set e.g. 60 when correlating to a recent change."}},"required":["namespace"]}`
}

// Call lists events and renders them.
func (t KubeEventsTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace    string `json:"namespace"`
		Object       string `json:"object"`
		AllTypes     bool   `json:"all_types"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// Prefer the windowing reader when the backend supports it (K2): a since_minutes
	// window makes the newest in-window events actually reachable in a busy
	// namespace. Fall back to the un-windowed Events for readers that don't (fakes,
	// alternate backends); since_minutes=0 is equivalent either way.
	var (
		events []providers.KubeEvent
		err    error
	)
	if w, ok := t.Kube.(providers.EventWindower); ok {
		events, err = w.EventsSince(ctx, in.Namespace, in.Object, !in.AllTypes, in.SinceMinutes)
	} else {
		events, err = t.Kube.Events(ctx, in.Namespace, in.Object, !in.AllTypes)
	}
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
		// Lead with the last-seen time: it is what lets the model correlate an
		// event to a change/deploy timestamp ("first BackOff at 14:03").
		when := ""
		if !e.LastSeen.IsZero() {
			when = e.LastSeen.UTC().Format(time.RFC3339) + " "
		}
		fmt.Fprintf(&b, "%s%s %s %s%s: %s\n", when, e.Type, e.Object, e.Reason, count, e.Message)
	}
	return b.String(), nil
}

// podTimes renders the pod's time anchor (K1) compactly and only when present: the
// restart count, the pod age, and the last-terminated window. A fresh, never-
// restarted pod adds no noise. It is what gives pod_status a notion of WHEN, so a
// crash loop can be tied to a change/deploy time.
func podTimes(p providers.PodStatus) string {
	var parts []string
	if p.Restarts > 0 {
		parts = append(parts, fmt.Sprintf("restarts=%d", p.Restarts))
	}
	if !p.CreatedAt.IsZero() {
		parts = append(parts, "age="+podAge(time.Since(p.CreatedAt)))
	}
	// The last-terminated window pins the most recent crash to a wall-clock time
	// the model can correlate; show it only when the pod has actually terminated.
	if !p.LastTerminatedFinished.IsZero() {
		parts = append(parts, "last-exit="+p.LastTerminatedFinished.UTC().Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
}

// podAge renders a duration as a short human age (e.g. "15m", "3h", "2d"),
// mirroring kubectl's AGE column — a compact anchor, not a precise timestamp.
func podAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// podNet renders the pod's IP/node context compactly and only when present, so an
// unscheduled pod (no IP yet) adds no noise. It is what lets the model bridge a
// network_drops IP (VPC/Hubble names an IP, not a pod) back to this workload (B8).
func podNet(p providers.PodStatus) string {
	var parts []string
	if p.PodIP != "" {
		parts = append(parts, "ip="+p.PodIP)
	}
	if p.NodeName != "" {
		parts = append(parts, "node="+p.NodeName)
	}
	if p.HostIP != "" {
		parts = append(parts, "hostIP="+p.HostIP)
	}
	if len(parts) == 0 {
		return ""
	}
	return " " + strings.Join(parts, " ")
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
