// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
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
