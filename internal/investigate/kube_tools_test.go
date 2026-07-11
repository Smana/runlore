// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

type fakeKube struct {
	pods   []providers.PodStatus
	events []providers.KubeEvent
}

func (f fakeKube) PodStatuses(context.Context, string, string) ([]providers.PodStatus, error) {
	return f.pods, nil
}
func (f fakeKube) Events(context.Context, string, string, bool) ([]providers.KubeEvent, error) {
	return f.events, nil
}

func TestPodStatusTool(t *testing.T) {
	tool := PodStatusTool{Kube: fakeKube{pods: []providers.PodStatus{
		{Name: "harbor-registry-x", Phase: "Pending", Ready: "1/2", Reasons: []string{
			"registry: CreateContainerConfigError: couldn't find key username in Secret tooling/xplane-harbor-access-key",
		}},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"tooling"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The exact root cause (the missing key) must reach the model.
	if !strings.Contains(out, "CreateContainerConfigError") || !strings.Contains(out, "couldn't find key username") {
		t.Fatalf("pod_status must surface the container reason + message, got:\n%s", out)
	}
	empty, _ := PodStatusTool{Kube: fakeKube{}}.Call(context.Background(), `{"namespace":"x"}`)
	if !strings.Contains(empty, "no pods") {
		t.Fatalf("expected empty message, got %q", empty)
	}
}

// B8 (CORE-707): pod_status output must carry podIP/nodeName so the model can bridge
// a network_drops IP back to a pod. Absent IPs (unscheduled pod) add no noise.
func TestPodStatusToolShowsIPs(t *testing.T) {
	tool := PodStatusTool{Kube: fakeKube{pods: []providers.PodStatus{
		{Name: "web-0", Phase: "Running", Ready: "1/1", PodIP: "10.42.3.7", NodeName: "ip-10-0-1-23", HostIP: "10.0.1.23"},
		{Name: "pending-0", Phase: "Pending", Ready: "0/1"},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "ip=10.42.3.7") || !strings.Contains(out, "node=ip-10-0-1-23") || !strings.Contains(out, "hostIP=10.0.1.23") {
		t.Fatalf("pod_status must surface podIP/node/hostIP for a scheduled pod, got:\n%s", out)
	}
	// A pod without an IP (unscheduled) must not render an empty ip=/node=.
	if strings.Contains(out, "ip= ") || strings.Contains(out, "node= ") {
		t.Fatalf("absent IPs must add no noise, got:\n%s", out)
	}
}

// selectorKube returns matched pods only for a specific selector, and allPods for
// the empty (whole-namespace) selector — modelling a workload whose real labels
// don't match a guessed `app=<name>` selector.
type selectorKube struct {
	matchSelector string
	matched       []providers.PodStatus
	allPods       []providers.PodStatus
}

func (f selectorKube) PodStatuses(_ context.Context, _ string, selector string) ([]providers.PodStatus, error) {
	if selector == "" {
		return f.allPods, nil
	}
	if selector == f.matchSelector {
		return f.matched, nil
	}
	return nil, nil
}
func (f selectorKube) Events(context.Context, string, string, bool) ([]providers.KubeEvent, error) {
	return nil, nil
}

// A guessed label selector that matches nothing must NOT read as an empty
// namespace: that false negative produced confident-but-wrong "workload not
// deployed" RCAs. pod_status falls back to the whole namespace so the real
// (e.g. CrashLoopBackOff) pods still reach the model.
func TestPodStatusToolSelectorFallback(t *testing.T) {
	tool := PodStatusTool{Kube: selectorKube{
		matchSelector: "app.kubernetes.io/name=image-gallery",
		allPods: []providers.PodStatus{
			{Name: "xplane-image-gallery-xwdk7", Phase: "Running", Ready: "0/1", Reasons: []string{
				"image-gallery: CrashLoopBackOff: back-off restarting failed container",
			}},
		},
	}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps","selector":"app=xplane-image-gallery"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(out, "no pods in namespace") {
		t.Fatalf("a non-matching selector must not read as an empty namespace, got:\n%s", out)
	}
	if !strings.Contains(out, "xplane-image-gallery-xwdk7") || !strings.Contains(out, "CrashLoopBackOff") {
		t.Fatalf("fallback must surface the real namespace pods, got:\n%s", out)
	}
	if !strings.Contains(out, "app=xplane-image-gallery") {
		t.Fatalf("fallback should note the selector that matched nothing, got:\n%s", out)
	}
}

func TestKubeEventsTool(t *testing.T) {
	lastSeen := time.Date(2026, 7, 1, 14, 3, 5, 0, time.UTC)
	tool := KubeEventsTool{Kube: fakeKube{events: []providers.KubeEvent{
		{Type: "Warning", Reason: "FailedScheduling", Object: "Pod/valkey-0", Count: 13, LastSeen: lastSeen,
			Message: "0/9 nodes are available: 4 Insufficient cpu, 2 Insufficient memory."},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "FailedScheduling") || !strings.Contains(out, "Insufficient cpu") || !strings.Contains(out, "x13") {
		t.Fatalf("kube_events must surface reason + message + count, got:\n%s", out)
	}
	// The last-seen timestamp is what lets the model correlate an event to a
	// change/deploy time ("first BackOff at 14:03").
	if !strings.Contains(out, "2026-07-01T14:03:05Z") {
		t.Fatalf("kube_events must surface the event's last-seen time, got:\n%s", out)
	}
}

// A zero LastSeen (older API objects, fakes) must not render a bogus epoch time.
func TestKubeEventsToolZeroTimeOmitted(t *testing.T) {
	tool := KubeEventsTool{Kube: fakeKube{events: []providers.KubeEvent{
		{Type: "Warning", Reason: "BackOff", Object: "Pod/x", Message: "restarting"},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Fatalf("kube_events must omit a zero timestamp, got:\n%s", out)
	}
}
