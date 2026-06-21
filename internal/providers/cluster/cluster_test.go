package cluster

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPodLogs(t *testing.T) {
	pod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "flux-system", Labels: labels}}
	}
	client := fake.NewSimpleClientset(
		pod("source-controller-1", map[string]string{"app": "source-controller"}),
		pod("kustomize-controller-1", map[string]string{"app": "kustomize-controller"}),
	)
	r := New(client)

	lines, err := r.PodLogs(context.Background(), "flux-system", "app=source-controller", 30)
	if err != nil {
		t.Fatalf("PodLogs: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("expected log lines from the matching pod")
	}
	joined := ""
	for _, l := range lines {
		joined += l.Message + "\n"
	}
	// Only the label-matched pod's logs are returned (scoping), prefixed by pod name.
	if !strings.Contains(joined, "source-controller-1:") {
		t.Fatalf("expected source-controller-1 logs, got:\n%s", joined)
	}
	if strings.Contains(joined, "kustomize-controller-1:") {
		t.Fatalf("label selector leaked another pod's logs:\n%s", joined)
	}
}
