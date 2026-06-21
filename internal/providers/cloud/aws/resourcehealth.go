package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/eks"

	"github.com/Smana/runlore/internal/providers"
)

const maxActivities = 5

// ResourceHealth returns cloud-side state/health for resources backing the
// selector: EKS nodegroup status (cluster-scoped), ASG recent scaling activities,
// and — when the selector names an EC2 instance (i-…) — its instance status.
// Best-effort: a failing sub-query contributes an error line, not a hard failure,
// so partial cloud visibility still helps.
func (c *Client) ResourceHealth(ctx context.Context, sel providers.Selector, _ providers.TimeWindow) (providers.LogResult, error) {
	var lines providers.LogResult
	add := func(format string, a ...any) {
		lines = append(lines, providers.LogLine{Message: fmt.Sprintf(format, a...)})
	}

	// EKS nodegroups for the cluster.
	if c.clusterName != "" {
		if ng, err := c.eks.ListNodegroups(ctx, &eks.ListNodegroupsInput{ClusterName: ptr(c.clusterName)}); err != nil {
			add("eks: list nodegroups failed: %v", err)
		} else {
			for _, name := range ng.Nodegroups {
				d, err := c.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: ptr(c.clusterName), NodegroupName: ptr(name)})
				if err != nil || d.Nodegroup == nil {
					add("eks nodegroup %s: describe failed", name)
					continue
				}
				add("EKS nodegroup %s: status=%s%s", name, string(d.Nodegroup.Status), nodegroupHealth(d))
			}
		}
	}

	// ASG scaling activities (capacity/launch failures show up here).
	if asgs, err := c.asg.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{}); err != nil {
		add("asg: describe failed: %v", err)
	} else {
		for _, g := range asgs.AutoScalingGroups {
			name := deref(g.AutoScalingGroupName)
			if c.clusterName != "" && !asgInCluster(g.AutoScalingGroupName, c.clusterName) {
				// Best-effort cluster scoping by name substring; keep it if it matches.
				continue
			}
			add("ASG %s: desired=%d instances=%d", name, derefInt32(g.DesiredCapacity), len(g.Instances))
			if act, err := c.asg.DescribeScalingActivities(ctx, &autoscaling.DescribeScalingActivitiesInput{AutoScalingGroupName: g.AutoScalingGroupName, MaxRecords: ptr(int32(maxActivities))}); err == nil {
				for _, a := range act.Activities {
					add("  activity: %s %s — %s", string(a.StatusCode), deref(a.Description), deref(a.StatusMessage))
				}
			}
		}
	}

	// EC2 instance status when an instance id is selected.
	if strings.HasPrefix(sel.Name, "i-") {
		if s, err := c.ec2.DescribeInstanceStatus(ctx, &ec2.DescribeInstanceStatusInput{InstanceIds: []string{sel.Name}, IncludeAllInstances: ptr(true)}); err != nil {
			add("ec2 %s: status query failed: %v", sel.Name, err)
		} else {
			for _, st := range s.InstanceStatuses {
				add("EC2 %s: state=%s system=%s instance=%s", deref(st.InstanceId),
					instanceState(st.InstanceState), summaryStatus(st.SystemStatus), summaryStatus(st.InstanceStatus))
			}
		}
	}
	return lines, nil
}

func nodegroupHealth(d *eks.DescribeNodegroupOutput) string {
	if d.Nodegroup.Health == nil || len(d.Nodegroup.Health.Issues) == 0 {
		return ""
	}
	var parts []string
	for _, iss := range d.Nodegroup.Health.Issues {
		parts = append(parts, string(iss.Code)+": "+deref(iss.Message))
	}
	return " health=[" + strings.Join(parts, "; ") + "]"
}

func asgInCluster(name *string, cluster string) bool {
	return name != nil && strings.Contains(*name, cluster)
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func instanceState(s *ec2types.InstanceState) string {
	if s == nil {
		return ""
	}
	return string(s.Name)
}

func summaryStatus(s *ec2types.InstanceStatusSummary) string {
	if s == nil {
		return ""
	}
	return string(s.Status)
}
