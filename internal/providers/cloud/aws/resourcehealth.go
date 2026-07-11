// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

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
func (c *Client) ResourceHealth(ctx context.Context, sel providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
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
				shown := 0
				for _, a := range act.Activities {
					// Honor the incident window (P3): a scaling activity that ended before
					// w.Start is stale context — scope the lookback to the window so
					// cloud_resource_health is consistent with the other windowed
					// providers. A zero/unset window keeps today's behaviour (show all).
					if activityBeforeWindow(a, w.Start) {
						continue
					}
					add("  activity: %s %s — %s", string(a.StatusCode), deref(a.Description), deref(a.StatusMessage))
					shown++
				}
				if shown == 0 && !w.Start.IsZero() && len(act.Activities) > 0 {
					add("  (no scaling activity in the last %s)", windowAge(w))
				}
			}
		}
		if more {
			add("… ASGs truncated at %d (more exist)", c.maxEvents)
		}
	}

	// Karpenter-managed EC2 capacity (instances tagged karpenter.sh/nodepool).
	// These are standalone instances not tracked by EKS managed nodegroups or
	// explicit ASGs, so they are invisible to the sections above. Spot signal
	// (interruption / rebalance reason codes) is surfaced here so the model can
	// answer "why did my Karpenter node vanish?"
	if err := c.karpenterCapacity(ctx, add); err != nil {
		add("karpenter: capacity query failed: %v", err)
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
// When clusterName is set the request carries a server-side tag filter for the
// EKS-managed tag (eks:cluster-name=<cluster>) so the cap counts only
// cluster-relevant groups and large shared accounts with many unrelated ASGs
// do not exhaust the cap before cluster groups are seen. The caller still
// applies the asgInCluster substring check as a fallback for self-managed groups
// that carry the cluster name in their ASG name but not that tag.
func (c *Client) describeASGs(ctx context.Context) (groups []asgtypes.AutoScalingGroup, more bool, err error) {
	in := &autoscaling.DescribeAutoScalingGroupsInput{}
	if c.clusterName != "" {
		// "tag:<key>" filter: matches ASGs that have the tag with the given value.
		// EKS-managed nodegroup ASGs always carry this tag; it is the canonical
		// server-side scope. Self-managed groups lacking the tag survive via the
		// asgInCluster substring fallback applied by the caller.
		in.Filters = []asgtypes.Filter{{
			Name:   ptr("tag:eks:cluster-name"),
			Values: []string{c.clusterName},
		}}
	}
	p := autoscaling.NewDescribeAutoScalingGroupsPaginator(c.asg, in)
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

// karpenterCapacity enumerates EC2 instances tagged with karpenter.sh/nodepool
// (the canonical Karpenter-managed instance tag). Results are grouped by nodepool
// and rendered as compact summary lines; spot instances are flagged and, when the
// instance has terminated, any spot-interruption StateReason code is noted so the
// model can identify "node vanished due to spot reclaim" root causes.
//
// Filter used: tag-key=karpenter.sh/nodepool
// When clusterName is set a second filter is ANDed: tag:kubernetes.io/cluster/<name>=owned
// so multi-cluster accounts scope naturally.
func (c *Client) karpenterCapacity(ctx context.Context, add func(string, ...any)) error {
	if c.ec2 == nil {
		// ec2 client not wired (unit tests that focus on nodegroup/ASG paths
		// leave it nil); skip gracefully.
		return nil
	}
	filters := []ec2types.Filter{
		{Name: ptr("tag-key"), Values: []string{"karpenter.sh/nodepool"}},
	}
	if c.clusterName != "" {
		// Karpenter tags every node it provisions with this cluster ownership tag.
		filters = append(filters, ec2types.Filter{
			Name:   ptr("tag:kubernetes.io/cluster/" + c.clusterName),
			Values: []string{"owned"},
		})
	}

	out, err := c.ec2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: filters})
	if err != nil {
		return err
	}

	// Group by nodepool tag value. Track counts and accumulate spot/termination
	// signal per nodepool.
	type nodepoolStats struct {
		total           int
		spotCount       int
		terminated      int
		spotTermReasons []string
	}
	stats := map[string]*nodepoolStats{}

	for _, r := range out.Reservations {
		for _, inst := range r.Instances {
			pool := tagValue(inst.Tags, "karpenter.sh/nodepool")
			if pool == "" {
				pool = "(unknown-nodepool)"
			}
			s := stats[pool]
			if s == nil {
				s = &nodepoolStats{}
				stats[pool] = s
			}
			s.total++

			if inst.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
				s.spotCount++
			}

			// Terminated instances with spot interruption reason codes are the
			// primary "why did my node vanish" signal for Karpenter spot capacity.
			if inst.State != nil && inst.State.Name == ec2types.InstanceStateNameTerminated {
				s.terminated++
				if inst.StateReason != nil {
					code := deref(inst.StateReason.Code)
					if strings.Contains(code, "Spot") {
						s.spotTermReasons = append(s.spotTermReasons, code)
					}
				}
			}
		}
	}

	if len(stats) == 0 {
		// No Karpenter-managed instances found — omit the section entirely so
		// clusters not using Karpenter don't get noise.
		return nil
	}

	for pool, s := range stats {
		spotNote := ""
		if s.spotCount > 0 {
			spotNote = fmt.Sprintf(" spot=%d", s.spotCount)
		}
		termNote := ""
		if s.terminated > 0 {
			termNote = fmt.Sprintf(" terminated=%d", s.terminated)
		}
		reasonNote := ""
		if len(s.spotTermReasons) > 0 {
			reasonNote = " spot-term-reasons=[" + strings.Join(dedupStrings(s.spotTermReasons), ", ") + "]"
		}
		add("Karpenter nodepool %s: instances=%d%s%s%s", pool, s.total, spotNote, termNote, reasonNote)
	}
	return nil
}

// tagValue returns the value of the first tag matching key, or "" if absent.
func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if deref(t.Key) == key {
			return deref(t.Value)
		}
	}
	return ""
}

// dedupStrings returns a slice with duplicate strings removed (order preserved).
func dedupStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// activityBeforeWindow reports whether an ASG scaling activity ended before the
// window start, so it can be filtered out as stale. An activity is dated by its
// EndTime (when it finished); a still-running activity (nil EndTime) falls back to
// its StartTime and is kept when either is missing (no timestamp ⇒ can't judge it
// stale, so show it). A zero start means "no window" ⇒ never filtered.
func activityBeforeWindow(a asgtypes.Activity, start time.Time) bool {
	if start.IsZero() {
		return false
	}
	when := a.EndTime
	if when == nil {
		when = a.StartTime
	}
	if when == nil {
		return false // undated — keep it rather than hide a possibly-relevant activity
	}
	return when.Before(start)
}

// windowAge renders the window's span as a short human duration for the
// "no activity in the last N" note (best-effort; empty when the window is open-ended).
func windowAge(w providers.TimeWindow) string {
	if w.Start.IsZero() {
		return "window"
	}
	end := w.End
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(w.Start).Round(time.Minute).String()
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
