// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	container "google.golang.org/api/container/v1"
	"google.golang.org/api/googleapi"

	"github.com/Smana/runlore/internal/providers"
)

// ResourceHealth returns cloud-side state for the resources backing the selector: GKE
// cluster and node-pool status, managed-instance-group errors, and — when the selector
// names one — a Compute Engine instance's status.
//
// Best-effort, matching the AWS contract (cloud/aws/resourcehealth.go:25): a failing
// sub-query contributes an error line rather than failing the call. The cluster
// sub-query needs roles/container.clusterViewer and the two node-level ones need
// roles/compute.viewer, so a deployment granted one binding and not the other is an
// ordinary state — and failing the whole call would throw away the half that answered.
//
// The result is never empty. Every path through the cluster sub-query emits a line —
// the cluster's state, a permission or scope diagnosis, or the config keys that are
// unset — because cloud_resource_health renders zero lines as "no GCP resource health
// returned", which a model reads as a positive claim that the cloud was queried and
// found quiet.
func (c *Client) ResourceHealth(ctx context.Context, sel providers.Selector, w providers.TimeWindow) (providers.LogResult, error) {
	var lines providers.LogResult
	add := func(format string, a ...any) {
		lines = append(lines, providers.LogLine{Message: fmt.Sprintf(format, a...)})
	}

	migs, autopilot := c.describeCluster(ctx, add)

	// On Autopilot the node layer is Google's, and the node-level sub-queries below
	// answer questions the operator cannot act on. Skipping them silently would be the
	// worse failure: an empty node-capacity section is indistinguishable from a healthy
	// one, so a model investigating pending Pods would conclude capacity was fine on
	// the evidence of a query that never ran. The skip is therefore stated.
	//
	// The skip is enforced here, at the call site, rather than inside each sub-query:
	// one guard that is visible in the shape of the function cannot be half-honoured by
	// a later change to either sub-query.
	if autopilot {
		add("NOTE: GKE Autopilot cluster — the node layer is Google-managed, so the " +
			"instance-group error and instance-status lookups were SKIPPED. Their absence is " +
			"not evidence that node capacity is healthy; it means this lens did not look.")
		return lines, nil
	}

	c.describeMIGs(ctx, migs, w, add)
	c.describeInstance(ctx, sel, add)
	return lines, nil
}

// describeCluster reports cluster and node-pool state and returns the instance-group
// URLs the pools name, so the MIG sub-query is HANDED the groups belonging to this
// cluster — where the AWS side has to guess them from an ASG name substring
// (asgInCluster in cloud/aws/resourcehealth.go).
//
// autopilot comes back alongside because cluster.autopilot.enabled is only readable
// here, and it decides whether the node-level sub-queries run at all.
func (c *Client) describeCluster(ctx context.Context, add func(string, ...any)) (migs []string, autopilot bool) {
	if c.clusterName == "" || c.location == "" {
		// Not attempted rather than attempted and failed. A get built from an empty
		// name is a guaranteed 404 whose message blames the API for what is really an
		// unset config key, and this lens has no way to tell that 404 from a real one.
		add("gke: cluster scope unresolved (cluster=%q location=%q) — set cloud.gcp.cluster_name "+
			"and cloud.gcp.location", c.clusterName, c.location)
		return nil, false
	}

	resource := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", c.project, c.location, c.clusterName)
	cl, err := c.container.Projects.Locations.Clusters.Get(resource).Context(ctx).Do()
	if err != nil {
		add("gke cluster %s: %s", c.clusterName, describeAPIError(err,
			fmt.Sprintf("the RunLore principal is missing roles/container.clusterViewer on project %s%s",
				c.project,
				// The project NUMBER, not the id: an IAM principal:// binding is written
				// with the number and rejects the id, so an operator pasting the id from
				// this line would get a binding that does not resolve.
				optional(" (project number %s — the form an IAM principal:// binding requires)", c.projectNum)),
			fmt.Sprintf("no cluster %q at location %q in project %q — check cloud.gcp.cluster_name "+
				"and cloud.gcp.location%s", c.clusterName, c.location, c.project,
				optional(" (this scope was resolved from: %s)", c.identitySrc))))
		return nil, false
	}

	autopilot = cl.Autopilot != nil && cl.Autopilot.Enabled
	mode := ""
	if autopilot {
		mode = " mode=Autopilot"
	}
	// Cluster-wide, and labelled as such. It is the only current node count the
	// container API returns; there is no per-pool equivalent, and presenting this
	// number next to a single pool would read as that pool's size.
	nodes := ""
	if cl.CurrentNodeCount > 0 {
		nodes = fmt.Sprintf(" nodes=%d (cluster-wide)", cl.CurrentNodeCount)
	}
	name := cl.Name
	if name == "" {
		name = c.clusterName
	}
	// The location is echoed because the scope is usually autodetected. Autodetection
	// landing on a same-named cluster in a neighbouring region answers confidently
	// about the wrong cluster, and this line is where a reader can catch it.
	add("GKE cluster %s (%s): status=%s%s%s%s%s", name, c.location, cl.Status,
		optional(" (%s)", cl.StatusMessage),
		optional(" control-plane version=%s", cl.CurrentMasterVersion), nodes, mode)
	for _, cond := range cl.Conditions {
		if cond == nil {
			continue
		}
		add("  cluster condition: %s", renderCondition(cond))
	}

	if len(cl.NodePools) == 0 && !autopilot {
		// A finding, not an empty section: a Standard cluster with no node pools has
		// nowhere to schedule, which is a complete answer to "why is nothing running".
		// Autopilot is excluded because a cluster with no workloads legitimately has no
		// pools yet, and Google creates them on demand.
		add("  no node pools — this cluster has no capacity to schedule on")
	}
	// Bound the listing the way every other listing in this lens family is bounded
	// (cloud/aws/resourcehealth.go caps nodegroups and ASGs at the same budget), because
	// GKE allows hundreds of node pools per cluster and an unbounded describe would
	// spend a model's whole context on a pool inventory.
	//
	// The cap applies only to pools with nothing to report. Capping uniformly would
	// drop the degraded pool behind twenty healthy ones — losing precisely the row this
	// lens exists to surface, and silently, since a truncation line cannot say what it
	// cut.
	quiet := int64(0)
	omitted := 0
	for _, np := range cl.NodePools {
		if np == nil {
			continue
		}
		if !notablePool(np, cl.CurrentMasterVersion) {
			if quiet >= c.maxEvents {
				omitted++
				continue
			}
			quiet++
		}
		// No node COUNT on this line. container.NodePool carries only
		// initialNodeCount, the creation-time per-zone value that never changes: a pool
		// created with 3 and now pinned at its ceiling of 10 would render "nodes=3
		// autoscaling=1..10", and a model investigating a capacity incident reads
		// headroom where there is none — the opposite conclusion, in the lens whose
		// whole job is capacity. The real number is the sum of the pool's instance
		// groups' targetSize, which the MIG sub-query is what fetches.
		add("  node pool %s: status=%s%s%s%s", np.Name, np.Status,
			optional(" (%s)", np.StatusMessage),
			versionSummary(np.Version, cl.CurrentMasterVersion),
			autoscalingSummary(np.Autoscaling))
		for _, cond := range np.Conditions {
			if cond == nil {
				continue
			}
			// Named rather than only indented. These lines reach a model as a flat list
			// of messages, where indentation alone leaves a condition attached to
			// whichever pool the reader last saw.
			add("    node pool %s condition: %s", np.Name, renderCondition(cond))
		}
		migs = append(migs, np.InstanceGroupUrls...)
	}
	if omitted > 0 {
		// Not providers.TruncationLine: that sentinel tells the reader to "narrow the
		// query or shorten the window", and a cluster describe has neither. The AWS
		// lens words its own listing caps for the same reason
		// (cloud/aws/resourcehealth.go). Saying the omitted pools were healthy is the
		// load-bearing part — it is what keeps the cap from reading as a hidden finding.
		add("  … %d further node pools omitted, all RUNNING with no conditions and at the "+
			"control-plane version", omitted)
	}
	return migs, autopilot
}

// notablePool reports whether a node pool carries anything worth a line of a model's
// attention: any status other than a clean RUNNING, any condition, or a version behind
// the control plane. Pools that pass are exempt from the listing cap.
func notablePool(np *container.NodePool, controlPlane string) bool {
	return np.Status != "RUNNING" ||
		len(np.Conditions) > 0 ||
		(np.Version != "" && controlPlane != "" && np.Version != controlPlane)
}

// versionSummary renders a node pool's version and flags skew against the control
// plane.
//
// Skew is worth its own marker because it is invisible from inside the cluster once the
// nodes report Ready, while it is a standing cause of scheduling and API-compatibility
// failures — and because GKE upgrades the control plane and the pools independently, so
// a pool left behind by an auto-upgrade window is an ordinary way to arrive here.
func versionSummary(poolVersion, controlPlane string) string {
	if poolVersion == "" {
		return ""
	}
	out := " version=" + poolVersion
	if controlPlane != "" && poolVersion != controlPlane {
		out += " SKEW: control plane is " + controlPlane
	}
	return out
}

// autoscalingSummary renders a node pool's scaling bounds.
func autoscalingSummary(a *container.NodePoolAutoscaling) string {
	if a == nil || !a.Enabled {
		// Stated, not omitted. "This pool cannot grow at all" is frequently the whole
		// answer during a capacity incident, and an absent field reads as unknown.
		return " autoscaling=off"
	}
	// totalMin/MaxNodeCount are the pool-wide bounds and the API documents them as
	// mutually exclusive with the per-location min/maxNodeCount pair. Reading only the
	// per-location pair prints "autoscaling=0..0" for a pool configured with totals — a
	// pool that can reach 30 nodes reported as unable to scale at all.
	if a.TotalMaxNodeCount > 0 {
		return fmt.Sprintf(" autoscaling=%d..%d pool total", a.TotalMinNodeCount, a.TotalMaxNodeCount)
	}
	return fmt.Sprintf(" autoscaling=%d..%d per zone", a.MinNodeCount, a.MaxNodeCount)
}

// renderCondition renders one cluster or node-pool condition.
//
// CanonicalCode is the google.rpc.Code name; Code is the older GKE-specific enum the
// API documents as deprecated but still populates on some responses. Preferring the
// canonical one and falling back to the other keeps a condition from arriving as a bare
// message with no classification, which is the difference between a model seeing
// RESOURCE_EXHAUSTED and seeing a sentence it has to interpret.
func renderCondition(cond *container.StatusCondition) string {
	code := cond.CanonicalCode
	if code == "" {
		code = cond.Code
	}
	switch {
	case code == "":
		return cond.Message
	case cond.Message == "":
		return code
	default:
		return code + " — " + cond.Message
	}
}

// describeAPIError turns a Google API error into an actionable line.
//
// 403 and 404 are separated deliberately, and this is the one place this provider goes
// beyond the AWS error shape. A 403 on clusters.get is a missing IAM binding. A 404 is a
// cluster name or location that does not exist — which, on a deployment that configures
// nothing and lets identity.go autodetect the scope, means autodetection picked wrong.
// Reporting both as "denied" sends the operator to the IAM console for a metadata
// problem, and every binding they add there will fail to fix it.
//
// The API's own message is appended in all three cases: it is the only part of the line
// that says which permission or which resource the service actually objected to.
func describeAPIError(err error, onForbidden, onNotFound string) string {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		// A transport failure, a context deadline or a credential refusal — no HTTP
		// status to key on, so the error is reported verbatim rather than guessed at.
		return fmt.Sprintf("lookup failed: %v", err)
	}
	switch gerr.Code {
	case http.StatusForbidden:
		return "permission denied — " + onForbidden + apiDetail(gerr)
	case http.StatusNotFound:
		return "not found — " + onNotFound + apiDetail(gerr)
	default:
		return fmt.Sprintf("lookup failed (HTTP %d)%s", gerr.Code, apiDetail(gerr))
	}
}

// apiDetail renders the service's own message, or nothing when it sent none.
func apiDetail(gerr *googleapi.Error) string {
	return optional(": %s", strings.TrimSpace(gerr.Message))
}

// optional renders format with v only when v is non-empty.
//
// Every optional field in this lens goes through it, because the alternative — an
// unconditionally appended suffix — renders an ABSENT value as a LOST one, and those
// are different claims. renderCall in auditlog.go guards its " by <principal>" for
// exactly this reason: unguarded, a Google-initiated system_event reads
// "compute.instances.hostError by ", which says the caller's identity went missing
// where the truth is that there was no caller. An empty "()" and a trailing "resolved
// from: " are the same defect in this file.
func optional(format, v string) string {
	if v == "" {
		return ""
	}
	return fmt.Sprintf(format, v)
}

// describeMIGs reports managed-instance-group errors — stockouts, quota and IP
// exhaustion — and instance churn for the groups the node pools named. Not implemented
// yet; it lands with the compute half of this lens.
func (*Client) describeMIGs(context.Context, []string, providers.TimeWindow, func(string, ...any)) {
}

// describeInstance reports a single Compute Engine instance's status when the selector
// names one. Not implemented yet; it lands with the compute half of this lens.
func (*Client) describeInstance(context.Context, providers.Selector, func(string, ...any)) {
}
