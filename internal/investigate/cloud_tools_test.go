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

func (f fakeCloud) CloudChanges(context.Context, providers.Selector, providers.TimeWindow) ([]providers.Change, error) {
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
}

func (f *exactMatchCloud) CloudChanges(_ context.Context, sel providers.Selector, _ providers.TimeWindow) ([]providers.Change, error) {
	f.scopedCalls = append(f.scopedCalls, sel.Name)
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
