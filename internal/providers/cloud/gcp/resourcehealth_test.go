// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// route is one canned answer from the fake container/compute API: any request whose
// path contains frag gets it.
//
// An ordered slice rather than a map keyed by fragment, because map iteration order is
// randomised per run — two fragments that both matched a path would pick a different
// answer on different runs and produce a test that fails once every few CI runs for no
// visible reason.
type route struct {
	frag   string
	status int // 0 means 200 OK
	body   any // marshalled as the response body; typed structs only, never map[string]any
}

// gapiErrorBody is the shape Google APIs return on failure and the shape
// googleapi.CheckResponse parses to build the *googleapi.Error the provider inspects.
// Typed rather than a map because encoding/json sorts map keys, which no real API does.
type gapiErrorBody struct {
	Error gapiError `json:"error"`
}

type gapiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// healthServer records the path of every request the lens made. The recording is what
// makes "which lookups actually ran" testable at all: a lookup that ran and found
// nothing and a lookup that was never issued produce the same silence in the returned
// lines, and the difference between them is the difference between evidence and its
// absence.
type healthServer struct {
	*httptest.Server
	paths []string
}

func (s *healthServer) requested(frag string) bool {
	for _, p := range s.paths {
		if strings.Contains(p, frag) {
			return true
		}
	}
	return false
}

// healthClient wires a Client to a fake API serving routes. Any path no route claims
// gets a 404, matching what the real API does for a cluster that does not exist.
func healthClient(t *testing.T, id Identity, routes ...route) (*Client, *healthServer) {
	t.Helper()
	srv := &healthServer{}
	srv.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.paths = append(srv.paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		for _, rt := range routes {
			if !strings.Contains(r.URL.Path, rt.frag) {
				continue
			}
			body, err := json.Marshal(rt.body)
			if err != nil {
				t.Errorf("marshal route %q: %v", rt.frag, err)
			}
			if rt.status != 0 {
				w.WriteHeader(rt.status)
			}
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"not found","status":"NOT_FOUND"}}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(context.Background(), id,
		option.WithHTTPClient(srv.Client()),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

// healthIdentity is the resolved scope the health tests run against. Source is set
// because a real deployment always has one, and the tests that care about its absence
// override it explicitly.
func healthIdentity() Identity {
	return Identity{
		Project:       "my-proj",
		Location:      "europe-west1",
		ClusterName:   "prod",
		ProjectNumber: "123456789012",
		Source:        sourceMetadata,
	}
}

func joined(lines providers.LogResult) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// clusterRoute answers container.projects.locations.clusters.get. The path fragment is
// "/clusters/" because the generated client builds "v1/projects/P/locations/L/clusters/C".
func clusterRoute(cl container.Cluster) route {
	return route{frag: "/clusters/", body: cl}
}

// TestResourceHealthReportsClusterAndNodePoolState pins the Standard-cluster path: the
// cluster's own status and conditions, then every pool's status, conditions,
// autoscaling bounds and version skew against the control plane.
//
// Substring assertions rather than a whole-output comparison, and one sub-test per
// claim, because these lines come from four different parts of one response and a
// single golden string would report "output changed" without saying which of the four
// broke.
func TestResourceHealthReportsClusterAndNodePoolState(t *testing.T) {
	cluster := container.Cluster{
		Name:                 "prod",
		Location:             "europe-west1",
		Status:               "DEGRADED",
		StatusMessage:        "one or more node pools are degraded",
		CurrentMasterVersion: "1.30.4-gke.100",
		CurrentNodeCount:     7,
		Conditions: []*container.StatusCondition{{
			CanonicalCode: "RESOURCE_EXHAUSTED",
			Message:       "IP space exhausted in subnet gke-prod-subnet",
		}},
		NodePools: []*container.NodePool{
			{
				Name:          "default",
				Status:        "ERROR",
				StatusMessage: "Instance group is unhealthy",
				Version:       "1.28.9-gke.100",
				Autoscaling:   &container.NodePoolAutoscaling{Enabled: true, MinNodeCount: 1, MaxNodeCount: 10},
				Conditions: []*container.StatusCondition{{
					CanonicalCode: "RESOURCE_EXHAUSTED",
					Message:       "ZONE_RESOURCE_POOL_EXHAUSTED",
				}},
				InstanceGroupUrls: []string{
					"https://www.googleapis.com/compute/v1/projects/my-proj/zones/europe-west1-b/instanceGroupManagers/gke-prod-default-grp",
				},
			},
			{
				Name:        "spot",
				Status:      "RUNNING",
				Version:     "1.30.4-gke.100",
				Autoscaling: &container.NodePoolAutoscaling{Enabled: true, TotalMinNodeCount: 2, TotalMaxNodeCount: 30},
			},
			{
				Name:    "fixed",
				Status:  "RUNNING",
				Version: "1.30.4-gke.100",
			},
		},
	}
	c, _ := healthClient(t, healthIdentity(), clusterRoute(cluster))

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	present := []struct {
		name string
		want string
	}{
		{"the cluster's own status is reported", "GKE cluster prod"},
		{"the cluster status message is not dropped", "one or more node pools are degraded"},
		{"the resolved location is echoed, so a mis-autodetected scope is visible", "europe-west1"},
		{"the control-plane version is reported", "control-plane version=1.30.4-gke.100"},
		{"cluster-level conditions are surfaced", "IP space exhausted in subnet gke-prod-subnet"},
		{"a degraded pool is named with its status", "node pool default: status=ERROR"},
		{"the pool's status message is not dropped", "Instance group is unhealthy"},
		{"pool conditions are surfaced", "ZONE_RESOURCE_POOL_EXHAUSTED"},
		{"the skewed pool's own version is reported", "1.28.9-gke.100"},
		{"the skew is called out against the control-plane version", "SKEW"},
		{"per-zone autoscaling bounds are labelled per zone", "autoscaling=1..10 per zone"},
		{"pool-wide autoscaling bounds are labelled as totals", "autoscaling=2..30 pool total"},
		{"a fixed-size pool is reported as unable to scale", "autoscaling=off"},
		{"healthy pools are listed too, so the pool inventory is complete", "node pool spot"},
	}
	for _, tc := range present {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not contain %q:\n%s", tc.want, out)
			}
		})
	}

	t.Run("only the skewed pool is flagged, so an up-to-date pool is not false-positived", func(t *testing.T) {
		if n := strings.Count(out, "SKEW"); n != 1 {
			t.Errorf("got %d SKEW markers, want exactly 1:\n%s", n, out)
		}
	})
	t.Run("a Standard cluster carries no Autopilot note", func(t *testing.T) {
		if strings.Contains(out, "Autopilot") {
			t.Errorf("Standard cluster reported as Autopilot:\n%s", out)
		}
	})
	t.Run("an absent status message renders no empty parenthesis", func(t *testing.T) {
		if strings.Contains(out, "()") {
			t.Errorf("an absent field rendered as an empty () a reader cannot tell from a truncated one:\n%s", out)
		}
	})
}

// TestResourceHealthCapsTheQuietNodePoolsButNeverTheDegradedOne pins the listing bound.
// GKE allows hundreds of pools per cluster, so the inventory has to be capped — but a
// uniform cap would drop the one degraded pool behind the healthy majority, losing the
// row the lens exists to surface, and losing it silently.
func TestResourceHealthCapsTheQuietNodePoolsButNeverTheDegradedOne(t *testing.T) {
	const controlPlane = "1.30.4-gke.100"
	cluster := container.Cluster{
		Name: "prod", Location: "europe-west1", Status: "RUNNING",
		CurrentMasterVersion: controlPlane,
	}
	// The degraded pool is placed LAST, behind more healthy pools than the budget
	// allows, which is where a uniform cap would lose it.
	const quiet = 40
	for i := range quiet {
		cluster.NodePools = append(cluster.NodePools, &container.NodePool{
			Name: fmt.Sprintf("quiet-%02d", i), Status: "RUNNING", Version: controlPlane,
		})
	}
	cluster.NodePools = append(cluster.NodePools, &container.NodePool{
		Name: "broken", Status: "ERROR", StatusMessage: "Instance group is unhealthy", Version: controlPlane,
	})
	c, _ := healthClient(t, healthIdentity(), clusterRoute(cluster))

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	if !strings.Contains(out, "node pool broken: status=ERROR") {
		t.Errorf("the degraded pool was capped away:\n%s", out)
	}
	if n := strings.Count(out, "node pool quiet-"); int64(n) != defaultMaxEvents {
		t.Errorf("listed %d quiet pools, want the %d-pool budget", n, defaultMaxEvents)
	}
	if want := fmt.Sprintf("%d further node pools omitted", quiet-defaultMaxEvents); !strings.Contains(out, want) {
		t.Errorf("output does not contain %q, so the cap is silent:\n%s", want, out)
	}
}

// TestResourceHealthOnAutopilotStillQueriesTheNodeLayerAndCaveatsAnEmptyResult is the
// Autopilot contract. An earlier draft skipped the node-level sub-queries entirely on
// Autopilot, on the reasoning that the node layer is Google's. That is wrong in the way
// that matters: the node VMs and their instance groups live in the CUSTOMER project and
// are readable with the same roles/compute.viewer, and a zonal stockout strands
// Autopilot Pods as Pending exactly as it strands Standard ones. Skipping discarded the
// highest-value line this provider emits precisely where the operator has the least
// visibility of their own.
//
// What the skip got right is that an EMPTY node-layer answer must not read as "capacity
// is fine" — so the empty answer is hedged instead of not being sought.
//
// The recorded request paths are the load-bearing half: no assertion on the returned
// lines can tell a lookup that ran and found nothing from one that never ran.
func TestResourceHealthOnAutopilotStillQueriesTheNodeLayerAndCaveatsAnEmptyResult(t *testing.T) {
	c, srv := healthClient(t, healthIdentity(),
		clusterRoute(autopilotCluster()),
		migErrorsRoute(),
		migInstancesRoute(),
		// A real aggregatedList answers 200 with no items for a name that does not
		// exist, which is the "found nothing" path this test is about — a 404 would
		// exercise the transport-failure path instead.
		instancesRoute(),
	)

	got, err := c.ResourceHealth(context.Background(), providers.Selector{Name: "gk3-prod-pool-1-abc"}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	t.Run("the instance-group lookup is issued, because Autopilot's groups are in the customer project", func(t *testing.T) {
		if !srv.requested("instanceGroupManagers") {
			t.Errorf("Autopilot skipped the instance-group lookup; paths: %v", srv.paths)
		}
	})
	t.Run("the instance lookup is issued when the selector names one", func(t *testing.T) {
		if !srv.requested("/instances") {
			t.Errorf("Autopilot skipped the instance lookup; paths: %v", srv.paths)
		}
	})

	checks := []struct {
		name string
		want string
	}{
		{"the cluster is identified as Autopilot", "mode=Autopilot"},
		{"an empty instance-group answer is hedged, not reported as healthy", "limited visibility"},
		{"the hedge names Autopilot as the reason the answer may be partial", "autopilot cluster"},
		{"the hedge says the node layer is Google's", "google-managed"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(strings.ToLower(out), strings.ToLower(tc.want)) {
				t.Errorf("output does not contain %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestResourceHealthReportsAnAutopilotStockoutWithoutHedging is the other half of the
// Autopilot contract, and the reason the lookups run at all: when the node layer DOES
// answer with a capacity failure, that finding is reported plainly. Hedging a real
// stockout would bury the one line an operator with no node access cannot get any other
// way.
func TestResourceHealthReportsAnAutopilotStockoutWithoutHedging(t *testing.T) {
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(autopilotCluster()),
		migErrorsRoute(stockoutError("2026-08-24T10:00:00Z")),
		migInstancesRoute(),
	)

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	for _, want := range []string{migShortName, "ZONE_RESOURCE_POOL_EXHAUSTED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(strings.ToLower(out), "limited visibility") {
		t.Errorf("a real Autopilot stockout was hedged as a visibility limit:\n%s", out)
	}
}

// TestResourceHealthDegradesWhenTheClusterLookupFails pins the per-sub-query contract
// shared with the AWS provider (cloud/aws/resourcehealth.go:25): a failing sub-query
// contributes a line, it does not fail the call, so the sub-queries that did succeed
// still reach the investigation.
func TestResourceHealthDegradesWhenTheClusterLookupFails(t *testing.T) {
	c, _ := healthClient(t, healthIdentity(), route{
		frag:   "/clusters/",
		status: http.StatusServiceUnavailable,
		body:   gapiErrorBody{Error: gapiError{Code: 503, Message: "backend unavailable", Status: "UNAVAILABLE"}},
	})

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth must not hard-fail on a sub-query error, got: %v", err)
	}
	// Empty is the dangerous answer, which is why the degraded path still emits a line:
	// cloud_resource_health renders zero lines as "no GCP resource health returned", a
	// positive claim that the cloud was queried and found quiet. A model repeats that
	// into a finding as evidence; an error line establishes nothing, which is correct.
	if len(got) == 0 {
		t.Fatal("a failed sub-query returned no lines, which renders as a quiet cloud")
	}
	out := joined(got)
	if !strings.Contains(out, "gke cluster prod") {
		t.Errorf("no error line naming the failed cluster lookup:\n%s", out)
	}
	if !strings.Contains(out, "backend unavailable") {
		t.Errorf("the underlying error is not reported, leaving nothing to diagnose:\n%s", out)
	}
}

// TestResourceHealthTellsAPermissionDenialApartFromAWrongScope is the one place this
// provider goes beyond the AWS error shape. A 403 on clusters.get is a missing
// container.clusterViewer binding; a 404 is a cluster name or location that does not
// exist, which on a zero-config deployment means identity autodetection picked wrong.
// Collapsing the two sends the operator to the IAM console for a metadata problem.
func TestResourceHealthTellsAPermissionDenialApartFromAWrongScope(t *testing.T) {
	tests := []struct {
		name       string
		id         Identity
		status     int
		apiMessage string
		want       []string
		absent     []string
	}{
		{
			name:       "a 403 names the role to grant and the project number an IAM principal binding needs",
			id:         healthIdentity(),
			status:     http.StatusForbidden,
			apiMessage: "Required \"container.clusters.get\" permission",
			want:       []string{"permission denied", "roles/container.clusterViewer", "123456789012"},
			absent:     []string{"cloud.gcp.cluster_name"},
		},
		{
			name:       "a 404 points at the resolved scope and the keys that override it, not at IAM",
			id:         healthIdentity(),
			status:     http.StatusNotFound,
			apiMessage: "Not found: projects/my-proj/locations/europe-west1/clusters/prod",
			want: []string{
				"not found", `cluster "prod"`, `location "europe-west1"`,
				"cloud.gcp.cluster_name", "cloud.gcp.location", sourceMetadata,
			},
			absent: []string{"roles/container.clusterViewer", "permission denied"},
		},
		{
			// The dangling-suffix defect this guards against is the one renderCall in
			// auditlog.go guards separately: an unconditional " by " + principalEmail
			// would render "hostError by " on every Google-initiated event, claiming a
			// caller whose identity was lost rather than no caller at all.
			name: "an unknown resolution tier omits the clause rather than trailing off",
			id: Identity{
				Project: "my-proj", Location: "europe-west1", ClusterName: "prod",
			},
			status:     http.StatusNotFound,
			apiMessage: "Not found",
			want:       []string{"not found"},
			absent:     []string{"resolved from"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := healthClient(t, tc.id, route{
				frag:   "/clusters/",
				status: tc.status,
				body: gapiErrorBody{Error: gapiError{
					Code: tc.status, Message: tc.apiMessage, Status: "ERROR",
				}},
			})
			got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("ResourceHealth: %v", err)
			}
			out := joined(got)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("output wrongly contains %q:\n%s", absent, out)
				}
			}
		})
	}
}

// TestResourceHealthReportsAnUnresolvedScopeWithoutCallingTheAPI covers the case that
// reaches this lens when autodetection found a project but no cluster. A get against an
// empty cluster name is a guaranteed 404 whose message would blame the API, so the
// lookup is not attempted and the line names the config keys instead.
func TestResourceHealthReportsAnUnresolvedScopeWithoutCallingTheAPI(t *testing.T) {
	c, srv := healthClient(t, Identity{Project: "my-proj", Source: sourceNone})

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)
	for _, want := range []string{"cloud.gcp.cluster_name", "cloud.gcp.location"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not name %q:\n%s", want, out)
		}
	}
	if len(srv.paths) != 0 {
		t.Errorf("a request was issued against an unresolved scope: %v", srv.paths)
	}
}

// The instance group every node-layer test is addressed to. Written as a full self-link
// because that is what container.NodePool.instanceGroupUrls carries, and parsing it is
// what decides which compute endpoint the lookup goes to.
const (
	migSelfLink = "https://www.googleapis.com/compute/v1/projects/my-proj/zones/" +
		"europe-west1-b/instanceGroupManagers/gke-prod-default-grp"
	migShortName = "gke-prod-default-grp"
	migZone      = "europe-west1-b"
)

// standardCluster is a healthy Standard cluster with one pool backed by migSelfLink, so
// a node-layer test can be about the node layer rather than about cluster rendering.
func standardCluster() container.Cluster {
	return container.Cluster{
		Name: "prod", Location: "europe-west1", Status: "RUNNING",
		CurrentMasterVersion: "1.30.4-gke.100",
		NodePools: []*container.NodePool{{
			Name: "default", Status: "RUNNING", Version: "1.30.4-gke.100",
			InstanceGroupUrls: []string{migSelfLink},
		}},
	}
}

// autopilotCluster carries a node pool for the same reason a real one does: Autopilot
// still runs node VMs in the customer's project and still reports the pools and
// instance groups holding them through the container API. A fixture with no pools would
// make "the lookups still run on Autopilot" untestable by accident.
func autopilotCluster() container.Cluster {
	cl := standardCluster()
	cl.Autopilot = &container.Autopilot{Enabled: true}
	cl.NodePools[0].Name = "pool-1"
	return cl
}

func migErrorsRoute(errs ...*compute.InstanceManagedByIgmError) route {
	return route{frag: "/listErrors", body: compute.InstanceGroupManagersListErrorsResponse{Items: errs}}
}

func migInstancesRoute(insts ...*compute.ManagedInstance) route {
	return route{
		frag: "/listManagedInstances",
		body: compute.InstanceGroupManagersListManagedInstancesResponse{ManagedInstances: insts},
	}
}

// instancesRoute answers compute.instances.aggregatedList. The zone key is the real
// "zones/<zone>" scope form, because the zone the lens reports comes out of it.
func instancesRoute(insts ...*compute.Instance) route {
	return route{
		frag: "/aggregated/instances",
		body: compute.InstanceAggregatedList{Items: map[string]compute.InstancesScopedList{
			"zones/" + migZone: {Instances: insts},
		}},
	}
}

func stockoutError(ts string) *compute.InstanceManagedByIgmError {
	return &compute.InstanceManagedByIgmError{
		Timestamp: ts,
		Error: &compute.InstanceManagedByIgmErrorManagedInstanceError{
			Code:    "ZONE_RESOURCE_POOL_EXHAUSTED",
			Message: "The zone 'europe-west1-b' does not have enough resources available.",
		},
	}
}

// TestResourceHealthSurfacesMIGStockouts covers the highest-value line this provider
// emits. A stockout, an exhausted quota or an exhausted IP range stops a node pool
// scaling with no in-cluster symptom other than Pods that stay Pending: the nodes
// simply never arrive, and no Kubernetes object records why. listErrors is the only
// place the reason is written down.
func TestResourceHealthSurfacesMIGStockouts(t *testing.T) {
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		migErrorsRoute(stockoutError("2026-08-24T10:00:00Z")),
		migInstancesRoute(),
	)

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	want := []struct {
		name string
		want string
	}{
		{"the failing group is named, so the operator knows which pool stopped scaling", migShortName},
		{"the error code is reported, because it is what classifies the failure", "ZONE_RESOURCE_POOL_EXHAUSTED"},
		{"the API's own message is kept", "does not have enough resources"},
		{"the zone is reported, since a stockout is zonal and the fix is usually another zone", migZone},
		{"the error is dated, so it can be lined up against the incident", "2026-08-24T10:00:00Z"},
	}
	for _, tc := range want {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(out, tc.want) {
				t.Errorf("output does not mention %q:\n%s", tc.want, out)
			}
		})
	}
}

// TestResourceHealthScopesMIGErrorsToTheIncidentWindow mirrors activityBeforeWindow on
// the AWS side (cloud/aws/resourcehealth.go). A group keeps its errors for hours, so an
// unscoped read hands the model last night's stockout while today's incident is a
// config change — a wrong cause that reads as a well-evidenced one.
func TestResourceHealthScopesMIGErrorsToTheIncidentWindow(t *testing.T) {
	inside := stockoutError("2026-08-24T10:00:00Z")
	before := &compute.InstanceManagedByIgmError{
		Timestamp: "2026-08-23T22:00:00Z",
		Error:     &compute.InstanceManagedByIgmErrorManagedInstanceError{Code: "STALE_LAST_NIGHT"},
	}
	after := &compute.InstanceManagedByIgmError{
		Timestamp: "2026-08-24T23:00:00Z",
		Error:     &compute.InstanceManagedByIgmErrorManagedInstanceError{Code: "AFTER_THE_WINDOW"},
	}
	// An error the API dated in a form this lens cannot parse is KEPT: dropping it
	// would hide a real capacity failure over a format quirk, and the timestamp is
	// printed alongside so a reader can judge it.
	undated := &compute.InstanceManagedByIgmError{
		Error: &compute.InstanceManagedByIgmErrorManagedInstanceError{Code: "UNDATED_KEPT"},
	}
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		migErrorsRoute(before, inside, after, undated),
		migInstancesRoute(),
	)

	w := providers.TimeWindow{
		Start: time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC),
	}
	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, w)
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	for _, want := range []string{"ZONE_RESOURCE_POOL_EXHAUSTED", "UNDATED_KEPT"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"STALE_LAST_NIGHT", "AFTER_THE_WINDOW"} {
		if strings.Contains(out, absent) {
			t.Errorf("an error outside the window was reported as current context (%q):\n%s", absent, out)
		}
	}
}

// TestResourceHealthCapsInstanceChurnSoOnePoolCannotFloodTheLens pins the churn bound.
// A pool caught in a create-fail-recreate loop can hold hundreds of instances mid-action
// at once, and listing all of them would spend a model's whole context on one symptom —
// crowding out the listErrors line that says WHY they keep failing.
func TestResourceHealthCapsInstanceChurnSoOnePoolCannotFloodTheLens(t *testing.T) {
	const churning = 40
	var insts []*compute.ManagedInstance
	for i := range churning {
		insts = append(insts, &compute.ManagedInstance{
			Name:           fmt.Sprintf("churn-%02d", i),
			CurrentAction:  "RECREATING",
			InstanceStatus: "PROVISIONING",
		})
	}
	// A steady instance is never a churn line, so the cap is spent on the instances
	// that are actually moving.
	insts = append(insts, &compute.ManagedInstance{
		Name: "steady-00", CurrentAction: "NONE", InstanceStatus: "RUNNING",
	})
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		migErrorsRoute(),
		migInstancesRoute(insts...),
	)

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	if n := strings.Count(out, "instance churn-"); int64(n) != defaultMaxEvents {
		t.Errorf("listed %d churning instances, want the %d-instance budget:\n%s", n, defaultMaxEvents, out)
	}
	if want := fmt.Sprintf("%d further", churning-defaultMaxEvents); !strings.Contains(out, want) {
		t.Errorf("output does not contain %q, so the cap is silent:\n%s", want, out)
	}
	if strings.Contains(out, "steady-00") {
		t.Errorf("a steady instance was reported as churn, spending the budget on nothing:\n%s", out)
	}
	if !strings.Contains(out, "RECREATING") {
		t.Errorf("the churn action is not reported, leaving nothing to interpret:\n%s", out)
	}
}

// TestResourceHealthReportsTheSelectedInstancesState covers the third sub-query. The
// question it answers is the one a node object cannot: a node gone NotReady because its
// VM is TERMINATED and a node gone NotReady with a healthy VM under it are the same
// symptom in Kubernetes and completely different incidents.
func TestResourceHealthReportsTheSelectedInstancesState(t *testing.T) {
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		migErrorsRoute(),
		migInstancesRoute(),
		instancesRoute(&compute.Instance{
			Name:               "gke-prod-default-grp-abcd",
			Status:             "TERMINATED",
			StatusMessage:      "Instance terminated by Compute Engine",
			LastStartTimestamp: "2026-08-24T08:11:00Z",
			Zone:               "https://www.googleapis.com/compute/v1/projects/my-proj/zones/" + migZone,
		}),
	)

	got, err := c.ResourceHealth(context.Background(),
		providers.Selector{Name: "gke-prod-default-grp-abcd"}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	for _, want := range []string{
		"gke-prod-default-grp-abcd", "TERMINATED",
		"Instance terminated by Compute Engine", "2026-08-24T08:11:00Z", migZone,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "()") {
		t.Errorf("an absent field rendered as an empty ():\n%s", out)
	}
}

// TestResourceHealthDegradesWhenANodeLayerLookupFails extends the per-sub-query
// contract (cloud/aws/resourcehealth.go:25) to the compute half. The two halves need
// DIFFERENT IAM roles — container.clusterViewer and compute.viewer — so a deployment
// granted one and not the other is an ordinary state, not a misconfiguration, and
// failing the call would throw away the half that answered.
func TestResourceHealthDegradesWhenANodeLayerLookupFails(t *testing.T) {
	c, _ := healthClient(t, healthIdentity(),
		clusterRoute(standardCluster()),
		route{
			frag:   "instanceGroupManagers",
			status: http.StatusServiceUnavailable,
			body: gapiErrorBody{Error: gapiError{
				Code: 503, Message: "backend unavailable", Status: "UNAVAILABLE",
			}},
		},
	)

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth must not hard-fail on a sub-query error, got: %v", err)
	}
	out := joined(got)

	if !strings.Contains(out, "GKE cluster prod") {
		t.Errorf("the cluster half was lost with the compute half:\n%s", out)
	}
	if !strings.Contains(out, migShortName) {
		t.Errorf("no error line naming the group whose lookup failed:\n%s", out)
	}
	if !strings.Contains(out, "backend unavailable") {
		t.Errorf("the underlying error is not reported, leaving nothing to diagnose:\n%s", out)
	}
	// The dangerous rendering is not the error but the reassurance: a failed lookup
	// that also prints "no errors" invites the model to conclude capacity was checked.
	if strings.Contains(out, "no errors") {
		t.Errorf("a failed lookup was also reported as finding no errors:\n%s", out)
	}
}

// TestResourceHealthTellsAComputeDenialApartFromAMissingGroup extends the 403/404 split
// T7 established for clusters.get to the compute calls, for the same reason: a 403 is a
// missing roles/compute.viewer binding, a 404 is a group that is not there — usually a
// pool deleted mid-incident. Reporting the second as a denial sends the operator to the
// IAM console, where every binding they add will fail to fix it.
func TestResourceHealthTellsAComputeDenialApartFromAMissingGroup(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		apiMessage string
		want       []string
		absent     []string
	}{
		{
			name:       "a 403 names the compute role to grant and the project number an IAM binding needs",
			status:     http.StatusForbidden,
			apiMessage: "Required 'compute.instanceGroupManagers.list' permission",
			want:       []string{"permission denied", "roles/compute.viewer", "123456789012"},
			absent:     []string{"not found"},
		},
		{
			name:       "a 404 names the group and its zone rather than blaming IAM",
			status:     http.StatusNotFound,
			apiMessage: "The resource 'gke-prod-default-grp' was not found",
			want:       []string{"not found", migShortName, migZone},
			absent:     []string{"roles/compute.viewer", "permission denied"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := healthClient(t, healthIdentity(),
				clusterRoute(standardCluster()),
				route{
					frag:   "instanceGroupManagers",
					status: tc.status,
					body: gapiErrorBody{Error: gapiError{
						Code: tc.status, Message: tc.apiMessage, Status: "ERROR",
					}},
				},
			)
			got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("ResourceHealth: %v", err)
			}
			out := joined(got)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output does not contain %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("output wrongly contains %q:\n%s", absent, out)
				}
			}
		})
	}
}

// TestResourceHealthClaimsNothingAboutCapacityWhenTheClusterLookupFailed guards the
// handoff between the two halves of this lens. A failed cluster read hands the node
// half no groups, which is indistinguishable in shape from a cluster that has none —
// and a "no instance-group errors" line under a cluster error would answer a question
// nothing asked, with a reassurance nothing established.
func TestResourceHealthClaimsNothingAboutCapacityWhenTheClusterLookupFailed(t *testing.T) {
	c, srv := healthClient(t, healthIdentity(), route{
		frag:   "/clusters/",
		status: http.StatusServiceUnavailable,
		body:   gapiErrorBody{Error: gapiError{Code: 503, Message: "backend unavailable", Status: "UNAVAILABLE"}},
	})

	got, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	if len(got) != 1 {
		t.Errorf("want exactly the cluster error line, got %d lines:\n%s", len(got), out)
	}
	for _, absent := range []string{"no errors", "instance group", "MIG", "Autopilot"} {
		if strings.Contains(out, absent) {
			t.Errorf("output claims something about the node layer (%q) that was never read:\n%s", absent, out)
		}
	}
	if srv.requested("instanceGroupManagers") {
		t.Errorf("a group lookup was issued with no groups to look up: %v", srv.paths)
	}
}

// TestMIGNameParsesZonalAndRegionalSelfLinks pins the parse that decides which compute
// endpoint a group lookup goes to. Sending a zonal group to the regional endpoint 404s,
// and a 404 in this lens renders as "that group is gone" — a silent false negative on
// the sub-query whose whole purpose is to catch capacity failures.
func TestMIGNameParsesZonalAndRegionalSelfLinks(t *testing.T) {
	const base = "https://www.googleapis.com/compute/v1/projects/p"
	cases := []struct {
		name  string
		in    string
		scope string
		mig   string
		zonal bool
		ok    bool
	}{
		{
			name:  "a zonal manager self-link yields its zone",
			in:    base + "/zones/europe-west1-b/instanceGroupManagers/g",
			scope: "europe-west1-b", mig: "g", zonal: true, ok: true,
		},
		{
			name:  "a regional manager self-link yields its region",
			in:    base + "/regions/europe-west1/instanceGroupManagers/g",
			scope: "europe-west1", mig: "g", zonal: false, ok: true,
		},
		{
			// The container API has published both spellings for the same field. A
			// zonal instance group and the manager that owns it share a name, so the
			// unmanaged spelling resolves to the same manager rather than being
			// dropped as unparseable — dropping it would silently skip the group.
			name:  "an unmanaged instance-group self-link resolves to the manager of the same name",
			in:    base + "/zones/europe-west1-b/instanceGroups/g",
			scope: "europe-west1-b", mig: "g", zonal: true, ok: true,
		},
		{
			name: "a URL naming no group is rejected rather than guessed at",
			in:   "https://example.com/nope",
		},
		{
			name: "a group in neither a zone nor a region is rejected, since there is no endpoint for it",
			in:   base + "/instanceGroupManagers/g",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope, mig, zonal, ok := migName(tc.in)
			if ok != tc.ok || scope != tc.scope || mig != tc.mig || (ok && zonal != tc.zonal) {
				t.Errorf("migName(%q) = (%q,%q,%v,%v), want (%q,%q,%v,%v)",
					tc.in, scope, mig, zonal, ok, tc.scope, tc.mig, tc.zonal, tc.ok)
			}
		})
	}
}
