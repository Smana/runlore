// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	logging "google.golang.org/api/logging/v2"
	"google.golang.org/api/option"

	"github.com/Smana/runlore/internal/providers"
)

// TestTheGCPVocabularyIsCompleteEnoughToRender is the in-tree caller
// providers.CloudVocabulary.Validate was written for.
//
// Validate is deliberately never called at render time — a half-filled vocabulary is a
// coding mistake, and failing a live investigation over one is worse than the degraded
// sentence the renderers already produce. That trade only holds if something else
// catches the mistake, and "something else" is this test. Without it Validate is an
// exported guard with no user, which is how the defect it was written for ships anyway.
//
// The failure mode is silent by construction: an empty fragment does not panic, it
// renders. A vocabulary missing LagNote yields "since_minutes default 90 ()", and one
// missing ChangeExamples yields "events (Cloud Audit Logs) — , and other infra changes
// invisible to GitOps". Both reach a model as confident prose about what it just
// queried.
func TestTheGCPVocabularyIsCompleteEnoughToRender(t *testing.T) {
	if err := (Client{}).CloudVocabulary().Validate(); err != nil {
		t.Errorf("the GCP vocabulary is incomplete:\n%v", err)
	}
}

// awsNouns is a copy of the sweep in internal/investigate/cloud_tools_test.go, which
// cannot see this vocabulary: that test drives the tools through a hand-written GCP
// fixture, because internal/investigate does not — and to stay a layer above the
// providers must not — import a concrete cloud provider. The fixture proves the TOOLS
// consult a vocabulary; only a check here can prove THIS vocabulary is clean.
//
// Word boundaries, not strings.Contains, for the reason the original records: "ARN" is
// a substring of "WARNING", and this text uses capitals for emphasis throughout
// (MUTATING, REJECTED, OMIT, SUBSTRING). Boundaries are also what make the short,
// collision-prone names safe to list at all.
var awsNouns = regexp.MustCompile(`\b(AWS|CloudTrail|EC2|EKS|ASG|ARN|RDS|SG)\b`)

// TestTheGCPVocabularyNamesGCPAndNeverAWS sweeps every model-facing surface this
// vocabulary can produce, not just the two Description() renderers.
//
// The renderers were only ever the most visible third of the problem. The JSON-schema
// fragments (FailureFilterArg, InstanceArg) each carry AWS verbs and identifiers on the
// AWS side, and the three empty-result messages — the sentences a model is most likely
// to quote verbatim into a finding — name a cloud outright. An investigation that
// closes with "no mutating AWS events in the window" reads to the on-call as evidence,
// and on GKE it would be evidence of nothing.
//
// Each case also asserts a GCP noun it must CONTAIN. Absence-only assertions pass just
// as happily on the empty string, which is precisely what a vocabulary wired to the
// wrong field would render.
func TestTheGCPVocabularyNamesGCPAndNeverAWS(t *testing.T) {
	v := (Client{}).CloudVocabulary()

	tests := []struct {
		name string
		got  string
		want string // a GCP noun proving the fragment was actually filled in
	}{
		{
			name: "cloud_what_changed's description names Cloud Audit Logs",
			got:  v.ChangeDescription(),
			want: "Cloud Audit Logs",
		},
		{
			name: "cloud_resource_health's description names GKE node pools",
			got:  v.HealthDescription(),
			want: "GKE cluster and node-pool",
		},
		{
			name: "the dropped-scope banner explains GCP's substring match rule",
			got:  v.RenderWidenedBanner("guessed"),
			want: "SUBSTRING match on protoPayload.resourceName",
		},
		{
			name: "the failed_only schema description names GCP's read verbs",
			got:  v.FailureFilterArg,
			want: "Data Access audit logs are off by default",
		},
		{
			name: "the instance argument asks for a Compute Engine instance name",
			got:  v.InstanceArg,
			want: "Compute Engine instance name",
		},
		{
			name: "incident_timeline's cloud clause lists GCP services",
			got:  v.TimelineExamples,
			want: "GKE",
		},
		{
			name: "an empty changes lookup does not claim AWS was quiet",
			got:  v.EmptyChangesMessage(),
			want: "no mutating GCP events",
		},
		{
			name: "an empty failed_only lookup does not claim AWS had no failures",
			got:  v.EmptyFailedChangesMessage(),
			want: "no FAILED GCP control-plane calls",
		},
		{
			name: "an empty health lookup does not claim AWS returned nothing",
			got:  v.EmptyHealthMessage(),
			want: "no GCP resource health returned",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tt.got, tt.want) {
				t.Errorf("GCP wording %q missing:\n%s", tt.want, tt.got)
			}
			if leaked := awsNouns.FindAllString(tt.got, -1); leaked != nil {
				t.Errorf("still names %v on a GCP provider:\n%s", leaked, tt.got)
			}
		})
	}
}

// TestTheWidenedBannerQuotesTheResourceItDropped guards the one fragment that takes an
// argument. The banner only renders on the widen path — a scoped lookup that matched
// nothing followed by an unscoped retry that found something — which no smoke test
// reaches, so a banner that ignored its argument, or that named the wrong one, would
// ship unnoticed and tell the model its filter was dropped without saying which filter.
func TestTheWidenedBannerQuotesTheResourceItDropped(t *testing.T) {
	const resource = "gke-prod-pool-a1b2c3"
	got := (Client{}).CloudVocabulary().RenderWidenedBanner(resource)
	if !strings.Contains(got, fmt.Sprintf("%q", resource)) {
		t.Errorf("banner does not quote the dropped resource %q:\n%s", resource, got)
	}
	if strings.Contains(got, "%!") {
		t.Errorf("banner rendered a bad printf verb:\n%s", got)
	}
}

// TestNewRefusesAnIdentityWithoutAProject pins the one input that has no usable
// default. Every Cloud Logging, container and compute call this provider makes is
// addressed to "projects/<id>/…", so an empty project does not degrade — it builds a
// client whose every request 400s mid-investigation, at which point the real cause
// (identity resolution found nothing and nobody set cloud.gcp.project) is several
// layers away from the error the on-call reads.
func TestNewRefusesAnIdentityWithoutAProject(t *testing.T) {
	_, err := New(context.Background(), Identity{Location: "europe-west1", ClusterName: "c"})
	if err == nil {
		t.Fatal("New accepted an Identity with no project")
	}
	if !strings.Contains(err.Error(), "cloud.gcp.project") {
		t.Errorf("the error does not name the config key that fixes it: %v", err)
	}
}

// hostGuard records every request URL and refuses any that is not addressed to the
// test server.
//
// It exists because the failure this test is really hunting — New forgetting to thread
// opts into one of its three service constructors — does not look like a failure
// without it. That service keeps the generated default BasePath, so its request goes
// to the real googleapis.com: in a networked environment the test hangs on a live call
// and then fails on an auth error from a distant layer, and in a sandboxed one it fails
// with a DNS message that names neither the service nor the missing option. Refusing
// the request here turns all of that into one deterministic assertion, and guarantees a
// unit test cannot make an outbound call.
type hostGuard struct {
	base  http.RoundTripper
	allow string
	hosts []string
}

func (h *hostGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	h.hosts = append(h.hosts, req.URL.Host)
	if req.URL.Host != h.allow {
		return nil, fmt.Errorf("request escaped to %s: a service constructor did not receive the endpoint option", req.URL.Host)
	}
	return h.base.RoundTrip(req)
}

// TestNewPointsEveryServiceAtTheInjectedEndpoint proves the seam the rest of this
// package's tests are built on.
//
// The Google generated clients are structs, not interfaces, so there is no way to
// substitute a fake service: the only injection point is the ClientOption list, and the
// only thing that makes the later lenses testable is New passing that list to all three
// constructors. Nothing about a forgotten `opts...` on one of them shows up at compile
// time, and the two that were wired correctly would keep every neighbouring test green.
//
// Asserting one request per service rather than a total count is deliberate — three
// requests could equally be three logging calls.
func TestNewPointsEveryServiceAtTheInjectedEndpoint(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	guard := &hostGuard{base: srv.Client().Transport, allow: strings.TrimPrefix(srv.URL, "http://")}
	httpClient := &http.Client{Transport: guard}

	ctx := context.Background()
	c, err := New(ctx, Identity{Project: "my-proj", Location: "europe-west1-b", ClusterName: "my-cluster"},
		option.WithHTTPClient(httpClient),
		option.WithEndpoint(srv.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// One call per service, each through the same seam production uses.
	calls := []struct {
		service string
		call    func() error
		want    string // a path fragment identifying the service that was reached
	}{
		{
			service: "logging",
			call: func() error {
				_, err := c.entries.List(ctx, &logging.ListLogEntriesRequest{
					ResourceNames: []string{"projects/my-proj"},
				})
				return err
			},
			want: "entries:list",
		},
		{
			service: "container",
			call: func() error {
				_, err := c.container.Projects.Locations.Clusters.
					Get("projects/my-proj/locations/europe-west1-b/clusters/my-cluster").Context(ctx).Do()
				return err
			},
			want: "clusters/my-cluster",
		},
		{
			service: "compute",
			call: func() error {
				_, err := c.compute.Instances.Get("my-proj", "europe-west1-b", "an-instance").Context(ctx).Do()
				return err
			},
			want: "instances/an-instance",
		},
	}
	for _, tc := range calls {
		t.Run(tc.service+" is addressed to the injected endpoint", func(t *testing.T) {
			before := len(paths)
			if err := tc.call(); err != nil {
				t.Fatalf("%s call: %v", tc.service, err)
			}
			if len(paths) != before+1 {
				t.Fatalf("%s made %d requests to the test server, want 1", tc.service, len(paths)-before)
			}
			if got := paths[before]; !strings.Contains(got, tc.want) {
				t.Errorf("%s reached %q, which does not contain %q", tc.service, got, tc.want)
			}
		})
	}
	if len(guard.hosts) != len(calls) {
		t.Errorf("saw %d outbound requests %v, want %d", len(guard.hosts), guard.hosts, len(calls))
	}
}

// TestTheUnimplementedLensFailsLoudlyRatherThanReportingCalm asserts that the one
// CloudProvider method this package has not filled in yet returns an error instead of
// an empty result. CloudChanges has since been implemented in auditlog.go and is
// covered there; ResourceHealth is all that is left.
//
// Empty is the dangerous answer. cloud_resource_health renders no lines as "no GCP
// resource health returned" — a positive claim that the cloud was queried and found
// quiet, which a model repeats into a finding as evidence. An error, by contrast, is
// reported as a tool failure and the model is told nothing was established. Until the
// lens exists, that is the only honest answer.
func TestTheUnimplementedLensFailsLoudlyRatherThanReportingCalm(t *testing.T) {
	c := &Client{}
	if _, err := c.ResourceHealth(context.Background(), providers.Selector{}, providers.TimeWindow{}); !errors.Is(err, errNotImplemented) {
		t.Errorf("ResourceHealth returned %v, want errNotImplemented", err)
	}
}
