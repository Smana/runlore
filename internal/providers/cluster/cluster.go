// SPDX-License-Identifier: Apache-2.0

// Package cluster reads Kubernetes pod logs for investigation (read-only) via the
// client-go CoreV1 GetLogs API. It backs the controller_logs tool, which surfaces
// why a Flux controller failed to reconcile a source/object.
package cluster

import (
	"bufio"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/Smana/runlore/internal/providers"
)

const (
	maxPods   = 5   // bound the fan-out across matching pods
	tailLines = 300 // per-pod tail cap (bounded fetch)
)

// Reader reads pod logs via a client-go clientset (read-only).
type Reader struct{ client kubernetes.Interface }

// New builds a log Reader from a clientset.
func New(client kubernetes.Interface) *Reader { return &Reader{client: client} }

var _ providers.LogReader = (*Reader)(nil)

// PodLogs returns recent log lines from up to maxPods pods selected by q, bounded to
// the last q.SinceMinutes. When q.Previous is true it reads each pod's last-terminated
// container (the crash output of a CrashLoopBackOff) instead of the running one. Each
// line is prefixed with its pod name. Best-effort: a pod whose log stream fails is
// skipped, not fatal.
func (r *Reader) PodLogs(ctx context.Context, q providers.PodLogQuery) (providers.LogResult, error) {
	pods, err := r.client.CoreV1().Pods(q.Namespace).List(ctx, metav1.ListOptions{LabelSelector: q.LabelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods (%s/%s): %w", q.Namespace, q.LabelSelector, err)
	}
	var since *int64
	if q.SinceMinutes > 0 {
		s := int64(q.SinceMinutes) * 60
		since = &s
	}
	tail := int64(tailLines)
	var out providers.LogResult
	for i := range pods.Items {
		if i >= maxPods {
			break
		}
		name := pods.Items[i].Name
		// Timestamps: the kubelet prefixes each line with RFC3339Nano; we parse it
		// into LogLine.Time so the renderer can show WHEN a line was emitted (the
		// signal that correlates a crash to a change/event time).
		stream, err := r.client.CoreV1().Pods(q.Namespace).
			GetLogs(name, &corev1.PodLogOptions{SinceSeconds: since, TailLines: &tail, Previous: q.Previous, Timestamps: true}).
			Stream(ctx)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			ts, msg := splitLogTimestamp(sc.Text())
			out = append(out, providers.LogLine{Time: ts, Message: name + ": " + msg})
		}
		_ = stream.Close()
	}
	return out, nil
}

// splitLogTimestamp splits the RFC3339Nano prefix that PodLogOptions.Timestamps
// adds to each line into a time plus the bare message. A line without a parseable
// prefix (a fake client, a malformed line) is returned untouched with a zero time,
// so callers never lose content.
func splitLogTimestamp(line string) (time.Time, string) {
	prefix, rest, found := strings.Cut(line, " ")
	if !found {
		prefix, rest = line, ""
	}
	ts, err := time.Parse(time.RFC3339Nano, prefix)
	if err != nil {
		return time.Time{}, line
	}
	return ts, rest
}

var _ providers.KubeReader = (*Reader)(nil)

// PodStatuses returns pod health in a namespace (optional label selector), with
// per-container waiting/terminated reasons — surfacing pod-level failures
// (CreateContainerConfigError, ImagePullBackOff, CrashLoopBackOff) that never
// reach logs because the container never started. Unhealthy pods sort first.
func (r *Reader) PodStatuses(ctx context.Context, namespace, labelSelector string) ([]providers.PodStatus, error) {
	pods, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods (%s): %w", namespace, err)
	}
	out := make([]providers.PodStatus, 0, len(pods.Items))
	for i := range pods.Items {
		out = append(out, podStatus(&pods.Items[i]))
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Healthy && out[j].Healthy })
	return out, nil
}

func podStatus(p *corev1.Pod) providers.PodStatus {
	ps := providers.PodStatus{Name: p.Name, Phase: string(p.Status.Phase)}
	// Container memory limits (from the spec) — needed to tie an OOMKill to the limit.
	memLimit := map[string]string{}
	addLimits := func(cs []corev1.Container) {
		for _, c := range cs {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				memLimit[c.Name] = q.String()
			}
		}
	}
	addLimits(p.Spec.InitContainers)
	addLimits(p.Spec.Containers)
	ready, total := 0, 0
	collect := func(cs []corev1.ContainerStatus) {
		for _, c := range cs {
			total++
			if c.Ready {
				ready++
			}
			oom := false
			switch {
			case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "ContainerCreating" && c.State.Waiting.Reason != "PodInitializing":
				msg := fmt.Sprintf("%s: %s: %s", c.Name, c.State.Waiting.Reason, c.State.Waiting.Message)
				// A waiting (e.g. CrashLoopBackOff) container hides WHY it died — the
				// reason/exit code live in the LAST termination (e.g. OOMKilled, exit 137).
				if lt := c.LastTerminationState.Terminated; lt != nil && lt.Reason != "" {
					msg += fmt.Sprintf(" [last termination: %s (exit %d)]", lt.Reason, lt.ExitCode)
					oom = lt.Reason == "OOMKilled"
				}
				ps.Reasons = append(ps.Reasons, msg)
			case c.State.Terminated != nil && c.State.Terminated.Reason != "" && c.State.Terminated.Reason != "Completed":
				ps.Reasons = append(ps.Reasons, fmt.Sprintf("%s: %s (exit %d): %s", c.Name, c.State.Terminated.Reason, c.State.Terminated.ExitCode, c.State.Terminated.Message))
				oom = c.State.Terminated.Reason == "OOMKilled"
			}
			if oom {
				if lim, ok := memLimit[c.Name]; ok {
					ps.Reasons = append(ps.Reasons, fmt.Sprintf("%s: memory limit %s (OOMKilled ⇒ limit likely too low for the workload)", c.Name, lim))
				}
			}
		}
	}
	collect(p.Status.InitContainerStatuses)
	collect(p.Status.ContainerStatuses)
	ps.Ready = fmt.Sprintf("%d/%d", ready, total)
	ps.Healthy = len(ps.Reasons) == 0 && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded) && ready == total
	return ps
}

// Events returns recent Events in a namespace (optionally for one object,
// optionally Warning-only), most-recent first.
func (r *Reader) Events(ctx context.Context, namespace, objectName string, warnOnly bool) ([]providers.KubeEvent, error) {
	opts := metav1.ListOptions{Limit: 200}
	if objectName != "" {
		opts.FieldSelector = "involvedObject.name=" + objectName
	}
	list, err := r.client.CoreV1().Events(namespace).List(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("list events (%s): %w", namespace, err)
	}
	// Carry each kept event's timestamp alongside it: warnOnly filtering makes
	// the kept slice shorter than list.Items, so the sort must not index back
	// into list.Items (the indices diverge and ordering would read the wrong
	// events' timestamps).
	type timedEvent struct {
		ev providers.KubeEvent
		at time.Time
	}
	kept := make([]timedEvent, 0, len(list.Items))
	for i := range list.Items {
		e := &list.Items[i]
		if warnOnly && e.Type != corev1.EventTypeWarning {
			continue
		}
		at := eventTime(e)
		kept = append(kept, timedEvent{
			ev: providers.KubeEvent{
				Type:     e.Type,
				Reason:   e.Reason,
				Object:   e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
				Message:  e.Message,
				Count:    e.Count,
				LastSeen: at,
			},
			at: at,
		})
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].at.After(kept[j].at) })
	out := make([]providers.KubeEvent, len(kept))
	for i := range kept {
		out[i] = kept[i].ev
	}
	return out, nil
}

func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	return e.EventTime.Time
}
