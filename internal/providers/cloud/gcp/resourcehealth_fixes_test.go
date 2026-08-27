// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"strings"
	"testing"

	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"

	"github.com/Smana/runlore/internal/providers"
)

// migAggregatedRoute answers compute.instanceGroupManagers.aggregatedList, the one call
// that replaces a per-group listManagedInstances on a healthy cluster.
func migAggregatedRoute(managers map[string][]*compute.InstanceGroupManager) route {
	items := map[string]compute.InstanceGroupManagersScopedList{}
	for scope, ms := range managers {
		items[scope] = compute.InstanceGroupManagersScopedList{InstanceGroupManagers: ms}
	}
	return route{
		frag: "/aggregated/instanceGroupManagers",
		body: compute.InstanceGroupManagerAggregatedList{Items: items},
	}
}

// idleManager is a group with nothing in flight — CurrentActions all zero but None.
func idleManager(name string, idle int64) *compute.InstanceGroupManager {
	return &compute.InstanceGroupManager{
		Name:           name,
		CurrentActions: &compute.InstanceGroupManagerActionsSummary{None: idle},
	}
}

// TestQuietNodePoolsStillHaveTheirInstanceGroupsChecked pins a silent false negative.
//
// The node-pool listing is capped, because GKE allows hundreds of pools and an unbounded
// describe would spend a model's whole context on an inventory. The cap used to `continue`
// past the collection of the omitted pool's instance-group URLs as well — so those groups
// were never queried for capacity failures, while the truncation line asserted the omitted
// pools were "all RUNNING with no conditions".
//
// That assertion was about the POOL level, and the two claims are not the same. A pool
// whose scale-out is failing on a zonal stockout or an exhausted IP range reports
// status=RUNNING with no conditions — notablePool returns false and it lands in exactly
// the omitted bucket. So the one finding this provider exists to surface was dropped
// precisely for the pools the output claimed were fine.
func TestQuietNodePoolsStillHaveTheirInstanceGroupsChecked(t *testing.T) {
	// One more quiet pool than the listing cap, so the last one is omitted from the
	// listing. Its group is the one carrying the stockout.
	const hiddenGroup = "gke-prod-hidden-grp"
	cl := container.Cluster{
		Name: "prod", Location: "europe-west1", Status: "RUNNING",
		CurrentMasterVersion: "1.30.4-gke.100",
	}
	for i := 0; i <= defaultMaxEvents; i++ {
		name := fmt.Sprintf("quiet-%02d", i)
		group := migShortName
		if i == defaultMaxEvents {
			// The pool that gets omitted from the listing.
			name, group = "quiet-last", hiddenGroup
		}
		cl.NodePools = append(cl.NodePools, &container.NodePool{
			Name: name, Status: "RUNNING", Version: "1.30.4-gke.100",
			InstanceGroupUrls: []string{
				"https://www.googleapis.com/compute/v1/projects/my-proj/zones/" +
					migZone + "/instanceGroupManagers/" + group,
			},
		})
	}

	c, srv := healthClient(t, healthIdentity(),
		clusterRoute(cl),
		migErrorsRoute(stockoutError("2026-08-24T10:00:00Z")),
		migInstancesRoute(),
	)

	lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(lines)

	if !srv.requested(hiddenGroup) {
		t.Errorf("the omitted pool's instance group was never queried, so a stockout behind a "+
			"capped listing is invisible:\n%s", out)
	}
	if !strings.Contains(out, "ZONE_RESOURCE_POOL_EXHAUSTED") {
		t.Errorf("the stockout was not reported:\n%s", out)
	}
	// The truncation line may describe the LISTING, never the capacity of what it cut.
	if strings.Contains(out, "all RUNNING with no conditions and at the control-plane version\n") &&
		!strings.Contains(out, "still checked") {
		t.Errorf("the truncation line claims the omitted pools are fine without saying their "+
			"groups were checked:\n%s", out)
	}
}

// TestTheInstanceGroupFanOutHasACeilingAndSaysSo pins a bound on the WORK, not just on
// the output.
//
// The node-pool cap bounds how many pool lines are printed; it never bounded how many
// instance groups got queried. Each group costs two sequential Compute round-trips, a
// regional cluster contributes one group per zone per pool, and a blue-green node-pool
// upgrade has the container API report both the blue and the green groups at once —
// so the fan-out was unbounded exactly when this lens is most likely to be consulted.
// Enough groups and the whole tool call times out, which turns being slow into being
// absent.
//
// What is dropped must be REPORTED: a silently capped fan-out that finds nothing is
// indistinguishable from a healthy cluster.
func TestTheInstanceGroupFanOutHasACeilingAndSaysSo(t *testing.T) {
	cl := container.Cluster{
		Name: "prod", Location: "europe-west1", Status: "RUNNING",
		CurrentMasterVersion: "1.30.4-gke.100",
	}
	// One pool holding far more groups than the ceiling, so the cap is on groups rather
	// than a side effect of the pool listing.
	var urls []string
	for i := 0; i < maxMIGsExamined*3; i++ {
		urls = append(urls, fmt.Sprintf(
			"https://www.googleapis.com/compute/v1/projects/my-proj/zones/%s/instanceGroupManagers/grp-%03d",
			migZone, i))
	}
	cl.NodePools = []*container.NodePool{{
		Name: "big", Status: "RUNNING", Version: "1.30.4-gke.100", InstanceGroupUrls: urls,
	}}

	c, srv := healthClient(t, healthIdentity(),
		clusterRoute(cl), migErrorsRoute(), migInstancesRoute())

	lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(lines)

	listErrorCalls := 0
	for _, p := range srv.requestedPaths() {
		if strings.Contains(p, "/listErrors") {
			listErrorCalls++
		}
	}
	if listErrorCalls > maxMIGsExamined {
		t.Errorf("queried %d instance groups, ceiling is %d", listErrorCalls, maxMIGsExamined)
	}
	if !strings.Contains(out, "NOT checked") {
		t.Errorf("the fan-out was capped silently, so an unchecked cluster reads as a clean "+
			"one:\n%s", out)
	}
}

// TestInstanceLookupIsDeterministicWhenAZoneIsAmbiguous pins the fix for an answer that
// changed between identical calls.
//
// A GCE instance name is unique per ZONE, not per project, and the selector carries no
// zone. describeInstance returned the FIRST match while ranging a map, and Go randomises
// map iteration order per run — so repeated calls for the same node could report
// different zones and different statuses, and the model could be told a TERMINATED VM
// was RUNNING for the node it was investigating.
func TestInstanceLookupIsDeterministicWhenAZoneIsAmbiguous(t *testing.T) {
	const name = "gke-prod-default-abcd"
	twoZones := route{
		frag: "/aggregated/instances",
		body: compute.InstanceAggregatedList{Items: map[string]compute.InstancesScopedList{
			"zones/europe-west1-b": {Instances: []*compute.Instance{{Name: name, Status: "RUNNING"}}},
			"zones/europe-west1-c": {Instances: []*compute.Instance{{Name: name, Status: "TERMINATED"}}},
		}},
	}

	var first string
	for i := 0; i < 12; i++ {
		c, _ := healthClient(t, healthIdentity(),
			clusterRoute(standardCluster()), migErrorsRoute(), migInstancesRoute(), twoZones)
		lines, err := c.ResourceHealth(context.Background(),
			providers.Selector{Name: name}, providers.TimeWindow{})
		if err != nil {
			t.Fatalf("ResourceHealth: %v", err)
		}
		out := joined(lines)
		if i == 0 {
			first = out
			continue
		}
		if out != first {
			t.Fatalf("the same lookup produced different answers across runs — map iteration "+
				"order is leaking into a health verdict:\nfirst:\n%s\nlater:\n%s", first, out)
		}
	}
	// Both are reported, because this lens genuinely cannot resolve the ambiguity and
	// silently picking one is how a TERMINATED VM gets reported as RUNNING.
	for _, want := range []string{"RUNNING", "TERMINATED", "share this name in different zones"} {
		if !strings.Contains(first, want) {
			t.Errorf("the ambiguous match does not report %q:\n%s", want, first)
		}
	}
}

// TestInstanceFilterIsQuoted: sel.Name is model-written free text. Unquoted, a name with
// a space makes Compute reject the whole filter with a 400, which this lens renders as a
// lookup failure for a name that may well exist.
func TestInstanceFilterIsQuoted(t *testing.T) {
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()), migErrorsRoute(), migInstancesRoute(),
		route{frag: "/aggregated/instances", body: compute.InstanceAggregatedList{}})

	// The whole call must survive a hostile name, not just the filter builder.
	if _, err := c.ResourceHealth(context.Background(),
		providers.Selector{Name: `weird name"with quote`}, providers.TimeWindow{}); err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	got := c.instanceFilterFor(`weird name"with quote`)
	if !strings.HasPrefix(got, `name="`) || !strings.Contains(got, `\"`) {
		t.Errorf("the instance filter is not quoted/escaped: %s", got)
	}
}

// TestMIGErrorsAreCappedSoOneLoopingGroupCannotFloodTheLens mirrors the churn cap.
//
// listErrors defaults to 500 results per page, and a group stuck in a create-fail-recreate
// loop — the exact case this sub-query exists to catch — returns hundreds of in-window
// errors that are all the same error. Uncapped, one such group spends the entire row
// budget the tool renders and crowds out every other group's lines.
func TestMIGErrorsAreCappedSoOneLoopingGroupCannotFloodTheLens(t *testing.T) {
	var errs []*compute.InstanceManagedByIgmError
	for i := 0; i < defaultMaxEvents*8; i++ {
		errs = append(errs, stockoutError("2026-08-24T10:00:00Z"))
	}
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()), migErrorsRoute(errs...), migInstancesRoute())

	lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(lines)
	shown := strings.Count(out, "ZONE_RESOURCE_POOL_EXHAUSTED")
	if shown > defaultMaxEvents {
		t.Errorf("emitted %d error lines for one group, cap is %d", shown, defaultMaxEvents)
	}
	if !strings.Contains(out, "further errors in the window, not listed") {
		t.Errorf("the cap was applied silently, so a partial view reads as a complete one:\n%s", out)
	}
}

// TestAnIdleClusterSkipsThePerGroupInstanceListing pins the aggregated read.
//
// One aggregatedList answers "does any group have anything moving at all", which on a
// healthy cluster — the normal case — replaces one listManagedInstances round-trip per
// group with one round-trip total.
func TestAnIdleClusterSkipsThePerGroupInstanceListing(t *testing.T) {
	c, srv := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		migErrorsRoute(),
		migAggregatedRoute(map[string][]*compute.InstanceGroupManager{
			"zones/" + migZone: {idleManager(migShortName, 3)},
		}),
		migInstancesRoute(), // present, and must go unused
	)

	if _, err := c.ResourceHealth(context.Background(),
		providers.Selector{}, providers.TimeWindow{}); err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	if srv.requested("/listManagedInstances") {
		t.Error("a group the aggregated summary reported as idle was still listed per-group; " +
			"that is the round-trip the summary exists to avoid")
	}
}

// TestAnUnavailableChurnSummaryFallsBackRatherThanReportingHealth is the safety half of
// the optimisation above.
//
// An aggregatedList that fails — most plausibly a permission gap — must NOT be read as
// "nothing is churning". That would turn a missing binding into a clean bill of health,
// which is the one answer this lens must never invent. Here the aggregated route is
// absent, so it 404s and the per-group listing has to run.
func TestAnUnavailableChurnSummaryFallsBackRatherThanReportingHealth(t *testing.T) {
	c, srv := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		migErrorsRoute(),
		migInstancesRoute(&compute.ManagedInstance{
			Name: "gke-prod-default-abcd", CurrentAction: "RECREATING", InstanceStatus: "STOPPING",
		}),
	)

	lines, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	if !srv.requested("/listManagedInstances") {
		t.Error("the per-group listing was skipped on an unavailable summary, so churn behind a " +
			"permission gap reads as an idle cluster")
	}
	if out := joined(lines); !strings.Contains(out, "RECREATING") {
		t.Errorf("the churn was not reported:\n%s", out)
	}
}
