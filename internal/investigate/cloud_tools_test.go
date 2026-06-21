package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

type fakeCloud struct {
	changes []providers.Change
	health  providers.LogResult
}

func (f fakeCloud) CloudChanges(context.Context, providers.Selector, providers.TimeWindow) ([]providers.Change, error) {
	return f.changes, nil
}
func (f fakeCloud) ResourceHealth(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return f.health, nil
}

func TestCloudWhatChangedTool(t *testing.T) {
	tool := CloudWhatChangedTool{Cloud: fakeCloud{changes: []providers.Change{{
		When: time.Unix(1700000000, 0).UTC(), ManagedBy: "autoscaling.amazonaws.com",
		Workload: providers.Workload{Kind: "AWS::EC2::Instance", Name: "i-0abc"},
		Source:   providers.SourceRef{Path: "TerminateInstanceInAutoScalingGroup by karpenter"},
	}}}}
	out, err := tool.Call(context.Background(), `{"resource":"i-0abc"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"autoscaling.amazonaws.com", "i-0abc", "TerminateInstanceInAutoScalingGroup by karpenter"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCloudResourceHealthTool(t *testing.T) {
	tool := CloudResourceHealthTool{Cloud: fakeCloud{health: providers.LogResult{
		{Message: "EKS nodegroup default: status=DEGRADED health=[AsgInstanceLaunchFailures: ...]"},
	}}}
	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Fatalf("unexpected output: %s", out)
	}
	// Empty result → friendly message.
	empty, _ := CloudResourceHealthTool{Cloud: fakeCloud{}}.Call(context.Background(), `{}`)
	if !strings.Contains(empty, "no AWS resource health") {
		t.Fatalf("expected empty message, got %s", empty)
	}
}
