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

// defaultContainerAnnotation names the pod's primary (app) container; kubectl sets
// it so `kubectl logs` picks the app over an injected sidecar. We honor it to read
// the app container FIRST (its logs are the ones an investigation wants).
const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// PodLogs returns recent log lines from up to maxPods pods selected by q, bounded to
// the last q.SinceMinutes. When q.Previous is true it reads each pod's last-terminated
// container (the crash output of a CrashLoopBackOff) instead of the running one. Each
// line is prefixed with "<pod>/<container>". Best-effort: a pod whose log stream fails
// still emits a marker line so a tool error reads as missing data, not silence.
//
// B6 (CORE-706): every container of a pod is read explicitly. The old code set no
// Container in PodLogOptions, and the Kubernetes API REJECTS a log request on a pod
// with more than one container ("a container name must be specified…"); that error
// was swallowed, so every pod with an istio/linkerd/cloudsql sidecar silently
// yielded nothing.
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
	var out providers.LogResult
	for i := range pods.Items {
		if i >= maxPods {
			// The fan-out cap bound silently before: the model saw N pods' logs with no
			// signal that more matched, so it could wrongly conclude a selector was
			// fully covered. Emit a sentinel naming the shortfall so it narrows instead.
			out = append(out, providers.LogLine{
				Message: fmt.Sprintf("… %d more pods not shown (fan-out capped at %d) — narrow the selector to see the rest", len(pods.Items)-maxPods, maxPods),
			})
			break
		}
		pod := &pods.Items[i]
		containers := containerOrder(pod, q.Container)
		if len(containers) == 0 {
			continue // a pod with no containers in spec (shouldn't happen) has nothing to read
		}
		// Split the per-pod tail budget across the pod's containers so a multi-
		// container pod doesn't blow the bound; at least 1 line each.
		tail := int64(tailLines) / int64(len(containers))
		if tail < 1 {
			tail = 1
		}
		for _, c := range containers {
			// Timestamps: the kubelet prefixes each line with RFC3339Nano; we parse it
			// into LogLine.Time so the renderer can show WHEN a line was emitted (the
			// signal that correlates a crash to a change/event time).
			stream, err := r.client.CoreV1().Pods(q.Namespace).
				GetLogs(pod.Name, &corev1.PodLogOptions{Container: c, SinceSeconds: since, TailLines: &tail, Previous: q.Previous, Timestamps: true}).
				Stream(ctx)
			if err != nil {
				// A tool error is missing data, not silence: surface it as a marker
				// line so the model knows this container's logs were unavailable.
				out = append(out, providers.LogLine{Message: fmt.Sprintf("%s/%s: log stream failed: %v", pod.Name, c, err)})
				continue
			}
			sc := bufio.NewScanner(stream)
			sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
			for sc.Scan() {
				ts, msg := splitLogTimestamp(sc.Text())
				out = append(out, providers.LogLine{Time: ts, Message: pod.Name + "/" + c + ": " + msg})
			}
			_ = stream.Close()
		}
	}
	return out, nil
}

// containerOrder returns the container names to read for a pod. When q.Container is
// set it scopes to that one; otherwise it returns all spec containers with the
// default-container (the app, per the kubectl annotation) moved first so its logs
// lead. A pod's own container ordering is otherwise preserved.
func containerOrder(pod *corev1.Pod, only string) []string {
	if only != "" {
		return []string{only}
	}
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	if def := pod.Annotations[defaultContainerAnnotation]; def != "" {
		for i, n := range names {
			if n == def && i > 0 {
				names = append([]string{def}, append(names[:i:i], names[i+1:]...)...)
				break
			}
		}
	}
	return names
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
	// PodIP/NodeName/HostIP are already on the object — surfacing them lets the model
	// bridge a network_drops IP back to this pod at zero extra API cost (B8, CORE-707).
	ps := providers.PodStatus{
		Name:      p.Name,
		Phase:     string(p.Status.Phase),
		PodIP:     p.Status.PodIP,
		NodeName:  p.Spec.NodeName,
		HostIP:    p.Status.HostIP,
		CreatedAt: p.CreationTimestamp.Time, // pod age anchor (K1)
	}
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
			// Restarts (K1): sum RestartCount across containers — the pod-level
			// count of how many times a container has looped. Track the most-recent
			// last-termination window so a crash loop has a start/finish time.
			ps.Restarts += int(c.RestartCount)
			if lt := c.LastTerminationState.Terminated; lt != nil {
				if lt.FinishedAt.After(ps.LastTerminatedFinished) {
					ps.LastTerminatedStarted = lt.StartedAt.Time
					ps.LastTerminatedFinished = lt.FinishedAt.Time
				}
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

// eventPageLimit is the per-page fetch size; eventMaxPages bounds the total pages
// walked when a time window is set, so a busy namespace can't unbound the fetch.
const (
	eventPageLimit = 200
	eventMaxPages  = 10
)

var _ providers.EventWindower = (*Reader)(nil)

// Events returns recent Events in a namespace (optionally for one object,
// optionally Warning-only), most-recent first. It is EventsSince with no time
// window — kept for the KubeReader interface and existing callers.
func (r *Reader) Events(ctx context.Context, namespace, objectName string, warnOnly bool) ([]providers.KubeEvent, error) {
	return r.EventsSince(ctx, namespace, objectName, warnOnly, 0)
}

// EventsSince returns recent Events in a namespace, dropping any older than
// sinceMinutes (0 = no lower bound), most-recent first.
//
// K2: the old code fetched a single Limit:200 page and only sorted newest-first
// AFTER fetching — so in a busy namespace the newest events could sit on a page
// never fetched. When a window is set we walk pages (bounded by eventMaxPages) and
// keep every in-window event, so the newest in-window events are actually returned.
func (r *Reader) EventsSince(ctx context.Context, namespace, objectName string, warnOnly bool, sinceMinutes int) ([]providers.KubeEvent, error) {
	var cutoff time.Time
	if sinceMinutes > 0 {
		cutoff = time.Now().Add(-time.Duration(sinceMinutes) * time.Minute)
	}
	// Carry each kept event's timestamp alongside it: warnOnly / window filtering
	// makes the kept slice shorter than the fetched items, so the sort must not
	// index back into the raw list (the indices diverge and ordering would read
	// the wrong events' timestamps).
	type timedEvent struct {
		ev providers.KubeEvent
		at time.Time
	}
	var kept []timedEvent

	opts := metav1.ListOptions{Limit: eventPageLimit}
	if objectName != "" {
		opts.FieldSelector = "involvedObject.name=" + objectName
	}
	// Windowing walks pages until the window is fully covered; without a window we
	// keep today's behavior exactly (a single Limit:200 page, no paging).
	maxPages := 1
	if !cutoff.IsZero() {
		maxPages = eventMaxPages
	}
	for page := 0; page < maxPages; page++ {
		list, err := r.client.CoreV1().Events(namespace).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("list events (%s): %w", namespace, err)
		}
		for i := range list.Items {
			e := &list.Items[i]
			if warnOnly && e.Type != corev1.EventTypeWarning {
				continue
			}
			at := eventTime(e)
			if !cutoff.IsZero() && at.Before(cutoff) {
				continue // outside the window
			}
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
		if list.Continue == "" {
			break // last page
		}
		opts.Continue = list.Continue
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
