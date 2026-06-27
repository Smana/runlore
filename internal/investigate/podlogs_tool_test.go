package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeWorkloadLogs records the PodLogs call (incl. the previous flag) and returns
// canned lines.
type fakeWorkloadLogs struct {
	lines       providers.LogResult
	gotNS       string
	gotSelector string
	gotSince    int
	gotPrevious bool
}

func (f *fakeWorkloadLogs) PodLogs(_ context.Context, q providers.PodLogQuery) (providers.LogResult, error) {
	f.gotNS, f.gotSelector, f.gotSince, f.gotPrevious = q.Namespace, q.LabelSelector, q.SinceMinutes, q.Previous
	return f.lines, nil
}

func TestPodLogsToolPreviousContainer(t *testing.T) {
	f := &fakeWorkloadLogs{lines: providers.LogResult{
		{Message: "web-abc: panic: runtime error: invalid memory address or nil pointer dereference"},
	}}
	tool := PodLogsTool{Logs: f}
	if tool.Name() != "pod_logs" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"namespace":"apps","selector":"app=web","since_minutes":15,"previous":true}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The crash output of a CrashLoopBackOff lives in the PREVIOUS container; the
	// tool must thread previous=true and the scoping through to the reader.
	if f.gotNS != "apps" || f.gotSelector != "app=web" || f.gotSince != 15 || !f.gotPrevious {
		t.Fatalf("unexpected query: ns=%q selector=%q since=%d previous=%v", f.gotNS, f.gotSelector, f.gotSince, f.gotPrevious)
	}
	if !strings.Contains(out, "panic: runtime error") {
		t.Fatalf("crash output not surfaced:\n%s", out)
	}
}

func TestPodLogsToolDefaultsCurrentContainer(t *testing.T) {
	f := &fakeWorkloadLogs{lines: providers.LogResult{{Message: "web-abc: serving on :8080"}}}
	tool := PodLogsTool{Logs: f}
	if _, err := tool.Call(context.Background(), `{"namespace":"apps"}`); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// previous defaults to false (current container), since defaults to 30m.
	if f.gotPrevious {
		t.Fatalf("previous should default to false")
	}
	if f.gotSince != 30 {
		t.Fatalf("since default = %d, want 30", f.gotSince)
	}
}

func TestPodLogsToolRequiresNamespace(t *testing.T) {
	tool := PodLogsTool{Logs: &fakeWorkloadLogs{}}
	if _, err := tool.Call(context.Background(), `{"selector":"app=web"}`); err == nil {
		t.Fatal("want an error when namespace is missing")
	}
}

func TestPodLogsToolEmpty(t *testing.T) {
	tool := PodLogsTool{Logs: &fakeWorkloadLogs{}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no log lines") {
		t.Fatalf("want a no-lines note, got: %q", out)
	}
}
