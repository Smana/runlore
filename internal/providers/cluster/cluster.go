// Package cluster reads Kubernetes pod logs for investigation (read-only) via the
// client-go CoreV1 GetLogs API. It backs the controller_logs tool, which surfaces
// why a Flux controller failed to reconcile a source/object.
package cluster

import (
	"bufio"
	"context"
	"fmt"
	"sort"
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

// PodLogs returns recent log lines from up to maxPods pods matching labelSelector
// in namespace, bounded to the last sinceMinutes. Each line is prefixed with its
// pod name. Best-effort: a pod whose log stream fails is skipped, not fatal.
func (r *Reader) PodLogs(ctx context.Context, namespace, labelSelector string, sinceMinutes int) (providers.LogResult, error) {
	pods, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods (%s/%s): %w", namespace, labelSelector, err)
	}
	var since *int64
	if sinceMinutes > 0 {
		s := int64(sinceMinutes) * 60
		since = &s
	}
	tail := int64(tailLines)
	var out providers.LogResult
	for i := range pods.Items {
		if i >= maxPods {
			break
		}
		name := pods.Items[i].Name
		stream, err := r.client.CoreV1().Pods(namespace).
			GetLogs(name, &corev1.PodLogOptions{SinceSeconds: since, TailLines: &tail}).
			Stream(ctx)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			out = append(out, providers.LogLine{Message: name + ": " + sc.Text()})
		}
		_ = stream.Close()
	}
	return out, nil
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
	out := make([]providers.KubeEvent, 0, len(list.Items))
	for i := range list.Items {
		e := &list.Items[i]
		if warnOnly && e.Type != corev1.EventTypeWarning {
			continue
		}
		out = append(out, providers.KubeEvent{
			Type:    e.Type,
			Reason:  e.Reason,
			Object:  e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Message: e.Message,
			Count:   e.Count,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return eventTime(&list.Items[i]).After(eventTime(&list.Items[j])) })
	return out, nil
}

func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	return e.EventTime.Time
}
