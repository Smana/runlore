// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
// activities list. descErr (when set) makes the describe call fail. lastIn
// captures the most-recent input so tests can assert request shapes.
type fakeASG struct {
	pages      []*autoscaling.DescribeAutoScalingGroupsOutput
	call       int
	descErr    error
	lastIn     *autoscaling.DescribeAutoScalingGroupsInput
	activities []asgtypes.Activity // returned by DescribeScalingActivities for every ASG
}

func (f *fakeASG) DescribeAutoScalingGroups(_ context.Context, in *autoscaling.DescribeAutoScalingGroupsInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	f.lastIn = in
	if f.descErr != nil {
		return nil, f.descErr
	}
	out := f.pages[f.call]
	f.call++
	return out, nil
}

func (f *fakeASG) DescribeScalingActivities(_ context.Context, _ *autoscaling.DescribeScalingActivitiesInput, _ ...func(*autoscaling.Options)) (*autoscaling.DescribeScalingActivitiesOutput, error) {
	return &autoscaling.DescribeScalingActivitiesOutput{Activities: f.activities}, nil
}

// fakeEC2 serves DescribeInstanceStatus and DescribeInstances.
// statusErr (when set) makes DescribeInstanceStatus fail.
// reservations (when set) is returned by DescribeInstances; lastDescribeIn
// captures the most-recent DescribeInstances input so tests can assert request
// shapes (filters etc.).
type fakeEC2 struct {
	statuses       []ec2types.InstanceStatus
	statusErr      error
	reservations   []ec2types.Reservation
	lastDescribeIn *ec2.DescribeInstancesInput
}

func (f *fakeEC2) DescribeInstanceStatus(_ context.Context, _ *ec2.DescribeInstanceStatusInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceStatusOutput, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return &ec2.DescribeInstanceStatusOutput{InstanceStatuses: f.statuses}, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	f.lastDescribeIn = in
	return &ec2.DescribeInstancesOutput{Reservations: f.reservations}, nil
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

// TestDescribeASGsClusterFilter asserts that describeASGs passes a server-side
// tag filter (tag:eks:cluster-name=<cluster>) when clusterName is set, so the
// 25-group cap counts only cluster-relevant ASGs rather than all ASGs in the
// account. When clusterName is empty no filters are sent.
func TestDescribeASGsClusterFilter(t *testing.T) {
	t.Run("cluster set — filter sent", func(t *testing.T) {
		asgF := &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}}
		c := &Client{asg: asgF, clusterName: "prod", maxEvents: 25}
		_, _, err := c.describeASGs(context.Background())
		if err != nil {
			t.Fatalf("describeASGs: %v", err)
		}
		if asgF.lastIn == nil {
			t.Fatal("lastIn not captured")
		}
		if len(asgF.lastIn.Filters) != 1 {
			t.Fatalf("expected 1 filter, got %d: %v", len(asgF.lastIn.Filters), asgF.lastIn.Filters)
		}
		f := asgF.lastIn.Filters[0]
		if deref(f.Name) != "tag:eks:cluster-name" {
			t.Fatalf("expected filter name tag:eks:cluster-name, got %q", deref(f.Name))
		}
		if len(f.Values) != 1 || f.Values[0] != "prod" {
			t.Fatalf("expected filter value [prod], got %v", f.Values)
		}
	})

	t.Run("no cluster — no filters sent", func(t *testing.T) {
		asgF := &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}}
		c := &Client{asg: asgF, clusterName: "", maxEvents: 25}
		_, _, err := c.describeASGs(context.Background())
		if err != nil {
			t.Fatalf("describeASGs: %v", err)
		}
		if asgF.lastIn == nil {
			t.Fatal("lastIn not captured")
		}
		if len(asgF.lastIn.Filters) != 0 {
			t.Fatalf("expected no filters when clusterName is empty, got %v", asgF.lastIn.Filters)
		}
	})
}

// makeKarpenterInstance builds a minimal ec2types.Instance with the
// karpenter.sh/nodepool tag set to pool, the given lifecycle and state,
// and an optional StateReason code (for simulating spot terminations).
func makeKarpenterInstance(id, pool string, lifecycle ec2types.InstanceLifecycleType, stateName ec2types.InstanceStateName, reasonCode string) ec2types.Instance {
	inst := ec2types.Instance{
		InstanceId:        ptr(id),
		InstanceType:      ec2types.InstanceTypeM5Large,
		InstanceLifecycle: lifecycle,
		State:             &ec2types.InstanceState{Name: stateName},
		Tags: []ec2types.Tag{
			{Key: ptr("karpenter.sh/nodepool"), Value: ptr(pool)},
		},
	}
	if reasonCode != "" {
		inst.StateReason = &ec2types.StateReason{Code: ptr(reasonCode)}
	}
	return inst
}

// TestResourceHealthKarpenterCapacity exercises the karpenterCapacity path:
//   - Instances tagged karpenter.sh/nodepool are enumerated and grouped by pool.
//   - Spot instances are flagged (spot=N).
//   - Terminated instances with spot interruption reason codes surface a
//     spot-term-reasons=[...] annotation so the model can diagnose "node vanished".
//   - When no Karpenter instances exist the section is silently omitted.
//   - When clusterName is set the DescribeInstances request includes a
//     tag:kubernetes.io/cluster/<name>=owned filter alongside the nodepool tag-key filter.
func TestResourceHealthKarpenterCapacity(t *testing.T) {
	tests := []struct {
		name         string
		reservations []ec2types.Reservation
		clusterName  string
		wantLines    []string
		wantAbsent   []string
	}{
		{
			name: "mixed spot + on-demand across two nodepools — grouped, spot flagged",
			reservations: []ec2types.Reservation{
				{Instances: []ec2types.Instance{
					makeKarpenterInstance("i-spot1", "default", ec2types.InstanceLifecycleTypeSpot, ec2types.InstanceStateNameRunning, ""),
					makeKarpenterInstance("i-spot2", "default", ec2types.InstanceLifecycleTypeSpot, ec2types.InstanceStateNameRunning, ""),
					makeKarpenterInstance("i-od1", "default", ec2types.InstanceLifecycleType(""), ec2types.InstanceStateNameRunning, ""),
				}},
				{Instances: []ec2types.Instance{
					makeKarpenterInstance("i-gpu1", "gpu-pool", ec2types.InstanceLifecycleTypeSpot, ec2types.InstanceStateNameRunning, ""),
				}},
			},
			wantLines: []string{
				"Karpenter nodepool default: instances=3 spot=2",
				"Karpenter nodepool gpu-pool: instances=1 spot=1",
			},
		},
		{
			name: "terminated spot with interruption reason — spot-term-reasons surfaced",
			reservations: []ec2types.Reservation{
				{Instances: []ec2types.Instance{
					makeKarpenterInstance("i-term1", "default", ec2types.InstanceLifecycleTypeSpot, ec2types.InstanceStateNameTerminated, "Server.SpotInstanceTermination"),
					makeKarpenterInstance("i-run1", "default", ec2types.InstanceLifecycleTypeSpot, ec2types.InstanceStateNameRunning, ""),
				}},
			},
			wantLines: []string{
				"Karpenter nodepool default: instances=2 spot=2 terminated=1 spot-term-reasons=[Server.SpotInstanceTermination]",
			},
		},
		{
			name:         "no Karpenter instances — section silently omitted",
			reservations: nil,
			wantAbsent:   []string{"Karpenter"},
		},
		{
			name:        "cluster set — DescribeInstances carries cluster ownership filter",
			clusterName: "prod",
			reservations: []ec2types.Reservation{
				{Instances: []ec2types.Instance{
					makeKarpenterInstance("i-a1", "default", ec2types.InstanceLifecycleTypeSpot, ec2types.InstanceStateNameRunning, ""),
				}},
			},
			wantLines: []string{"Karpenter nodepool default: instances=1 spot=1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ec2f := &fakeEC2{reservations: tc.reservations}
			c := &Client{
				eks:         &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
				asg:         &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("")}},
				ec2:         ec2f,
				clusterName: tc.clusterName,
				maxEvents:   25,
			}
			lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("ResourceHealth: %v", err)
			}
			for _, want := range tc.wantLines {
				if !linesContain(lines, want) {
					t.Errorf("want line containing %q, got %v", want, lines)
				}
			}
			for _, absent := range tc.wantAbsent {
				if linesContain(lines, absent) {
					t.Errorf("expected no line containing %q, got %v", absent, lines)
				}
			}
			// When clusterName is set, assert the cluster-ownership filter was sent.
			if tc.clusterName != "" && ec2f.lastDescribeIn != nil {
				clusterFilterFound := false
				expectedFilterName := "tag:kubernetes.io/cluster/" + tc.clusterName
				for _, f := range ec2f.lastDescribeIn.Filters {
					if deref(f.Name) == expectedFilterName {
						clusterFilterFound = true
						break
					}
				}
				if !clusterFilterFound {
					t.Errorf("expected DescribeInstances filter %q not found in %v", expectedFilterName, ec2f.lastDescribeIn.Filters)
				}
				// Also assert the nodepool tag-key filter is always present.
				nodepoolFilterFound := false
				for _, f := range ec2f.lastDescribeIn.Filters {
					if deref(f.Name) == "tag-key" && len(f.Values) == 1 && f.Values[0] == "karpenter.sh/nodepool" {
						nodepoolFilterFound = true
						break
					}
				}
				if !nodepoolFilterFound {
					t.Errorf("expected DescribeInstances tag-key=karpenter.sh/nodepool filter not found in %v", ec2f.lastDescribeIn.Filters)
				}
			}
		})
	}
}

// TestResourceHealthHonorsWindow asserts P3: a non-zero TimeWindow scopes the ASG
// scaling-activity lookback — activities that ended before w.Start are filtered out,
// while a zero window keeps today's behaviour (all activities shown).
func TestResourceHealthHonorsWindow(t *testing.T) {
	now := time.Now()
	recent := now.Add(-10 * time.Minute)
	stale := now.Add(-6 * time.Hour)
	acts := []asgtypes.Activity{
		{StatusCode: asgtypes.ScalingActivityStatusCodeSuccessful, Description: ptr("recent-scale-up"), StatusMessage: ptr("ok"), EndTime: &recent},
		{StatusCode: asgtypes.ScalingActivityStatusCodeFailed, Description: ptr("stale-scale-up"), StatusMessage: ptr("old"), EndTime: &stale},
	}
	newClient := func() *Client {
		return &Client{
			eks:         &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}},
			asg:         &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("", "demo-asg-a")}, activities: acts},
			clusterName: "demo",
			maxEvents:   25,
		}
	}

	t.Run("windowed -> stale activity filtered out", func(t *testing.T) {
		w := providers.TimeWindow{Start: now.Add(-60 * time.Minute), End: now}
		lines, err := newClient().ResourceHealth(context.Background(), providers.Selector{}, w)
		if err != nil {
			t.Fatalf("ResourceHealth: %v", err)
		}
		if !linesContain(lines, "recent-scale-up") {
			t.Fatalf("in-window activity must be shown, got %v", lines)
		}
		if linesContain(lines, "stale-scale-up") {
			t.Fatalf("stale (pre-window) activity must be filtered, got %v", lines)
		}
	})

	t.Run("zero window -> all activities shown (backward compatible)", func(t *testing.T) {
		lines, err := newClient().ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
		if err != nil {
			t.Fatalf("ResourceHealth: %v", err)
		}
		if !linesContain(lines, "recent-scale-up") || !linesContain(lines, "stale-scale-up") {
			t.Fatalf("zero window must show all activities, got %v", lines)
		}
	})

	t.Run("windowed with all stale -> no-activity note", func(t *testing.T) {
		w := providers.TimeWindow{Start: now.Add(-30 * time.Minute), End: now}
		onlyStale := &fakeASG{pages: []*autoscaling.DescribeAutoScalingGroupsOutput{asgPage("", "demo-asg-a")}, activities: []asgtypes.Activity{acts[1]}}
		c := &Client{eks: &fakeEKS{listPages: []*eks.ListNodegroupsOutput{ngPage("")}}, asg: onlyStale, clusterName: "demo", maxEvents: 25}
		lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, w)
		if err != nil {
			t.Fatalf("ResourceHealth: %v", err)
		}
		if !linesContain(lines, "no scaling activity in the last") {
			t.Fatalf("want a no-activity-in-window note, got %v", lines)
		}
	})
}
