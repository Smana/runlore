// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Smana/runlore/internal/providers"
)

type fakeGitOpsFailureLister struct {
	failures []providers.FailureEvent
	err      error
}

func (f fakeGitOpsFailureLister) ListFailures(_ context.Context, ns string) ([]providers.FailureEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	if ns == "" {
		return f.failures, nil
	}
	var out []providers.FailureEvent
	for _, ev := range f.failures {
		if ev.Workload.Namespace == ns {
			out = append(out, ev)
		}
	}
	return out, nil
}

func TestStatusHealthyCluster(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "apps"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "web", Ready: true, RestartCount: 0},
			},
		},
	})

	report, err := collectStatus(context.Background(), cs, nil, "", now)
	if err != nil {
		t.Fatalf("collectStatus: %v", err)
	}
	if report.UnhealthyCount() != 0 || len(report.WarningEvents) != 0 {
		t.Fatalf("expected healthy report, got %+v", report)
	}

	var buf bytes.Buffer
	if err := renderStatus(&buf, report, false); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	want := "Cluster healthy (0 GitOps failures, 0 unhealthy pods, 0 recent warning events)."
	if !strings.Contains(buf.String(), want) {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}

	if err := statusExitError(report, true); err != nil {
		t.Errorf("healthy cluster with --exit-code must exit 0, got %v", err)
	}
}

func TestStatusUnhealthyClusterAndFiltering(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset(
		// Healthy pod (excluded)
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ok-pod", Namespace: "apps"},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		},
		// CrashLoopBackOff pod
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout-api-1", Namespace: "apps"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:         "api",
					RestartCount: 5,
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 5m0s"},
					},
				}},
			},
		},
		// Pending pod
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "apps"},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		},
		// High-restart running pod
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "flappy-1", Namespace: "apps"},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "app", Ready: true, RestartCount: 4},
				},
			},
		},
		// Fresh Warning event (5m ago — included)
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-fresh", Namespace: "apps"},
			Type:           corev1.EventTypeWarning,
			Reason:         "BackOff",
			Message:        "Back-off restarting failed container",
			Count:          3,
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "checkout-api-1", Namespace: "apps"},
			LastTimestamp:  metav1.NewTime(now.Add(-5 * time.Minute)),
		},
		// Stale Warning event (20m ago — excluded)
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-stale", Namespace: "apps"},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedScheduling",
			Message:        "old event",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "worker-1", Namespace: "apps"},
			LastTimestamp:  metav1.NewTime(now.Add(-20 * time.Minute)),
		},
		// Normal event (excluded)
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev-normal", Namespace: "apps"},
			Type:           corev1.EventTypeNormal,
			Reason:         "Scheduled",
			Message:        "Successfully assigned",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "ok-pod", Namespace: "apps"},
			LastTimestamp:  metav1.NewTime(now.Add(-1 * time.Minute)),
		},
	)

	gitops := fakeGitOpsFailureLister{failures: []providers.FailureEvent{{
		Workload: providers.Workload{Kind: "Kustomization", Namespace: "flux-system", Name: "checkout"},
		Engine:   providers.EngineFlux,
		Reason:   "HealthCheckFailed",
		Message:  "Deployment/apps/checkout-api not ready",
		When:     now.Add(-2 * time.Minute),
	}}}

	report, err := collectStatus(context.Background(), cs, []providers.GitOpsFailureLister{gitops}, "", now)
	if err != nil {
		t.Fatalf("collectStatus: %v", err)
	}
	if len(report.GitOpsFailures) != 1 {
		t.Fatalf("GitOpsFailures = %d, want 1", len(report.GitOpsFailures))
	}
	if len(report.UnhealthyPods) != 3 {
		t.Fatalf("UnhealthyPods = %d (%+v), want 3", len(report.UnhealthyPods), report.UnhealthyPods)
	}
	if len(report.WarningEvents) != 1 || report.WarningEvents[0].Reason != "BackOff" {
		t.Fatalf("WarningEvents = %+v, want only fresh BackOff event", report.WarningEvents)
	}

	// Human output checks.
	var textBuf bytes.Buffer
	if err := renderStatus(&textBuf, report, false); err != nil {
		t.Fatalf("renderStatus(text): %v", err)
	}
	out := textBuf.String()
	for _, want := range []string{
		"GitOps failures",
		"Unhealthy pods",
		"Recent warning events",
		"Next step:",
		"lore investigate Kustomization/flux-system/checkout",
		"lore investigate Pod/apps/checkout-api-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q:\n%s", want, out)
		}
	}

	// JSON output checks.
	var jsonBuf bytes.Buffer
	if err := renderStatus(&jsonBuf, report, true); err != nil {
		t.Fatalf("renderStatus(json): %v", err)
	}
	var decoded StatusReport
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, jsonBuf.String())
	}
	if len(decoded.GitOpsFailures) != 1 || len(decoded.UnhealthyPods) != 3 || len(decoded.WarningEvents) != 1 {
		t.Errorf("decoded JSON counts mismatch: %+v", decoded)
	}

	// --exit-code returns ErrStatusUnhealthy; without --exit-code returns nil.
	if err := statusExitError(report, false); err != nil {
		t.Errorf("without --exit-code must exit 0, got %v", err)
	}
	if err := statusExitError(report, true); !errors.Is(err, ErrStatusUnhealthy) {
		t.Errorf("with --exit-code = %v, want ErrStatusUnhealthy", err)
	}
}

func TestStatusWarningEventsAloneExitZero(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	cs := fake.NewSimpleClientset(&corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev-warn", Namespace: "apps"},
		Type:           corev1.EventTypeWarning,
		Reason:         "Unhealthy",
		Message:        "Readiness probe failed once",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-1", Namespace: "apps"},
		LastTimestamp:  metav1.NewTime(now.Add(-2 * time.Minute)),
	})

	report, err := collectStatus(context.Background(), cs, nil, "", now)
	if err != nil {
		t.Fatalf("collectStatus: %v", err)
	}
	if len(report.WarningEvents) != 1 {
		t.Fatalf("WarningEvents = %d, want 1", len(report.WarningEvents))
	}
	if err := statusExitError(report, true); err != nil {
		t.Errorf("warning events alone must exit 0 even with --exit-code, got %v", err)
	}
}
