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
// makes an ABSENCE testable: "Autopilot skips the node-layer lookups" is a claim about
// calls that were not issued, and no assertion on the returned lines can distinguish a
// skipped lookup from one that ran and found nothing.
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

// TestResourceHealthOnAutopilotSkipsTheNodeLayerAndSaysSo is the degradation contract
// for Autopilot. An Autopilot cluster has no operator-owned node pools, so the
// instance-group and instance-status lookups have nothing an operator could act on —
// but returning silently would leave the model reading an empty node-capacity section
// as "capacity is fine", a false negative on exactly the question the lens exists to
// answer. The skip is therefore stated, not implied.
//
// The negative assertion on requested paths is the load-bearing half: the note alone
// could be emitted while the lookups still ran.
func TestResourceHealthOnAutopilotSkipsTheNodeLayerAndSaysSo(t *testing.T) {
	cluster := container.Cluster{
		Name:                 "prod",
		Location:             "europe-west1",
		Status:               "RUNNING",
		CurrentMasterVersion: "1.30.4-gke.100",
		Autopilot:            &container.Autopilot{Enabled: true},
	}
	c, srv := healthClient(t, healthIdentity(), clusterRoute(cluster))

	got, err := c.ResourceHealth(context.Background(), providers.Selector{Name: "gk3-prod-pool-1-abc"}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("ResourceHealth: %v", err)
	}
	out := joined(got)

	checks := []struct {
		name string
		want string
	}{
		{"the cluster is identified as Autopilot", "Autopilot"},
		{"the mode is on the cluster line, not only in the note", "mode=Autopilot"},
		{"the note says the node layer is Google-managed", "google-managed"},
		{"the note says the node-layer lookups were skipped", "skipped"},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(strings.ToLower(out), strings.ToLower(tc.want)) {
				t.Errorf("output does not contain %q:\n%s", tc.want, out)
			}
		})
	}

	t.Run("no instance-group lookup is issued", func(t *testing.T) {
		if srv.requested("instanceGroupManagers") {
			t.Errorf("Autopilot ran the instance-group lookup; paths: %v", srv.paths)
		}
	})
	t.Run("no instance lookup is issued even though the selector names one", func(t *testing.T) {
		if srv.requested("/instances") {
			t.Errorf("Autopilot ran the instance lookup; paths: %v", srv.paths)
		}
	})
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
