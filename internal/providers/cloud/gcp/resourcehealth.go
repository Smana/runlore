// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"
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

	c.describeMIGs(ctx, migs, w, autopilot, add)
	c.describeInstance(ctx, sel, autopilot, add)
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

// describeMIGs reports capacity failures and instance churn for the instance groups
// describeCluster handed over. listErrors is the highest-value line this provider
// emits: a stockout, an exhausted quota or an exhausted IP range stops a pool scaling
// with no in-cluster symptom beyond Pods that stay Pending — the nodes simply never
// arrive, and no Kubernetes object records why.
//
// It emits NOTHING when handed no groups. That case is reached two ways — a cluster
// with no pools, and a cluster lookup that failed — and in the second the error line
// above is already the answer. A "no instance-group errors" line under a cluster error
// would answer a question nothing asked with a reassurance nothing established.
//
// autopilot only changes the wording of an EMPTY answer, never whether the lookup runs:
// Autopilot node VMs and their groups live in the customer project and read with the
// same roles/compute.viewer, so the finding is available and worth having. What
// Autopilot does change is what silence means — Google manages that layer, so an empty
// answer may be missing visibility rather than missing problems, and the two must not
// render identically.
func (c *Client) describeMIGs(ctx context.Context, migs []string, w providers.TimeWindow, autopilot bool, add func(string, ...any)) {
	if len(migs) == 0 {
		return
	}
	found := false
	for _, self := range migs {
		scope, name, zonal, ok := migName(self)
		if !ok {
			// Reported rather than skipped: an unparseable self-link means a group
			// this lens cannot check, and silence would count as "checked, clean".
			add("gke node group: cannot route a lookup for %q — unrecognised self-link shape", self)
			found = true
			continue
		}
		if c.migErrors(ctx, scope, name, zonal, w, add) {
			found = true
		}
		if c.migChurn(ctx, scope, name, zonal, add) {
			found = true
		}
	}
	if found {
		return
	}
	if autopilot {
		add("gke node groups: no instance-group errors or instance churn reported — this is an " +
			"Autopilot cluster, so the node layer is Google-managed and this may reflect limited " +
			"visibility rather than an absence of problems")
		return
	}
	add("gke node groups: no instance-group errors or instance churn in the window")
}

// migErrors reads one group's recent failures, window-scoped. It reports whether it
// wrote anything, so the caller can tell a quiet group from an unchecked one.
func (c *Client) migErrors(ctx context.Context, scope, name string, zonal bool, w providers.TimeWindow, add func(string, ...any)) bool {
	var (
		errs []*compute.InstanceManagedByIgmError
		err  error
	)
	if zonal {
		var resp *compute.InstanceGroupManagersListErrorsResponse
		resp, err = c.compute.InstanceGroupManagers.ListErrors(c.project, scope, name).Context(ctx).Do()
		if resp != nil {
			errs = resp.Items
		}
	} else {
		var resp *compute.RegionInstanceGroupManagersListErrorsResponse
		resp, err = c.compute.RegionInstanceGroupManagers.ListErrors(c.project, scope, name).Context(ctx).Do()
		if resp != nil {
			errs = resp.Items
		}
	}
	if err != nil {
		add("gke node group %s (%s): %s", name, scope, describeAPIError(err,
			"the RunLore principal is missing roles/compute.viewer on project "+c.project+
				optional(" (project number %s — the form an IAM principal:// binding requires)", c.projectNum),
			fmt.Sprintf("no instance group %q in %q — a pool deleted mid-incident reads this way", name, scope)))
		return true
	}
	wrote := false
	for _, e := range errs {
		if e == nil || e.Error == nil {
			continue
		}
		if !errorInWindow(e.Timestamp, w) {
			continue
		}
		add("gke node group %s (%s): %s%s%s", name, scope, e.Error.Code,
			optional(" — %s", strings.TrimSpace(e.Error.Message)),
			optional(" at %s", e.Timestamp))
		wrote = true
	}
	return wrote
}

// errorInWindow keeps an error the window covers, and keeps one the API dated in a form
// this lens cannot parse.
//
// Scoping mirrors activityBeforeWindow on the AWS side: a group retains its errors for
// hours, so an unscoped read hands the model last night's stockout while today's
// incident is a config change — a wrong cause that arrives looking well-evidenced.
// Keeping the unparseable ones is the opposite trade: dropping a real capacity failure
// over a timestamp format is the more expensive mistake, and the raw timestamp is
// printed beside it so a reader can judge.
func errorInWindow(ts string, w providers.TimeWindow) bool {
	at, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	if !w.Start.IsZero() && at.Before(w.Start) {
		return false
	}
	if !w.End.IsZero() && at.After(w.End) {
		return false
	}
	return true
}

// migChurn reports instances mid-action, capped. A pool in a create-fail-recreate loop
// can hold hundreds at once, and listing them all would spend the model's context on
// one symptom while crowding out the listErrors line that says why they keep failing.
// Only instances actually moving are listed: a steady instance is not churn, and
// spending the budget on it would hide the ones that are.
func (c *Client) migChurn(ctx context.Context, scope, name string, zonal bool, add func(string, ...any)) bool {
	var (
		insts []*compute.ManagedInstance
		err   error
	)
	if zonal {
		var resp *compute.InstanceGroupManagersListManagedInstancesResponse
		resp, err = c.compute.InstanceGroupManagers.ListManagedInstances(c.project, scope, name).Context(ctx).Do()
		if resp != nil {
			insts = resp.ManagedInstances
		}
	} else {
		var resp *compute.RegionInstanceGroupManagersListInstancesResponse
		resp, err = c.compute.RegionInstanceGroupManagers.ListManagedInstances(c.project, scope, name).Context(ctx).Do()
		if resp != nil {
			insts = resp.ManagedInstances
		}
	}
	if err != nil {
		add("gke node group %s (%s) instances: %s", name, scope, describeAPIError(err,
			"the RunLore principal is missing roles/compute.viewer on project "+c.project+
				optional(" (project number %s — the form an IAM principal:// binding requires)", c.projectNum),
			fmt.Sprintf("no instance group %q in %q", name, scope)))
		return true
	}
	shown, moving := 0, 0
	for _, mi := range insts {
		if mi == nil || mi.CurrentAction == "" || mi.CurrentAction == "NONE" {
			continue
		}
		moving++
		if int64(shown) >= c.maxEvents {
			continue
		}
		add("gke node group %s (%s): instance %s %s%s", name, scope, mi.Name, mi.CurrentAction,
			optional(" (%s)", mi.InstanceStatus))
		shown++
	}
	if over := moving - shown; over > 0 {
		add("gke node group %s (%s): %d further instances mid-action, not listed", name, scope, over)
	}
	return shown > 0
}

// describeInstance answers the question a Node object cannot: a node NotReady because
// its VM is TERMINATED and a node NotReady with a healthy VM under it are the same
// symptom in Kubernetes and completely different incidents.
//
// aggregatedList rather than instances.get, because the selector carries a name and no
// zone. get needs both, so reaching it would mean guessing a zone — and a wrong guess
// 404s, which this lens renders as "that instance is gone": a false negative on exactly
// the question being asked.
func (c *Client) describeInstance(ctx context.Context, sel providers.Selector, autopilot bool, add func(string, ...any)) {
	if sel.Name == "" {
		return
	}
	resp, err := c.compute.Instances.AggregatedList(c.project).
		Filter("name=" + sel.Name).Context(ctx).Do()
	if err != nil {
		add("compute instance %s: %s", sel.Name, describeAPIError(err,
			"the RunLore principal is missing roles/compute.viewer on project "+c.project+
				optional(" (project number %s — the form an IAM principal:// binding requires)", c.projectNum),
			fmt.Sprintf("no instance %q in project %q", sel.Name, c.project)))
		return
	}
	for scope, list := range resp.Items {
		for _, in := range list.Instances {
			if in == nil || in.Name != sel.Name {
				continue
			}
			add("compute instance %s (%s): status=%s%s%s", in.Name,
				strings.TrimPrefix(scope, "zones/"), in.Status,
				optional(" (%s)", strings.TrimSpace(in.StatusMessage)),
				optional(" last started %s", in.LastStartTimestamp))
			return
		}
	}
	if autopilot {
		add("compute instance %s: not found in project %s — this is an Autopilot cluster, so the "+
			"node layer is Google-managed and this may reflect limited visibility rather than a "+
			"deleted instance", sel.Name, c.project)
		return
	}
	add("compute instance %s: not found in project %s", sel.Name, c.project)
}

// migName splits a compute self-link into the scope its lookup must target, the group
// name, and whether that scope is a zone.
//
// Routing is the whole point: a zonal group sent to the regional endpoint 404s, and a
// 404 in this lens renders as "that group is gone" — a silent false negative on the
// sub-query whose purpose is catching capacity failures.
//
// Both the instanceGroupManagers and instanceGroups spellings resolve, because the
// container API has published both for the same field. A zonal instance group and the
// manager owning it share a name, so the unmanaged spelling names the same manager
// rather than being unparseable — and dropping it would silently skip the group.
func migName(selfLink string) (scope, name string, zonal, ok bool) {
	parts := strings.Split(selfLink, "/")
	for i := len(parts) - 2; i > 0; i-- {
		if parts[i] != "instanceGroupManagers" && parts[i] != "instanceGroups" {
			continue
		}
		group := parts[i+1]
		if group == "" || i < 2 {
			return "", "", false, false
		}
		switch parts[i-2] {
		case "zones":
			return parts[i-1], group, true, true
		case "regions":
			return parts[i-1], group, false, true
		}
		return "", "", false, false
	}
	return "", "", false, false
}
