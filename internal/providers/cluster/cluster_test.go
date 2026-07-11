// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Smana/runlore/internal/providers"
)

func TestPodLogs(t *testing.T) {
	pod := func(name string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "flux-system", Labels: labels},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "manager"}}},
		}
	}
	client := fake.NewSimpleClientset(
		pod("source-controller-1", map[string]string{"app": "source-controller"}),
		pod("kustomize-controller-1", map[string]string{"app": "kustomize-controller"}),
	)
	r := New(client)

	lines, err := r.PodLogs(context.Background(), providers.PodLogQuery{Namespace: "flux-system", LabelSelector: "app=source-controller", SinceMinutes: 30})
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
	// Only the label-matched pod's logs are returned (scoping), attributed <pod>/<container>.
	if !strings.Contains(joined, "source-controller-1/manager:") {
		t.Fatalf("expected source-controller-1 logs attributed by container, got:\n%s", joined)
	}
	if strings.Contains(joined, "kustomize-controller-1/") {
		t.Fatalf("label selector leaked another pod's logs:\n%s", joined)
	}
}

// TestSplitLogTimestamp covers parsing the RFC3339Nano prefix the kubelet adds
// when PodLogOptions.Timestamps is set — the per-line time that lets the model
// correlate a log line to a change/event timestamp. Lines without a parseable
// prefix (fakes, malformed) pass through untouched with a zero time.
func TestSplitLogTimestamp(t *testing.T) {
	cases := []struct {
		name, in, wantMsg string
		wantZero          bool
	}{
		{"kubelet timestamped line", "2026-07-01T14:03:05.123456789Z panic: boom", "panic: boom", false},
		{"no timestamp", "fake logs", "fake logs", true},
		{"timestamp only", "2026-07-01T14:03:05Z", "", false},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		ts, msg := splitLogTimestamp(c.in)
		if msg != c.wantMsg {
			t.Errorf("%s: msg = %q, want %q", c.name, msg, c.wantMsg)
		}
		if ts.IsZero() != c.wantZero {
			t.Errorf("%s: ts.IsZero() = %v, want %v", c.name, ts.IsZero(), c.wantZero)
		}
	}
	ts, _ := splitLogTimestamp("2026-07-01T14:03:05.123456789Z panic: boom")
	if ts.UTC().Format("2006-01-02T15:04:05Z") != "2026-07-01T14:03:05Z" {
		t.Errorf("parsed timestamp wrong: %v", ts)
	}
}

// B6 (CORE-706): a pod with an app container + an istio sidecar must not silently
// yield nothing. The old code set no Container, and the Kubernetes API rejects a
// log request on a >1-container pod ("a container name must be specified…"); the
// error was swallowed by `continue`. PodLogs must iterate the pod's containers and
// attribute each line to <pod>/<container>, ordering the app container (named by
// the default-container annotation) first.
func TestPodLogsMultiContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-6f9d5c8b7-abcde",
			Namespace: "apps",
			Labels:    map[string]string{"app": "web"},
			// Tell the API which container is the app; the reader must read it first.
			Annotations: map[string]string{"kubectl.kubernetes.io/default-container": "app"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "istio-proxy"}, // sidecar listed first in the spec on purpose
			{Name: "app"},
		}},
	}
	r := New(fake.NewSimpleClientset(pod))
	lines, err := r.PodLogs(context.Background(), providers.PodLogQuery{Namespace: "apps", LabelSelector: "app=web", SinceMinutes: 30})
	if err != nil {
		t.Fatalf("PodLogs: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("a multi-container pod must not silently yield zero lines (B6)")
	}
	// Each line is attributed to a specific container, not just the pod.
	joined := ""
	for _, l := range lines {
		joined += l.Message + "\n"
	}
	if !strings.Contains(joined, "web-6f9d5c8b7-abcde/app:") {
		t.Fatalf("app-container logs must be returned and attributed <pod>/<container>, got:\n%s", joined)
	}
	// The default-container (app) must be read before the sidecar.
	iApp := strings.Index(joined, "/app:")
	iProxy := strings.Index(joined, "/istio-proxy:")
	if iProxy >= 0 && iApp >= 0 && iApp > iProxy {
		t.Fatalf("default-container (app) must be ordered before the sidecar, got:\n%s", joined)
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

// K1 (v0.9): pod_status is the only cluster tool with no time anchor. It must carry
// the container restart count, the pod's age (from CreationTimestamp), and the last
// termination's started/finished times so the model can time-correlate a crash loop.
func TestPodStatusCarriesRestartsAndTimes(t *testing.T) {
	started := time.Date(2026, 7, 1, 14, 0, 0, 0, time.UTC)
	finished := time.Date(2026, 7, 1, 14, 2, 30, 0, time.UTC)
	created := time.Date(2026, 7, 1, 13, 45, 0, 0, time.UTC)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "mem-hog",
			Namespace:         "runlore-eval",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         "app",
				Ready:        false,
				RestartCount: 7,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff", Message: "back-off restarting failed container"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason:     "OOMKilled",
					ExitCode:   137,
					StartedAt:  metav1.NewTime(started),
					FinishedAt: metav1.NewTime(finished),
				}},
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
	if got[0].Restarts != 7 {
		t.Fatalf("Restarts = %d, want 7 (summed container RestartCount)", got[0].Restarts)
	}
	if got[0].CreatedAt.UTC() != created {
		t.Fatalf("CreatedAt = %v, want %v (pod age anchor)", got[0].CreatedAt, created)
	}
	if got[0].LastTerminatedStarted.UTC() != started {
		t.Fatalf("LastTerminatedStarted = %v, want %v", got[0].LastTerminatedStarted, started)
	}
	if got[0].LastTerminatedFinished.UTC() != finished {
		t.Fatalf("LastTerminatedFinished = %v, want %v", got[0].LastTerminatedFinished, finished)
	}
}

// B8 (CORE-707): pod_status must carry podIP/nodeName/hostIP so the model can bridge
// a network_drops IP back to a pod. They live on the corev1.Pod object already.
func TestPodStatusCarriesIPs(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps"},
		Spec:       corev1.PodSpec{NodeName: "ip-10-0-1-23.ec2.internal"},
		Status: corev1.PodStatus{
			Phase:  corev1.PodRunning,
			PodIP:  "10.42.3.7",
			HostIP: "10.0.1.23",
		},
	}
	r := New(fake.NewSimpleClientset(pod))
	got, err := r.PodStatuses(context.Background(), "apps", "")
	if err != nil {
		t.Fatalf("PodStatuses: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 pod, got %d", len(got))
	}
	if got[0].PodIP != "10.42.3.7" {
		t.Fatalf("podIP = %q, want 10.42.3.7 (the IP a network drop names)", got[0].PodIP)
	}
	if got[0].NodeName != "ip-10-0-1-23.ec2.internal" {
		t.Fatalf("nodeName = %q, want the scheduling node", got[0].NodeName)
	}
	if got[0].HostIP != "10.0.1.23" {
		t.Fatalf("hostIP = %q, want 10.0.1.23", got[0].HostIP)
	}
}
