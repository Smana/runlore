package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	asgtypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
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

	// EKS nodegroups for the cluster. Paginate (a ListNodegroups page is ≤100) and
	// cap at c.maxEvents so a large cluster's later pages are not silently dropped;
	// a binding cap surfaces a truncation line so the model knows the view is partial.
	if c.clusterName != "" {
		names, more, err := c.listNodegroups(ctx)
		if err != nil {
			add("eks: list nodegroups failed: %v", err)
		} else {
			for _, name := range names {
				d, err := c.eks.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: ptr(c.clusterName), NodegroupName: ptr(name)})
				if err != nil || d.Nodegroup == nil {
					add("eks nodegroup %s: describe failed", name)
					continue
				}
				add("EKS nodegroup %s: status=%s%s", name, string(d.Nodegroup.Status), nodegroupHealth(d))
			}
			if more {
				add("… EKS nodegroups truncated at %d (more exist)", c.maxEvents)
			}
		}
	}

	// ASG scaling activities (capacity/launch failures show up here). Paginate and
	// cap the number of ASGs *examined* at c.maxEvents; a binding cap surfaces a
	// truncation line (the cap counts ASGs scanned, not just those matching the
	// cluster filter — the honest statement is "stopped scanning at N").
	asgs, more, err := c.describeASGs(ctx)
	if err != nil {
		add("asg: describe failed: %v", err)
	} else {
		for i := range asgs {
			g := asgs[i]
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
		if more {
			add("… ASGs truncated at %d (more exist)", c.maxEvents)
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

// listNodegroups returns up to c.maxEvents nodegroup names for the cluster,
// paging via the SDK paginator. more is true when the cap was reached with
// further pages still available (the truncation signal).
func (c *Client) listNodegroups(ctx context.Context) (names []string, more bool, err error) {
	p := eks.NewListNodegroupsPaginator(c.eks, &eks.ListNodegroupsInput{ClusterName: ptr(c.clusterName)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("list nodegroups: %w", err)
		}
		names = append(names, out.Nodegroups...)
		if len(names) >= c.maxEvents {
			names = names[:c.maxEvents]
			more = p.HasMorePages()
			break
		}
	}
	return names, more, nil
}

// describeASGs returns up to c.maxEvents Auto Scaling Groups, paging via the SDK
// paginator. more is true when the cap was reached with further pages available.
// The cap counts ASGs examined (before the cluster filter), matching the
// truncation line's "stopped scanning at N" meaning.
func (c *Client) describeASGs(ctx context.Context) (groups []asgtypes.AutoScalingGroup, more bool, err error) {
	p := autoscaling.NewDescribeAutoScalingGroupsPaginator(c.asg, &autoscaling.DescribeAutoScalingGroupsInput{})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("describe auto scaling groups: %w", err)
		}
		groups = append(groups, out.AutoScalingGroups...)
		if len(groups) >= c.maxEvents {
			groups = groups[:c.maxEvents]
			more = p.HasMorePages()
			break
		}
	}
	return groups, more, nil
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
