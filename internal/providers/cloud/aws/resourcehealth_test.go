package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/Smana/runlore/internal/providers"
)

// fakeEKS serves ListNodegroups across pages (driven by NextToken) and a fixed
// DescribeNodegroup for any name.
type fakeEKS struct {
	listPages []*eks.ListNodegroupsOutput
	listCall  int
}

func (f *fakeEKS) ListNodegroups(_ context.Context, _ *eks.ListNodegroupsInput, _ ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	out := f.listPages[f.listCall]
	f.listCall++
	return out, nil
}

func (f *fakeEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	return &eks.DescribeNodegroupOutput{Nodegroup: &ekstypes.Nodegroup{
		NodegroupName: in.NodegroupName,
		Status:        ekstypes.NodegroupStatusActive,
	}}, nil
}

// fakeASG serves DescribeAutoScalingGroups across pages and an empty scaling
// activities list.
type fakeASG struct {
	pages []*autoscaling.DescribeAutoScalingGroupsOutput
	call  int
}

func (f *fakeASG) DescribeAutoScalingGroups(_ context.Context, _ *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	out := f.pages[f.call]
	f.call++
	return out, nil
}

func (f *fakeASG) DescribeScalingActivities(_ context.Context, _ *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	return &autoscaling.DescribeScalingActivitiesOutput{}, nil
}

func asgPage(token string, names ...string) *autoscaling.DescribeAutoScalingGroupsOutput {
	groups := make([]asgtypes.AutoScalingGroup, 0, len(names))
	for _, n := range names {
		name := n
		groups = append(groups, asgtypes.AutoScalingGroup{AutoScalingGroupName: &name})
	}
	out := &autoscaling.DescribeAutoScalingGroupsOutput{AutoScalingGroups: groups}
	if token != "" {
		out.NextToken = ptr(token)
	}
	return out
}

func ngPage(token string, names ...string) *eks.ListNodegroupsOutput {
	out := &eks.ListNodegroupsOutput{Nodegroups: names}
	if token != "" {
		out.NextToken = ptr(token)
	}
	return out
}

func linesContain(lines providers.LogResult, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l.Message, sub) {
			return true
		}
	}
	return false
}

func countLines(lines providers.LogResult, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l.Message, sub) {
			n++
		}
	}
	return n
}

func TestResourceHealthNodegroupPagination(t *testing.T) {
	tests := []struct {
		name          string
		pages         []*eks.ListNodegroupsOutput
		maxEvents     int
		wantDescribed int
		wantTruncated bool
	}{
		{
			name:          "under cap across two pages — all described, no truncation line",
			pages:         []*eks.ListNodegroupsOutput{ngPage("p2", "ng-a", "ng-b"), ngPage("", "ng-c")},
			maxEvents:     25,
			wantDescribed: 3,
			wantTruncated: false,
		},
		{
			name:          "over cap — capped + truncation line",
			pages:         []*eks.ListNodegroupsOutput{ngPage("p2", "ng-a", "ng-b"), ngPage("", "ng-c", "ng-d")},
			maxEvents:     2,
			wantDescribed: 2,
			wantTruncated: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				eks:         &fakeEKS{listPages: tc.pages},
				asg:         &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
				clusterName: "demo",
				maxEvents:   tc.maxEvents,
			}
			lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("ResourceHealth: %v", err)
			}
			if got := countLines(lines, "EKS nodegroup "); got != tc.wantDescribed {
				t.Fatalf("described nodegroups = %d, want %d (lines: %v)", got, tc.wantDescribed, lines)
			}
			if got := linesContain(lines, "EKS nodegroups truncated at"); got != tc.wantTruncated {
				t.Fatalf("truncation line present = %v, want %v", got, tc.wantTruncated)
			}
		})
	}
}

func TestResourceHealthASGPagination(t *testing.T) {
	tests := []struct {
		name          string
		pages         []*autoscaling.DescribeAutoScalingGroupsOutput
		maxEvents     int
		wantRendered  int
		wantTruncated bool
	}{
		{
			name:          "under cap across two pages — all rendered, no truncation line",
			pages:         []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("p2", "demo-asg-a", "demo-asg-b"), asgPage("", "demo-asg-c")},
			maxEvents:     25,
			wantRendered:  3,
			wantTruncated: false,
		},
		{
			name:          "over cap — capped + truncation line",
			pages:         []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("p2", "demo-asg-a", "demo-asg-b"), asgPage("", "demo-asg-c", "demo-asg-d")},
			maxEvents:     2,
			wantRendered:  2,
			wantTruncated: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				eks:         &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
				asg:         &fakeASG{pages: tc.pages},
				clusterName: "demo",
				maxEvents:   tc.maxEvents,
			}
			lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("ResourceHealth: %v", err)
			}
			if got := countLines(lines, "ASG demo-asg-"); got != tc.wantRendered {
				t.Fatalf("rendered ASGs = %d, want %d (lines: %v)", got, tc.wantRendered, lines)
			}
			if got := linesContain(lines, "ASGs truncated at"); got != tc.wantTruncated {
				t.Fatalf("truncation line present = %v, want %v", got, tc.wantTruncated)
			}
		})
	}
}
