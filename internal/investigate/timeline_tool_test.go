// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// timelineGitOps is a GitOpsProvider whose Changes returns a fixed list; Diff and
// WatchFailures are unused by the timeline tool (it never resolves diffs).
type timelineGitOps struct {
	changes []providers.Change
	err     error
}

func (f timelineGitOps) Changes(context.Context, providers.TimeWindow, providers.Selector) ([]providers.Change, error) {
	return f.changes, f.err
}
func (f timelineGitOps) Diff(context.Context, providers.Change) (providers.Diff, error) {
	return providers.Diff{}, nil
}
func (f timelineGitOps) WatchFailures(context.Context) (<-chan providers.FailureEvent, error) {
	return nil, nil
}

// timelineKube is a KubeReader that also implements EventWindower, so the tool can
// exercise the windowed path.
type timelineKube struct {
	events     []providers.KubeEvent
	sawSince   int
	windowUsed bool
}

func (f *timelineKube) PodStatuses(context.Context, string, string) ([]providers.PodStatus, error) {
	return nil, nil
}
func (f *timelineKube) Events(context.Context, string, string, bool) ([]providers.KubeEvent, error) {
	return f.events, nil
}
func (f *timelineKube) EventsSince(_ context.Context, _, _ string, _ bool, sinceMinutes int) ([]providers.KubeEvent, error) {
	f.windowUsed = true
	f.sawSince = sinceMinutes
	return f.events, nil
}

func TestTimelineToolInterleavesAndSorts(t *testing.T) {
	base := time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC)
	gp := timelineGitOps{changes: []providers.Change{
		{ // 14:31 flux reconcile
			Engine: providers.EngineFlux, Type: providers.ChangeSync,
			Workload: providers.Workload{Kind: "Kustomization", Name: "payments", Namespace: "payments"},
			When:     base.Add(31 * time.Minute), ManagedBy: "payments",
		},
		{ // 14:02 git ToRev
			Engine: providers.EngineFlux, Type: providers.ChangeImageBump,
			Workload: providers.Workload{Kind: "Deployment", Name: "payments", Namespace: "payments"},
			When:     base.Add(2 * time.Minute), FromRev: "aaa", ToRev: "abc123",
		},
	}}
	kube := &timelineKube{events: []providers.KubeEvent{
		{ // 14:33 event
			Type: "Warning", Reason: "BackOff", Object: "Pod/harbor-core",
			Message: "Back-off restarting harbor-core", LastSeen: base.Add(33 * time.Minute),
		},
	}}
	cloud := fakeCloud{changes: []providers.Change{
		{ // 14:20 cloud
			Engine: providers.EngineAWS, Type: providers.ChangeCloudAPI, When: base.Add(20 * time.Minute),
			ManagedBy: "autoscaling.amazonaws.com",
			Workload:  providers.Workload{Kind: "AWS::EC2::Instance", Name: "i-0abc"},
		},
	}}
	tool := IncidentTimelineTool{GitOps: gp, Kube: kube, Cloud: cloud}
	out, err := tool.Call(context.Background(), `{"namespace":"payments","since_minutes":120}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Chronological order: git(14:02) < cloud(14:20) < flux(14:31) < event(14:33).
	order := []string{"abc123", "i-0abc", "Kustomization", "Back-off restarting"}
	last := -1
	for _, tok := range order {
		i := strings.Index(out, tok)
		if i < 0 {
			t.Fatalf("output missing %q:\n%s", tok, out)
		}
		if i < last {
			t.Fatalf("token %q out of chronological order:\n%s", tok, out)
		}
		last = i
	}
	// Source tags present so the model can read WHICH datasource each row came from.
	for _, tag := range []string{"[flux]", "[cloud]", "[event]"} {
		if !strings.Contains(out, tag) {
			t.Fatalf("output missing source tag %q:\n%s", tag, out)
		}
	}
	if !kube.windowUsed {
		t.Fatalf("expected EventsSince (windowed) path to be used")
	}
	if kube.sawSince != 120 {
		t.Fatalf("since_minutes not propagated to events: got %d", kube.sawSince)
	}
}

func TestTimelineToolSkipsAbsentProviders(t *testing.T) {
	base := time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC)
	// Only GitOps wired; Kube and Cloud nil — must not panic, must still render.
	gp := timelineGitOps{changes: []providers.Change{{
		Engine: providers.EngineFlux, Type: providers.ChangeSync,
		Workload: providers.Workload{Kind: "Kustomization", Name: "app", Namespace: "app"},
		When:     base.Add(time.Minute),
	}}}
	tool := IncidentTimelineTool{GitOps: gp}
	out, err := tool.Call(context.Background(), `{"namespace":"app"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "Kustomization") {
		t.Fatalf("expected the gitops row, got:\n%s", out)
	}
}

func TestTimelineToolEmpty(t *testing.T) {
	tool := IncidentTimelineTool{GitOps: timelineGitOps{}}
	out, err := tool.Call(context.Background(), `{"namespace":"nothing"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no timestamped") {
		t.Fatalf("expected empty-timeline message, got:\n%s", out)
	}
}

func TestTimelineToolCapsRows(t *testing.T) {
	base := time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC)
	var changes []providers.Change
	for i := 0; i < maxTimelineRows+30; i++ {
		changes = append(changes, providers.Change{
			Engine: providers.EngineFlux, Type: providers.ChangeSync,
			Workload: providers.Workload{Kind: "Kustomization", Name: "app", Namespace: "app"},
			When:     base.Add(time.Duration(i) * time.Second),
		})
	}
	tool := IncidentTimelineTool{GitOps: timelineGitOps{changes: changes}}
	out, err := tool.Call(context.Background(), `{"namespace":"app"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	lines := strings.Count(out, "\n")
	if lines > maxTimelineRows+3 { // rows + a truncation note + trailing newline slack
		t.Fatalf("row cap not enforced: %d lines\n%s", lines, out)
	}
	if !strings.Contains(out, "more") {
		t.Fatalf("expected a truncation note, got:\n%s", out)
	}
}

func TestTimelineToolBytesCap(t *testing.T) {
	base := time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC)
	var changes []providers.Change
	big := strings.Repeat("x", 4000)
	for i := 0; i < 20; i++ {
		changes = append(changes, providers.Change{
			Engine: providers.EngineFlux, Type: providers.ChangeSync,
			Workload: providers.Workload{Kind: "Kustomization", Name: big, Namespace: "app"},
			When:     base.Add(time.Duration(i) * time.Second),
		})
	}
	tool := IncidentTimelineTool{GitOps: timelineGitOps{changes: changes}}
	out, err := tool.Call(context.Background(), `{"namespace":"app"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(out) > maxTimelineBytes+200 {
		t.Fatalf("byte cap not enforced: %d bytes", len(out))
	}
}

// zeroWhenGitOps returns a change with no timestamp — it must be dropped (a
// timeline needs a WHEN).
func TestTimelineToolDropsZeroTimestamps(t *testing.T) {
	tool := IncidentTimelineTool{GitOps: timelineGitOps{changes: []providers.Change{
		{Engine: providers.EngineFlux, Workload: providers.Workload{Kind: "Kustomization", Name: "app", Namespace: "app"}}, // zero When
	}}}
	out, err := tool.Call(context.Background(), `{"namespace":"app"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no timestamped") {
		t.Fatalf("expected untimestamped change to be dropped, got:\n%s", out)
	}
}
