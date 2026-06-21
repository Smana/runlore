// Package cluster reads Kubernetes pod logs for investigation (read-only) via the
// client-go CoreV1 GetLogs API. It backs the controller_logs tool, which surfaces
// why a Flux controller failed to reconcile a source/object.
package cluster

import (
	"bufio"
	"context"
	"fmt"

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
