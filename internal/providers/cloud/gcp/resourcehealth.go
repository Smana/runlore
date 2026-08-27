// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
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
	var b lineBuf

	migs, autopilot := c.describeCluster(ctx, &b)

	c.describeMIGs(ctx, &b, migs, w, autopilot)
	c.describeInstance(ctx, &b, sel, autopilot)
	return b.lines, nil
}

// lineBuf accumulates the model-facing lines one call produces.
//
// A type rather than the closure this replaced, for two reasons. The closure had to be
// threaded through five signatures as an `add func(string, ...any)` parameter, and — the
// reason that matters — it appended to a captured slice, so it could not be called from
// the goroutines describeMIGs now uses. Each concurrent worker owns its own buffer and
// the results are merged in input order, which keeps the output deterministic.
type lineBuf struct{ lines providers.LogResult }

func (b *lineBuf) addf(format string, a ...any) {
	b.lines = append(b.lines, providers.LogLine{Message: fmt.Sprintf(format, a...)})
}

// migConcurrency bounds the in-flight Compute calls one ResourceHealth makes.
//
// The groups are independent, so the depth of this sub-query used to be its width: a
// regional cluster with a few dozen node pools contributes one instance group per zone
// per pool, and each group costs a sequential round-trip. Enough of those and the whole
// tool call hits its timeout and the lens returns nothing at all — the failure mode
// where being slow becomes being absent.
//
// Eight rather than unbounded: Compute's per-project read quota is shared with
// everything else in the project, and a health lens that answers by exhausting it has
// traded one problem for a worse one.
const migConcurrency = 8

// maxMIGsExamined is a hard ceiling on how many instance groups one call will QUERY.
//
// Distinct from the node-pool listing cap, which bounds only how many pool lines are
// printed. GKE allows hundreds of node pools; a regional cluster multiplies that by its
// zone count, and a blue-green node-pool upgrade — precisely when this lens gets
// consulted — has the container API report both the blue and the green groups at once.
// Without a ceiling the fan-out is unbounded in the one situation it most needs to be
// bounded. What is dropped is always reported: a silent cap reads as "checked, clean".
const maxMIGsExamined = 48

// describeCluster reports cluster and node-pool state and returns the instance-group
// URLs the pools name, so the MIG sub-query is HANDED the groups belonging to this
// cluster — where the AWS side has to guess them from an ASG name substring
// (asgInCluster in cloud/aws/resourcehealth.go).
//
// autopilot comes back alongside because cluster.autopilot.enabled is only readable
// here, and it decides whether the node-level sub-queries run at all.
func (c *Client) describeCluster(ctx context.Context, b *lineBuf) (migs []string, autopilot bool) {
	if c.id.ClusterName == "" || c.id.Location == "" {
		// Not attempted rather than attempted and failed. A get built from an empty
		// name is a guaranteed 404 whose message blames the API for what is really an
		// unset config key, and this lens has no way to tell that 404 from a real one.
		b.addf("gke: cluster scope unresolved (cluster=%q location=%q) — set cloud.gcp.cluster_name "+
			"and cloud.gcp.location", c.id.ClusterName, c.id.Location)
		return nil, false
	}

	resource := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", c.id.Project, c.id.Location, c.id.ClusterName)
	// Field mask: clusters.get otherwise returns every NodePool's full NodeConfig —
	// labels, taints, metadata, accelerators, shielded config — none of which is read
	// here. On a large cluster that is a few hundred KB of JSON parsed and discarded on
	// every call.
	cl, err := c.container.Projects.Locations.Clusters.Get(resource).
		Fields("name", "status", "statusMessage", "currentMasterVersion", "currentNodeCount",
			"autopilot/enabled", "conditions",
			"nodePools(name,status,statusMessage,version,autoscaling,conditions,instanceGroupUrls)").
		Context(ctx).Do()
	if err != nil {
		b.addf("gke cluster %s: %s", c.id.ClusterName, describeAPIError(err,
			c.missingRole("container.clusterViewer"),
			fmt.Sprintf("no cluster %q at location %q in project %q — check cloud.gcp.cluster_name "+
				"and cloud.gcp.location%s", c.id.ClusterName, c.id.Location, c.id.Project,
				optional(" (this scope was resolved from: %s)", c.id.Source))))
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
		name = c.id.ClusterName
	}
	// The location is echoed because the scope is usually autodetected. Autodetection
	// landing on a same-named cluster in a neighbouring region answers confidently
	// about the wrong cluster, and this line is where a reader can catch it.
	b.addf("GKE cluster %s (%s): status=%s%s%s%s%s", name, c.id.Location, cl.Status,
		optional(" (%s)", cl.StatusMessage),
		optional(" control-plane version=%s", cl.CurrentMasterVersion), nodes, mode)
	for _, cond := range cl.Conditions {
		if cond == nil {
			continue
		}
		b.addf("  cluster condition: %s", renderCondition(cond))
	}

	if len(cl.NodePools) == 0 && !autopilot {
		// A finding, not an empty section: a Standard cluster with no node pools has
		// nowhere to schedule, which is a complete answer to "why is nothing running".
		// Autopilot is excluded because a cluster with no workloads legitimately has no
		// pools yet, and Google creates them on demand.
		b.addf("  no node pools — this cluster has no capacity to schedule on")
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
	//
	// It caps the LINE, never the lookup. An omitted pool's instance groups are still
	// handed to the MIG sub-query, because "quiet at the pool level" and "healthy" are
	// different claims: a pool whose scale-out is failing on a zonal stockout or an
	// exhausted IP range reports status=RUNNING with no conditions, so notablePool
	// returns false and it lands in exactly this bucket. Skipping its groups would drop
	// the highest-value finding this provider emits, and the truncation line below would
	// then assert those pools were fine — something nothing had checked.
	quiet := 0
	omitted := 0
	for _, np := range cl.NodePools {
		if np == nil {
			continue
		}
		// Collected before the cap decision, for every pool, on purpose.
		migs = append(migs, np.InstanceGroupUrls...)
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
		b.addf("  node pool %s: status=%s%s%s%s", np.Name, np.Status,
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
			b.addf("    node pool %s condition: %s", np.Name, renderCondition(cond))
		}
	}
	if omitted > 0 {
		// Not providers.TruncationLine: that sentinel tells the reader to "narrow the
		// query or shorten the window", and a cluster describe has neither. The AWS
		// lens words its own listing caps for the same reason
		// (cloud/aws/resourcehealth.go). Saying the omitted pools were healthy is the
		// load-bearing part — it is what keeps the cap from reading as a hidden finding.
		b.addf("  … %d further node pools omitted from this listing, all RUNNING with no conditions "+
			"and at the control-plane version; their instance groups were still checked below", omitted)
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

// missingRole renders the "bind this role" half of a permission diagnosis.
//
// One copy because the parenthetical is load-bearing and was written out four times: an
// IAM principal:// binding is spelled with the project NUMBER and silently never
// matches when given the id, so an operator pasting the id from this line gets a
// binding that looks right and does nothing. Four copies of a sentence whose exact
// wording is what prevents that is four chances for one of them to drift.
func (c *Client) missingRole(role string) string {
	return fmt.Sprintf("the RunLore principal is missing roles/%s on project %s%s",
		role, c.id.Project,
		optional(" (project number %s — the form an IAM principal:// binding requires)", c.id.ProjectNumber))
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
func (c *Client) describeMIGs(ctx context.Context, b *lineBuf, migs []string, w providers.TimeWindow, autopilot bool) {
	if len(migs) == 0 {
		return
	}
	// Ceiling on groups QUERIED, applied before any call is made.
	overflow := 0
	if len(migs) > maxMIGsExamined {
		overflow = len(migs) - maxMIGsExamined
		migs = migs[:maxMIGsExamined]
	}

	// One aggregated read answers "which groups have anything moving at all", so the
	// per-group instance listing only runs where there is churn to enumerate. On a
	// healthy cluster — the normal case — that replaces one round-trip per group with
	// one round-trip total.
	churn, haveChurn := c.migChurnCounts(ctx)

	// Fan out over the groups. Each worker writes its OWN buffer and the buffers are
	// concatenated in input order afterwards, so concurrency changes the latency and
	// nothing else: the lines a model sees are byte-identical to the sequential order.
	bufs := make([]lineBuf, len(migs))
	found := make([]bool, len(migs))
	sem := make(chan struct{}, migConcurrency)
	var wg sync.WaitGroup
	for i, self := range migs {
		wg.Add(1)
		go func(i int, self string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			wb := &bufs[i]
			scope, name, zonal, ok := migName(self)
			if !ok {
				// Reported rather than skipped: an unparseable self-link means a group
				// this lens cannot check, and silence would count as "checked, clean".
				wb.addf("gke node group: cannot route a lookup for %q — unrecognised self-link shape", self)
				found[i] = true
				return
			}
			if c.migErrors(ctx, wb, scope, name, zonal, w) {
				found[i] = true
			}
			// Skip the listing only when the aggregated summary was actually read AND
			// reported this group still. An unavailable summary must not be read as
			// "nothing is churning" — that would turn a permission gap into a clean
			// bill of health — so the per-group call still runs in that case.
			if !haveChurn || churn[scope+"/"+name] > 0 {
				if c.migChurn(ctx, wb, scope, name, zonal) {
					found[i] = true
				}
			}
		}(i, self)
	}
	wg.Wait()

	anyFound := false
	for i := range bufs {
		b.lines = append(b.lines, bufs[i].lines...)
		if found[i] {
			anyFound = true
		}
	}
	if overflow > 0 {
		// Said out loud, and said even when everything examined was clean. A capped
		// fan-out that reports nothing is indistinguishable from a healthy cluster,
		// which is the one reading this line exists to prevent.
		b.addf("gke node groups: … %d further instance groups NOT checked (examined %d of %d) — "+
			"this cluster has more node pools than one health call can query; scope the "+
			"investigation to a node or pool to see them", overflow, len(migs), len(migs)+overflow)
	}
	if anyFound {
		return
	}
	if autopilot {
		b.addf("gke node groups: no instance-group errors or instance churn reported — this is an " +
			"Autopilot cluster, so the node layer is Google-managed and this may reflect limited " +
			"visibility rather than an absence of problems")
		return
	}
	b.addf("gke node groups: no instance-group errors or instance churn in the window")
}

// migChurnCounts reports, in one aggregated call, how many instances each managed
// instance group in the project currently has mid-action, keyed by "<scope>/<name>".
//
// ok=false means the summary is unavailable — the call failed, or the caller must not
// rely on it. It deliberately reports no error line of its own: every group's own
// lookups run right after and produce a role-specific diagnosis in place, so an error
// here would be the same permission problem stated twice.
func (c *Client) migChurnCounts(ctx context.Context) (map[string]int64, bool) {
	counts := map[string]int64{}
	token := ""
	for {
		call := c.compute.InstanceGroupManagers.AggregatedList(c.id.Project).
			// returnPartialSuccess is what the API's own documentation recommends for
			// aggregated lists: without it a single unreachable zone fails the whole
			// call, and this is an optimisation that must never cost the answer.
			ReturnPartialSuccess(true).
			Fields("nextPageToken", "items").
			Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, false
		}
		for scope, list := range resp.Items {
			for _, m := range list.InstanceGroupManagers {
				if m == nil || m.CurrentActions == nil {
					continue
				}
				counts[strings.TrimPrefix(strings.TrimPrefix(scope, "zones/"), "regions/")+"/"+m.Name] =
					movingInstances(m.CurrentActions)
			}
		}
		if resp.NextPageToken == "" {
			return counts, true
		}
		token = resp.NextPageToken
	}
}

// movingInstances sums every current action except None.
//
// Enumerated rather than derived from a total, because there is no total field and
// because "moving" is the question: a group sitting at its target size with every
// instance in None is not churning, and counting those would make every group look busy.
func movingInstances(a *compute.InstanceGroupManagerActionsSummary) int64 {
	return a.Abandoning + a.Creating + a.CreatingWithoutRetries + a.Deleting +
		a.Recreating + a.Refreshing + a.Restarting + a.Resuming +
		a.Starting + a.Stopping + a.Suspending + a.Verifying
}

// listMIGErrors reads one group's recent failures, routing zonal and regional groups to
// their own endpoints. Split out so migErrors reads as a flat function: the shape this
// replaced declared its response inside each branch while assigning to an `err` from the
// enclosing scope, which is the hardest thing in this file to follow.
func (c *Client) listMIGErrors(ctx context.Context, scope, name string, zonal bool) ([]*compute.InstanceManagedByIgmError, error) {
	if zonal {
		resp, err := c.compute.InstanceGroupManagers.ListErrors(c.id.Project, scope, name).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		return resp.Items, nil
	}
	resp, err := c.compute.RegionInstanceGroupManagers.ListErrors(c.id.Project, scope, name).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// listManagedInstances reads one group's instances, routing zonal and regional groups to
// their own endpoints.
func (c *Client) listManagedInstances(ctx context.Context, scope, name string, zonal bool) ([]*compute.ManagedInstance, error) {
	const fields = "managedInstances(name,currentAction,instanceStatus)"
	if zonal {
		resp, err := c.compute.InstanceGroupManagers.ListManagedInstances(c.id.Project, scope, name).
			Fields(fields).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		return resp.ManagedInstances, nil
	}
	resp, err := c.compute.RegionInstanceGroupManagers.ListManagedInstances(c.id.Project, scope, name).
		Fields(fields).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	return resp.ManagedInstances, nil
}

// migErrors reads one group's recent failures, window-scoped. It reports whether it
// wrote anything, so the caller can tell a quiet group from an unchecked one.
func (c *Client) migErrors(ctx context.Context, b *lineBuf, scope, name string, zonal bool, w providers.TimeWindow) bool {
	errs, err := c.listMIGErrors(ctx, scope, name, zonal)
	if err != nil {
		b.addf("gke node group %s (%s): %s", name, scope, describeAPIError(err,
			c.missingRole("compute.viewer"),
			fmt.Sprintf("no instance group %q in %q — a pool deleted mid-incident reads this way", name, scope)))
		return true
	}
	// Capped, the way migChurn below is capped and for the same reason. ListErrors
	// defaults to 500 results per page, and a group stuck in a create-fail-recreate
	// loop — the exact case this sub-query exists to catch — returns hundreds of
	// in-window errors that are all the same error. Uncapped, one such group spends the
	// whole per-call row budget and crowds out every other group's lines.
	shown, inWindow := 0, 0
	for _, e := range errs {
		if e == nil || e.Error == nil {
			continue
		}
		if !errorInWindow(e.Timestamp, w) {
			continue
		}
		inWindow++
		if shown >= c.maxEvents {
			continue
		}
		b.addf("gke node group %s (%s): %s%s%s", name, scope, e.Error.Code,
			optional(" — %s", strings.TrimSpace(e.Error.Message)),
			optional(" at %s", e.Timestamp))
		shown++
	}
	if over := inWindow - shown; over > 0 {
		b.addf("gke node group %s (%s): %d further errors in the window, not listed", name, scope, over)
	}
	return shown > 0
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
func (c *Client) migChurn(ctx context.Context, b *lineBuf, scope, name string, zonal bool) bool {
	insts, err := c.listManagedInstances(ctx, scope, name, zonal)
	if err != nil {
		b.addf("gke node group %s (%s) instances: %s", name, scope, describeAPIError(err,
			c.missingRole("compute.viewer"),
			fmt.Sprintf("no instance group %q in %q", name, scope)))
		return true
	}
	shown, moving := 0, 0
	for _, mi := range insts {
		if mi == nil || mi.CurrentAction == "" || mi.CurrentAction == "NONE" {
			continue
		}
		moving++
		if shown >= c.maxEvents {
			continue
		}
		b.addf("gke node group %s (%s): instance %s %s%s", name, scope, mi.Name, mi.CurrentAction,
			optional(" (%s)", mi.InstanceStatus))
		shown++
	}
	if over := moving - shown; over > 0 {
		b.addf("gke node group %s (%s): %d further instances mid-action, not listed", name, scope, over)
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
func (c *Client) describeInstance(ctx context.Context, b *lineBuf, sel providers.Selector, autopilot bool) {
	if sel.Name == "" {
		return
	}
	// Quoted: sel.Name is model-written free text, and an unquoted value containing a
	// space or a quote makes Compute reject the whole filter with a 400 that this lens
	// then renders as a lookup failure for a name that may well exist.
	matches, err := c.findInstances(ctx, sel.Name)
	if err != nil {
		b.addf("compute instance %s: %s", sel.Name, describeAPIError(err,
			c.missingRole("compute.viewer"),
			fmt.Sprintf("no instance %q in project %q", sel.Name, c.id.Project)))
		return
	}
	for _, m := range matches {
		b.addf("compute instance %s (%s): status=%s%s%s", m.instance.Name,
			m.zone, m.instance.Status,
			optional(" (%s)", strings.TrimSpace(m.instance.StatusMessage)),
			optional(" last started %s", m.instance.LastStartTimestamp))
	}
	if len(matches) > 1 {
		// Reported rather than resolved, because this lens cannot resolve it: a GCE
		// instance name is unique per ZONE, not per project, and the selector carries no
		// zone. Returning whichever one came back first made the answer depend on Go's
		// randomised map iteration order — the same node could be reported RUNNING on
		// one call and TERMINATED on the next.
		b.addf("compute instance %s: %d instances share this name in different zones — "+
			"all are listed above; the node under investigation is whichever zone it "+
			"schedules in", sel.Name, len(matches))
	}
	if len(matches) > 0 {
		return
	}
	if autopilot {
		b.addf("compute instance %s: not found in project %s — this is an Autopilot cluster, so the "+
			"node layer is Google-managed and this may reflect limited visibility rather than a "+
			"deleted instance", sel.Name, c.id.Project)
		return
	}
	b.addf("compute instance %s: not found in project %s", sel.Name, c.id.Project)
}

// instanceFilterFor builds the aggregatedList filter for an instance name.
//
// %q, not concatenation. name is model-written free text, and an unquoted value carrying
// a space or a quote makes Compute reject the whole filter with a 400 — which this lens
// renders as a lookup failure for a name that may well exist, i.e. a false negative on
// exactly the question being asked.
func (c *Client) instanceFilterFor(name string) string {
	return fmt.Sprintf("name=%q", name)
}

// instanceMatch is one instance that carried the requested name, with the zone that
// disambiguates it.
type instanceMatch struct {
	zone     string
	instance *compute.Instance
}

// findInstances returns every instance in the project with this exact name, ordered by
// zone so the answer does not depend on map iteration order.
//
// It follows nextPageToken. An aggregated list that stops at its first page renders a
// match on a later page as "not found in project" — which describeInstance's own doc
// comment calls a false negative on exactly the question being asked.
func (c *Client) findInstances(ctx context.Context, name string) ([]instanceMatch, error) {
	var out []instanceMatch
	token := ""
	for {
		call := c.compute.Instances.AggregatedList(c.id.Project).
			Filter(c.instanceFilterFor(name)).
			ReturnPartialSuccess(true).
			Fields("nextPageToken", "items").
			Context(ctx)
		if token != "" {
			call = call.PageToken(token)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, err
		}
		for scope, list := range resp.Items {
			for _, in := range list.Instances {
				// The name is re-checked because the server-side filter is the API's,
				// not this lens's: a filter that ever matched more loosely would put a
				// different instance's status under the name being investigated.
				if in == nil || in.Name != name {
					continue
				}
				out = append(out, instanceMatch{zone: strings.TrimPrefix(scope, "zones/"), instance: in})
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	sort.Slice(out, func(i, j int) bool { return out[i].zone < out[j].zone })
	return out, nil
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
