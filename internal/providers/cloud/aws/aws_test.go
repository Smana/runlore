package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/Smana/runlore/internal/providers"
)

type fakeCT struct {
	// pages are returned in order; a page's NextToken (set by the test) drives the
	// paginator to the next page. in/tokens record what the paginator sent.
	pages  []*cloudtrail.LookupEventsOutput
	call   int
	in     *cloudtrail.LookupEventsInput
	tokens []string // NextToken seen on each call ("" for the first)
}

func (f *fakeCT) LookupEvents(_ context.Context, in *cloudtrail.LookupEventsInput, _ ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error) {
	f.in = in
	tok := ""
	if in.NextToken != nil {
		tok = *in.NextToken
	}
	f.tokens = append(f.tokens, tok)
	out := f.pages[f.call]
	f.call++
	return out, nil
}

// ctEvent is a small helper to build a CloudTrail event at time t.
func ctEvent(id, name string, t time.Time) cttypes.Event {
	return cttypes.Event{
		EventId: ptr(id), EventName: ptr(name),
		EventSource: ptr("autoscaling.amazonaws.com"), Username: ptr("karpenter"), EventTime: ptr(t),
		Resources: []cttypes.Resource{{ResourceType: ptr("AWS::EC2::Instance"), ResourceName: ptr("i-" + id)}},
	}
}

// countTruncated reports how many sentinel (truncation) changes are present.
func countTruncated(changes []providers.Change) int {
	n := 0
	for _, c := range changes {
		if c.Workload.Kind == "(truncated)" {
			n++
		}
	}
	return n
}

func TestCloudChangesPagination(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 12, 0, 0, time.UTC)

	tests := []struct {
		name          string
		pages         []*cloudtrail.LookupEventsOutput
		maxEvents     int
		wantReal      int  // real (non-sentinel) changes
		wantTruncated bool // a sentinel appended
	}{
		{
			name: "under cap across two pages — all returned, no sentinel",
			pages: []*cloudtrail.LookupEventsOutput{
				{Events: []cttypes.Event{ctEvent("1", "A", t0), ctEvent("2", "B", t0.Add(-time.Minute))}, NextToken: ptr("page2")},
				{Events: []cttypes.Event{ctEvent("3", "C", t0.Add(-2*time.Minute))}},
			},
			maxEvents:     25,
			wantReal:      3,
			wantTruncated: false,
		},
		{
			name: "over cap — capped + sentinel",
			pages: []*cloudtrail.LookupEventsOutput{
				{Events: []cttypes.Event{ctEvent("1", "A", t0), ctEvent("2", "B", t0.Add(-time.Minute))}, NextToken: ptr("page2")},
				{Events: []cttypes.Event{ctEvent("3", "C", t0.Add(-2*time.Minute)), ctEvent("4", "D", t0.Add(-3*time.Minute))}},
			},
			maxEvents:     2,
			wantReal:      2,
			wantTruncated: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ct := &fakeCT{pages: tc.pages}
			c := &Client{ct: ct, maxEvents: tc.maxEvents}

			changes, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("CloudChanges: %v", err)
			}
			truncated := countTruncated(changes)
			realCount := len(changes) - truncated
			if realCount != tc.wantReal {
				t.Fatalf("real changes = %d, want %d (total %d)", realCount, tc.wantReal, len(changes))
			}
			if tc.wantTruncated {
				if truncated != 1 {
					t.Fatalf("want exactly 1 sentinel, got %d", truncated)
				}
				last := changes[len(changes)-1]
				if last.Workload.Kind != "(truncated)" {
					t.Fatalf("sentinel must be last; last = %+v", last)
				}
				if !strings.Contains(last.Workload.Name, "truncated at 2") {
					t.Fatalf("sentinel message = %q, want it to mention the cap", last.Workload.Name)
				}
				// The paginator must have followed the first page's NextToken.
				if len(ct.tokens) < 2 || ct.tokens[1] != "page2" {
					t.Fatalf("expected second call to carry NextToken=page2, tokens=%v", ct.tokens)
				}
			} else if truncated != 0 {
				t.Fatalf("did not expect a sentinel, got %d", truncated)
			}
		})
	}
}

func TestCloudChanges(t *testing.T) {
	t0 := time.Date(2026, 6, 21, 8, 12, 0, 0, time.UTC)
	ct := &fakeCT{pages: []*cloudtrail.LookupEventsOutput{{Events: []cttypes.Event{
		{
			EventId: ptr("evt-1"), EventName: ptr("TerminateInstanceInAutoScalingGroup"),
			EventSource: ptr("autoscaling.amazonaws.com"), Username: ptr("karpenter"), EventTime: ptr(t0),
			Resources: []cttypes.Resource{{ResourceType: ptr("AWS::EC2::Instance"), ResourceName: ptr("i-0abc")}},
		},
	}}}}
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
