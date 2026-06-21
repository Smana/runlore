package investigate

import (
	"context"
	"strings"
	"testing"

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

func TestKubeEventsTool(t *testing.T) {
	tool := KubeEventsTool{Kube: fakeKube{events: []providers.KubeEvent{
		{Type: "Warning", Reason: "FailedScheduling", Object: "Pod/valkey-0", Count: 13,
			Message: "0/9 nodes are available: 4 Insufficient cpu, 2 Insufficient memory."},
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "FailedScheduling") || !strings.Contains(out, "Insufficient cpu") || !strings.Contains(out, "x13") {
		t.Fatalf("kube_events must surface reason + message + count, got:\n%s", out)
	}
}
