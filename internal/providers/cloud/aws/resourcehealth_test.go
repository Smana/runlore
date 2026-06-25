package aws

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/Smana/runlore/internal/providers"
)

// fakeEKS serves ListNodegroups across pages (driven by NextToken) and a fixed
// DescribeNodegroup for any name. descErr (when set) makes every DescribeNodegroup
// fail; health (when set) is attached to the described nodegroup so the
// health-issue rendering path can be exercised.
type fakeEKS struct {
	listPages []*eks.ListNodegroupsOutput
	listCall  int
	descErr   error
	health    *ekstypes.NodegroupHealth
}

func (f *fakeEKS) ListNodegroups(_ context.Context, _ *eks.ListNodegroupsInput, _ ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	out := f.listPages[f.listCall]
	f.listCall++
	return out, nil
}

func (f *fakeEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	if f.descErr != nil {
		return nil, f.descErr
	}
	return &eks.DescribeNodegroupOutput{Nodegroup: &ekstypes.Nodegroup{
		NodegroupName: in.NodegroupName,
		Status:        ekstypes.NodegroupStatusActive,
		Health:        f.health,
	}}, nil
}

// fakeASG serves DescribeAutoScalingGroups across pages and an empty scaling
// activities list. descErr (when set) makes the describe call fail.
type fakeASG struct {
	pages   []*autoscaling.DescribeAutoScalingGroupsOutput
	call    int
	descErr error
}

func (f *fakeASG) DescribeAutoScalingGroups(_ context.Context, _ *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	if f.descErr != nil {
		return nil, f.descErr
	}
	out := f.pages[f.call]
	f.call++
	return out, nil
}

func (f *fakeASG) DescribeScalingActivities(_ context.Context, _ *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	return &autoscaling.DescribeScalingActivitiesOutput{}, nil
}

// fakeEC2 serves DescribeInstanceStatus (the only EC2 call ResourceHealth makes).
// statusErr (when set) makes the status query fail. DescribeInstances is part of
// the ec2API surface but unused here.
type fakeEC2 struct {
	statuses  []ec2types.InstanceStatus
	statusErr error
}

func (f *fakeEC2) DescribeInstanceStatus(_ context.Context, _ *ec2.DescribeInstanceStatusInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceStatusOutput, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: f.statuses}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{}, nil
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

// TestResourceHealthEC2InstanceStatus exercises the i-… selector branch: an
// instance id triggers DescribeInstanceStatus and renders state/system/instance,
// covering instanceState + summaryStatus (incl. the nil arms when a status summary
// is absent).
func TestResourceHealthEC2InstanceStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   ec2types.InstanceStatus
		wantLine string
	}{
		{
			name: "full status — state + both summaries rendered",
			status: ec2types.InstanceStatus{
				InstanceId:     ptr("i-0abc"),
				InstanceState:  &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
				SystemStatus:   &ec2types.InstanceStatusSummary{Status: ec2types.SummaryStatusImpaired},
				InstanceStatus: &ec2types.InstanceStatusSummary{Status: ec2types.SummaryStatusOk},
			},
			wantLine: "EC2 i-0abc: state=running system=impaired instance=ok",
		},
		{
			name:     "nil state and summaries — empty fields, no panic",
			status:   ec2types.InstanceStatus{InstanceId: ptr("i-0def")},
			wantLine: "EC2 i-0def: state= system= instance=",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{
				eks:       &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
				asg:       &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
				ec2:       &fakeEC2{statuses: []ec2types.InstanceStatus{tc.status}},
				maxEvents: 25,
			}
			lines, err := c.ResourceHealth(context.Background(), providers.Selector{Name: "i-0abc"}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("ResourceHealth: %v", err)
			}
			if !linesContain(lines, tc.wantLine) {
				t.Fatalf("want a line %q, got %v", tc.wantLine, lines)
			}
		})
	}
}

// TestResourceHealthEC2NonInstanceSelector asserts the EC2 status query is skipped
// when the selector is not an instance id (no i- prefix) — the fakeEC2 would
// otherwise have to serve it.
func TestResourceHealthEC2NonInstanceSelector(t *testing.T) {
	ec2f := &fakeEC2{statusErr: errors.New("must not be called")}
	c := &Client{
		eks:       &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
		asg:       &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
		ec2:       ec2f,
		maxEvents: 25,
	}
	lines, err := c.ResourceHealth(context.Background(), providers.Selector{Name: "harbor-core"}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	if linesContain(lines, "EC2 ") || linesContain(lines, "status query failed") {
		t.Fatalf("non-instance selector must skip the EC2 status query, got %v", lines)
	}
}

// TestResourceHealthErrorLines covers best-effort degradation: each sub-query
// failing contributes an error line, but ResourceHealth still returns nil error so
// partial cloud visibility survives a single failing call.
func TestResourceHealthErrorLines(t *testing.T) {
	boom := errors.New("AccessDenied")

	t.Run("nodegroup describe failure", func(t *testing.T) {
		c := &Client{
			eks:         &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("", "ng-a")}, descErr: boom},
			asg:         &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
			ec2:         &fakeEC2{},
			clusterName: "demo",
			maxEvents:   25,
		}
		lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
		if err != nil {
			t.Fatalf("ResourceHealth must not hard-fail on a describe error: %v", err)
		}
		if !linesContain(lines, "eks nodegroup ng-a: describe failed") {
			t.Fatalf("want a nodegroup describe-failed line, got %v", lines)
		}
	})

	t.Run("asg describe failure", func(t *testing.T) {
		c := &Client{
			eks:       &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
			asg:       &fakeASG{descErr: boom},
			ec2:       &fakeEC2{},
			maxEvents: 25,
		}
		lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
		if err != nil {
			t.Fatalf("ResourceHealth must not hard-fail on an ASG describe error: %v", err)
		}
		if !linesContain(lines, "asg: describe failed") {
			t.Fatalf("want an asg describe-failed line, got %v", lines)
		}
	})

	t.Run("ec2 instance-status query failure", func(t *testing.T) {
		c := &Client{
			eks:       &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
			asg:       &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
			ec2:       &fakeEC2{statusErr: boom},
			maxEvents: 25,
		}
		lines, err := c.ResourceHealth(context.Background(), providers.Selector{Name: "i-0abc"}, providers.TimeWindow{})
		if err != nil {
			t.Fatalf("ResourceHealth must not hard-fail on an EC2 status error: %v", err)
		}
		if !linesContain(lines, "ec2 i-0abc: status query failed") {
			t.Fatalf("want an ec2 status-query-failed line, got %v", lines)
		}
	})
}

// TestResourceHealthNodegroupHealthIssues asserts a nodegroup carrying health
// issues renders them in a health=[…] suffix (the nodegroupHealth path).
func TestResourceHealthNodegroupHealthIssues(t *testing.T) {
	health := &ekstypes.NodegroupHealth{Issues: []ekstypes.Issue{
		{Code: ekstypes.NodegroupIssueCodeAsgInstanceLaunchFailures, Message: ptr("insufficient capacity")},
	}}
	c := &Client{
		eks:         &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("", "ng-a")}, health: health},
		asg:         &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
		ec2:         &fakeEC2{},
		clusterName: "demo",
		maxEvents:   25,
	}
	lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	if !linesContain(lines, "health=[AsgInstanceLaunchFailures: insufficient capacity]") {
		t.Fatalf("want the nodegroup health issue rendered, got %v", lines)
	}
}
