// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

type fakeCloud struct {
	changes []providers.Change
	health  providers.LogResult
}

func (f fakeCloud) CloudChanges(context.Context, providers.Selector, providers.TimeWindow, providers.CloudChangeFilter) ([]providers.Change, error) {
	return f.changes, nil
}
func (f fakeCloud) ResourceHealth(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return f.health, nil
}

func TestCloudWhatChangedTool(t *testing.T) {
	tool := CloudWhatChangedTool{Cloud: fakeCloud{changes: []providers.Change{{
		When: time.Unix(1700000000, 0).UTC(), ManagedBy: "autoscaling.amazonaws.com",
		Workload: providers.Workload{Kind: "AWS::EC2::Instance", Name: "i-0abc"},
		Source:   providers.SourceRef{Path: "TerminateInstanceInAutoScalingGroup by karpenter"},
	}}}}
	out, err := tool.Call(context.Background(), `{"resource":"i-0abc"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"autoscaling.amazonaws.com", "i-0abc", "TerminateInstanceInAutoScalingGroup by karpenter"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestCloudResourceHealthTool(t *testing.T) {
	tool := CloudResourceHealthTool{Cloud: fakeCloud{health: providers.LogResult{
		{Message: "EKS nodegroup default: status=DEGRADED health=[AsgInstanceLaunchFailures: ...]"},
	}}}
	out, err := tool.Call(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Fatalf("unexpected output: %s", out)
	}
	// Empty result → friendly message.
	empty, _ := CloudResourceHealthTool{Cloud: fakeCloud{}}.Call(context.Background(), `{}`)
	if !strings.Contains(empty, "no AWS resource health") {
		t.Fatalf("expected empty message, got %s", empty)
	}
}

// exactMatchCloud models CloudTrail's real LookupEvents semantics: a ResourceName
// attribute is an EXACT match, so any scope other than the resource's full name
// returns nothing, while an unscoped lookup returns every mutating event.
type exactMatchCloud struct {
	resourceName string             // the only scope that matches
	changes      []providers.Change // returned when the scope matches, or unscoped
	scopedCalls  []string           // every Selector.Name the tool asked for, in order
	filters      []providers.CloudChangeFilter
}

func (f *exactMatchCloud) CloudChanges(_ context.Context, sel providers.Selector, _ providers.TimeWindow, f2 providers.CloudChangeFilter) ([]providers.Change, error) {
	f.scopedCalls = append(f.scopedCalls, sel.Name)
	f.filters = append(f.filters, f2)
	if sel.Name == "" || sel.Name == f.resourceName {
		return f.changes, nil
	}
	return nil, nil
}
func (f *exactMatchCloud) ResourceHealth(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return nil, nil
}

// TestCloudWhatChangedWidensOnScopedMiss pins the fallback.
//
// A guessed resource scope returns zero events, which is indistinguishable from "the
// control plane was quiet" — so the model concludes nothing changed. That happened on
// a real incident: an AWS Secrets Manager secret was deleted, CloudTrail recorded it
// under ResourceName "apps/app-wizard/llm", and the investigation tried
// "secretsmanager", "secretsmanager.amazonaws.com" and "app-wizard". All three missed,
// and it reported the deletion as uncapturable while the answer sat one unscoped
// lookup away.
func TestCloudWhatChangedWidensOnScopedMiss(t *testing.T) {
	deletion := providers.Change{
		When: time.Unix(1700000000, 0).UTC(), ManagedBy: "secretsmanager.amazonaws.com",
		Workload: providers.Workload{Kind: "secretsmanager.amazonaws.com", Name: "DeleteSecret"},
		Source:   providers.SourceRef{Path: "DeleteSecret by smana"},
	}

	t.Run("scoped miss falls back to unscoped and says so", func(t *testing.T) {
		cloud := &exactMatchCloud{resourceName: "apps/app-wizard/llm", changes: []providers.Change{deletion}}
		out, err := (CloudWhatChangedTool{Cloud: cloud}).Call(context.Background(), `{"resource":"secretsmanager"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "DeleteSecret") {
			t.Errorf("the deletion event must survive a bad resource scope:\n%s", out)
		}
		if !strings.Contains(out, "exact match") {
			t.Errorf("the model must be told WHY its filter missed, or it will guess again:\n%s", out)
		}
		if len(cloud.scopedCalls) != 2 || cloud.scopedCalls[0] != "secretsmanager" || cloud.scopedCalls[1] != "" {
			t.Errorf("want a scoped call then an unscoped retry, got %q", cloud.scopedCalls)
		}
	})

	t.Run("a matching scope is not widened", func(t *testing.T) {
		cloud := &exactMatchCloud{resourceName: "apps/app-wizard/llm", changes: []providers.Change{deletion}}
		out, err := (CloudWhatChangedTool{Cloud: cloud}).Call(context.Background(), `{"resource":"apps/app-wizard/llm"}`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "exact match") {
			t.Errorf("a scope that matched must not be reported as widened:\n%s", out)
		}
		if len(cloud.scopedCalls) != 1 {
			t.Errorf("a matching scope must not trigger a second lookup, got %q", cloud.scopedCalls)
		}
	})

	t.Run("genuinely quiet window still reports quiet", func(t *testing.T) {
		cloud := &exactMatchCloud{resourceName: "apps/app-wizard/llm"} // no changes at all
		out, err := (CloudWhatChangedTool{Cloud: cloud}).Call(context.Background(), `{"resource":"whatever"}`)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "no mutating AWS events") {
			t.Errorf("an empty window must not be dressed up as a widened result:\n%s", out)
		}
	})
}

// TestCloudWhatChangedFailedOnly: the flag reaches the provider, and it SURVIVES the
// unscoped widen. Without that, a scoped failure lookup that misses would widen into
// an unfiltered cluster-wide query and bury the rejected call under exactly the
// churn the flag exists to skip past — the widen would undo the fix.
func TestCloudWhatChangedFailedOnly(t *testing.T) {
	failed := providers.Change{
		When: time.Unix(1700000000, 0).UTC(), ManagedBy: "rds.amazonaws.com",
		Workload: providers.Workload{Kind: "AWS::RDS::DBCluster", Name: "aurora-serverless-postgres-old"},
		Source:   providers.SourceRef{Path: "CreateDBClusterSnapshot by backup — FAILED: InvalidDBClusterStateFault"},
	}
	cloud := &exactMatchCloud{resourceName: "aurora-serverless-postgres-old", changes: []providers.Change{failed}}
	out, err := (CloudWhatChangedTool{Cloud: cloud}).Call(context.Background(),
		`{"resource":"aurora-serverless-postgres","failed_only":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "InvalidDBClusterStateFault") {
		t.Errorf("the error code must reach the model:\n%s", out)
	}
	if len(cloud.filters) != 2 {
		t.Fatalf("want a scoped call then an unscoped retry, got %d", len(cloud.filters))
	}
	for i, f := range cloud.filters {
		if !f.FailedOnly {
			t.Errorf("call %d dropped FailedOnly: %+v", i, f)
		}
	}
	// The widen banner must not lecture about exact-match names here. Under
	// failed_only a scoped miss means "this resource had no REJECTED calls", which is
	// no evidence the name was wrong — and sending the model off to invent new names
	// is the dead end the widen exists to break, not to cause.
	if strings.Contains(out, "not a service or substring") {
		t.Errorf("failed_only widen misdiagnosed a correct name as an exact-match miss:\n%s", out)
	}
	if !strings.Contains(out, "may belong to OTHER resources") {
		t.Errorf("a widened failure lookup must warn whose failures it is showing:\n%s", out)
	}
}

// TestCloudWhatChangedFailedOnlyEmptyIsNotSilence: an empty FILTERED result must not
// read as "the control plane was quiet". submit_findings tells the model that an
// empty result is not evidence of absence; the tool has to hold up its end.
func TestCloudWhatChangedFailedOnlyEmptyIsNotSilence(t *testing.T) {
	out, err := (CloudWhatChangedTool{Cloud: &exactMatchCloud{}}).Call(context.Background(), `{"failed_only":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "FAILED") || !strings.Contains(out, "failed_only") {
		t.Errorf("an empty filtered result must name the filter that produced it:\n%s", out)
	}
}

// boundedScanCloud returns ONLY the provider's bounded-scan sentinel — the shape a
// failure scan produces after spending its page budget on a busy cluster with no
// failures in the window.
type boundedScanCloud struct{ calls int }

func (f *boundedScanCloud) CloudChanges(_ context.Context, _ providers.Selector, _ providers.TimeWindow, _ providers.CloudChangeFilter) ([]providers.Change, error) {
	f.calls++
	return []providers.Change{{
		Workload: providers.Workload{Kind: "(truncated)",
			Name: "scan stopped after 20 pages of successful events; any FAILED calls older than that were not examined — narrow the window's END, or scope to a resource"},
	}}, nil
}
func (f *boundedScanCloud) ResourceHealth(context.Context, providers.Selector, providers.TimeWindow) (providers.LogResult, error) {
	return nil, nil
}

// TestCloudWhatChangedBoundedScanIsNotAnEvent: a sentinel-only result is not a
// result. It is not empty either, so testing len(changes) == 0 rendered the sentinel
// as if it were the answer — one bogus row, no failures, and neither the widen nor
// the no-failures message ran. On the busy cluster failed_only was written for, that
// is the NORMAL outcome.
func TestCloudWhatChangedBoundedScanIsNotAnEvent(t *testing.T) {
	cloud := &boundedScanCloud{}
	out, err := (CloudWhatChangedTool{Cloud: cloud}).Call(context.Background(),
		`{"resource":"aurora-prod","failed_only":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Errorf("the sentinel was rendered as an event row:\n%s", out)
	}
	if !strings.Contains(out, "no FAILED AWS control-plane calls") {
		t.Errorf("a sentinel-only result must report as no failures found:\n%s", out)
	}
	// It must also NOT claim the window was quiet — the scan stopped reading.
	if !strings.Contains(out, "scan stopped after") {
		t.Errorf("a bounded scan must carry its own note rather than claiming absence:\n%s", out)
	}
	// And it must not spend a second 20-page budget on the widen.
	if cloud.calls != 1 {
		t.Errorf("a bounded scoped scan must not widen (40 pages against a ~2 TPS API): %d calls", cloud.calls)
	}
}

// describing wraps any CloudProvider with a vocabulary, turning it into a
// providers.CloudDescriber. It wraps rather than reimplements so the fakes above —
// which model provider BEHAVIOUR (exact-match scoping, bounded scans) — can be reused
// to test WORDING, instead of growing a parallel set of describing fakes whose
// behaviour would then have to be kept in step with theirs.
type describing struct {
	providers.CloudProvider
	vocab providers.CloudVocabulary
}

func (d describing) CloudVocabulary() providers.CloudVocabulary { return d.vocab }

// gcpish is a plausible GCP vocabulary. It is not the real one — that arrives with
// the GCP provider — but it names a different audit log, a different scope-match rule
// and a different instance identifier, which is everything a wording test needs.
func gcpish() providers.CloudVocabulary {
	return providers.CloudVocabulary{
		Cloud:            "GCP",
		AuditLog:         "Cloud Audit Logs",
		ChangeExamples:   "GKE/Compute/IAM changes, manual actions",
		TimelineExamples: "GKE/Compute/IAM actions",
		ScopeGuidance: "Optional resource is a SUBSTRING match on protoPayload.resourceName — a full " +
			"resource path or any part of one;",
		FailureFilterNote: "Set failed_only=true when the incident IS a rejected GCP API call and you do " +
			"not know which resource it happened to.",
		FailureFilterArg: "keep only MUTATING control-plane calls that were REJECTED, reporting each " +
			"error code; use when the incident is itself a rejected GCP write. Data Access audit logs " +
			"are off by default, so a denied read will NOT appear here",
		WidenedBanner: func(resource string) string {
			return fmt.Sprintf("resource %q matched no Cloud Audit Log entries in the window. Showing "+
				"ALL mutating entries instead:\n", resource)
		},
		LagNote:       "Cloud Audit Logs lag well under a minute",
		HealthSurface: "GKE cluster and node-pool conditions, and MIG scaling activity.",
		InstanceArg:   "optional Compute Engine instance name",
	}
}

// awsNouns are the words that must never reach a model on a non-AWS deployment.
// "EC2"/"EKS" are listed separately from "AWS" because the two Description() methods
// name AWS services without ever writing "AWS": a check for the brand alone passes
// while the health lens still advertises EKS nodegroups on a GKE cluster.
var awsNouns = []string{"AWS", "CloudTrail", "EC2", "EKS", "ASG", "ARN"}

// TestCloudToolsDescribeTheCloudTheyActuallyQuery is the point of the whole
// CloudDescriber exercise: a GCP-backed tool must never tell the model it is reading
// CloudTrail.
//
// It sweeps every model-facing surface rather than the two Description() methods,
// because those were only ever the most visible third of the problem. The JSON
// schemas each carry an argument description with AWS nouns in it, and the
// empty-result strings — the sentences a model is most likely to quote verbatim into
// a finding — carried them too. A test that checked Description() alone would have
// passed with most of the defect still shipping.
func TestCloudToolsDescribeTheCloudTheyActuallyQuery(t *testing.T) {
	ctx := context.Background()
	quiet := describing{CloudProvider: fakeCloud{}, vocab: gcpish()}
	// A scope that never matches, so the lookup widens and emits the banner.
	widening := describing{
		CloudProvider: &exactMatchCloud{resourceName: "//container.googleapis.com/projects/p/clusters/c",
			changes: []providers.Change{{
				When: time.Unix(1700000000, 0).UTC(), ManagedBy: "container.googleapis.com",
				Workload: providers.Workload{Kind: "gke_cluster", Name: "c"},
			}}},
		vocab: gcpish(),
	}

	call := func(tool interface {
		Call(context.Context, string) (string, error)
	}, args string) string {
		t.Helper()
		out, err := tool.Call(ctx, args)
		if err != nil {
			t.Fatalf("Call(%s): %v", args, err)
		}
		return out
	}

	tests := []struct {
		name string
		got  string
		want string // a GCP noun that proves the vocabulary was actually consulted
	}{
		{
			name: "cloud_what_changed's description names Cloud Audit Logs",
			got:  CloudWhatChangedTool{Cloud: quiet}.Description(),
			want: "Cloud Audit Logs",
		},
		{
			name: "cloud_what_changed's schema describes failed_only in GCP's terms",
			got:  CloudWhatChangedTool{Cloud: quiet}.Schema(),
			want: "Data Access audit logs are off by default",
		},
		{
			name: "cloud_resource_health's description names GKE, not EKS",
			got:  CloudResourceHealthTool{Cloud: quiet}.Description(),
			want: "GKE cluster and node-pool conditions",
		},
		{
			name: "cloud_resource_health's schema asks for a Compute Engine instance name",
			got:  CloudResourceHealthTool{Cloud: quiet}.Schema(),
			want: "Compute Engine instance name",
		},
		{
			name: "incident_timeline's description names the log its cloud rows come from",
			got:  IncidentTimelineTool{Cloud: quiet}.Description(),
			want: "Cloud Audit Logs: GKE/Compute/IAM actions",
		},
		{
			name: "a quiet window does not claim AWS was quiet",
			got:  call(CloudWhatChangedTool{Cloud: quiet}, `{}`),
			want: "no mutating GCP events in the window",
		},
		{
			name: "an empty failed_only lookup does not claim AWS had no failures",
			got:  call(CloudWhatChangedTool{Cloud: quiet}, `{"failed_only":true}`),
			want: "no FAILED GCP control-plane calls in the window",
		},
		{
			name: "the dropped-scope banner explains GCP's match rule, not CloudTrail's",
			got:  call(CloudWhatChangedTool{Cloud: widening}, `{"resource":"guessed"}`),
			want: "matched no Cloud Audit Log entries",
		},
		{
			name: "an empty health lookup does not claim AWS returned nothing",
			got:  call(CloudResourceHealthTool{Cloud: quiet}, `{}`),
			want: "no GCP resource health returned",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(tt.got, tt.want) {
				t.Errorf("GCP wording %q missing:\n%s", tt.want, tt.got)
			}
			for _, noun := range awsNouns {
				if strings.Contains(tt.got, noun) {
					t.Errorf("still names %q on a GCP provider:\n%s", noun, tt.got)
				}
			}
		})
	}
}

// TestCloudToolsFallBackToAWSWordingWithoutADescriber pins the compatibility half of
// the promise from the tools' side: a provider that does not implement CloudDescriber
// — which is every provider that existed before it, plus the nil Cloud
// IncidentTimelineTool is routinely registered with — renders exactly the AWS text.
//
// internal/providers/cloudvocabulary_test.go holds the frozen bytes; this only checks
// that the tools route to that vocabulary at all, which is the part a rewiring
// mistake would break.
func TestCloudToolsFallBackToAWSWordingWithoutADescriber(t *testing.T) {
	v := providers.AWSCloudVocabulary()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "a CloudProvider with no vocabulary gets cloud_what_changed's AWS description",
			got:  CloudWhatChangedTool{Cloud: fakeCloud{}}.Description(),
			want: v.ChangeDescription(),
		},
		{
			name: "a CloudProvider with no vocabulary gets cloud_resource_health's AWS description",
			got:  CloudResourceHealthTool{Cloud: fakeCloud{}}.Description(),
			want: v.HealthDescription(),
		},
		{
			name: "a nil Cloud does not panic and still renders incident_timeline",
			got:  IncidentTimelineTool{}.Description(),
			want: IncidentTimelineTool{Cloud: fakeCloud{}}.Description(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("fallback text drifted:\n got:  %q\nwant: %q", tt.got, tt.want)
			}
		})
	}
}
