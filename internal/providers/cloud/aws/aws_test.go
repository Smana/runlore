package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/Smana/runlore/internal/providers"
)

type fakeCT struct {
	out *cloudtrail.LookupEventsOutput
	in  *cloudtrail.LookupEventsInput
}

func (f *fakeCT) LookupEvents(_ context.Context, in *cloudtrail.LookupEventsInput, _ ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error) {
	f.in = in
	return f.out, nil
}

func TestCloudChanges(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 12, 0, 0, time.UTC)
	ct := &fakeCT{out: &cloudtrail.LookupEventsOutput{Events: []cttypes.Event{
		{
			EventId: ptr("evt-1"), EventName: ptr("TerminateInstanceInAutoScalingGroup"),
			EventSource: ptr("autoscaling.amazonaws.com"), Username: ptr("karpenter"), EventTime: ptr(t0),
			Resources: []cttypes.Resource{{ResourceType: ptr("AWS::EC2::Instance"), ResourceName: ptr("i-0abc")}},
		},
	}}}
	c := &Client{ct: ct, maxEvents: 25}

	changes, err := c.CloudChanges(context.Background(), providers.Selector{Name: "i-0abc"}, providers.TimeWindow{Start: t0.Add(-time.Hour), End: t0})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want 1 change, got %d", len(changes))
	}
	ch := changes[0]
	if ch.Engine != providers.EngineAWS || ch.Type != providers.ChangeCloudAPI {
		t.Fatalf("unexpected engine/type: %+v", ch)
	}
	if ch.Workload.Kind != "AWS::EC2::Instance" || ch.Workload.Name != "i-0abc" {
		t.Fatalf("unexpected workload: %+v", ch.Workload)
	}
	if ch.ManagedBy != "autoscaling.amazonaws.com" || ch.ToRev != "evt-1" {
		t.Fatalf("unexpected source attribution: %+v", ch)
	}
	if ch.Source.Path != "TerminateInstanceInAutoScalingGroup by karpenter" {
		t.Fatalf("unexpected rendered event: %q", ch.Source.Path)
	}
	// Mutating-only filter + resource scope must be passed to the API.
	if len(ct.in.LookupAttributes) != 2 {
		t.Fatalf("expected ReadOnly=false + ResourceName attributes, got %+v", ct.in.LookupAttributes)
	}
}
