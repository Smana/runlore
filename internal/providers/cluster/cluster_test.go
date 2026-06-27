package cluster

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

	lines, err := r.PodLogs(context.Background(), "flux-system", "app=source-controller", 30, false)
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

func TestPodStatusSurfacesOOMAndLimit(t *testing.T) {
	// An OOMKilled pod loops: its CURRENT state is Waiting{CrashLoopBackOff}; the
	// OOMKilled/exit-137 signal lives in LastTerminationState. pod_status must surface
	// that + the container's memory limit so the model can tie OOM → the limit.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "mem-hog", Namespace: "runlore-eval"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
			},
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				Ready: false,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting failed container"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "OOMKilled", ExitCode: 137}},
			}},
		},
	}
	r := New(fake.NewSimpleClientset(pod))
	got, err := r.PodStatuses(context.Background(), "runlore-eval", "")
	if err != nil {
		t.Fatalf("PodStatuses: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 pod, got %d", len(got))
	}
	joined := strings.Join(got[0].Reasons, " | ")
	for _, want := range []string{"CrashLoopBackOff", "OOMKilled", "137", "64Mi"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("pod_status reasons %q missing %q (the OOM-from-limit signal)", joined, want)
		}
	}
}
