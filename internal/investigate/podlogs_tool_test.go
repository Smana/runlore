package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeWorkloadLogs records the PodLogs call (incl. the previous flag) and returns
// canned lines. called reports whether the cluster was queried at all — the
// namespace-allowlist guard must reject out-of-allowlist requests BEFORE fetching.
type fakeWorkloadLogs struct {
	lines       providers.LogResult
	called      bool
	gotNS       string
	gotSelector string
	gotSince    int
	gotPrevious bool
}

func (f *fakeWorkloadLogs) PodLogs(_ context.Context, q providers.PodLogQuery) (providers.LogResult, error) {
	f.called = true
	f.gotNS, f.gotSelector, f.gotSince, f.gotPrevious = q.Namespace, q.LabelSelector, q.SinceMinutes, q.Previous
	return f.lines, nil
}

func TestPodLogsToolPreviousContainer(t *testing.T) {
	f := &fakeWorkloadLogs{lines: providers.LogResult{
		{Message: "web-abc: panic: runtime error: invalid memory address or nil pointer dereference"},
	}}
	tool := PodLogsTool{Logs: f, IncidentNamespace: "apps"}
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
	tool := PodLogsTool{Logs: f, IncidentNamespace: "apps"}
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
	tool := PodLogsTool{Logs: &fakeWorkloadLogs{}, IncidentNamespace: "apps"}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no log lines") {
		t.Fatalf("want a no-lines note, got: %q", out)
	}
}

// The pod_logs tool streams raw pod logs (which carry secrets/PII) to the external
// LLM, so the model must not be able to pull logs from arbitrary namespaces. The
// allowed set is the incident's own namespace ∪ an operator-configured allowlist.

func TestPodLogsToolAllowsIncidentNamespace(t *testing.T) {
	f := &fakeWorkloadLogs{lines: providers.LogResult{{Message: "web-abc: serving on :8080"}}}
	// No configured allowlist: only the incident's own namespace is permitted.
	tool := PodLogsTool{Logs: f, IncidentNamespace: "apps"}
	if _, err := tool.Call(context.Background(), `{"namespace":"apps","selector":"app=web"}`); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !f.called || f.gotNS != "apps" {
		t.Fatalf("incident namespace should be fetched: called=%v ns=%q", f.called, f.gotNS)
	}
}

func TestPodLogsToolAllowsConfiguredNamespace(t *testing.T) {
	f := &fakeWorkloadLogs{lines: providers.LogResult{{Message: "kustomize-controller: reconciled"}}}
	// flux-system is not the incident namespace but is on the configured allowlist.
	tool := PodLogsTool{Logs: f, IncidentNamespace: "apps", AllowedNamespaces: []string{"flux-system"}}
	if _, err := tool.Call(context.Background(), `{"namespace":"flux-system","selector":"app=kustomize-controller"}`); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !f.called || f.gotNS != "flux-system" {
		t.Fatalf("allowlisted namespace should be fetched: called=%v ns=%q", f.called, f.gotNS)
	}
}

func TestPodLogsToolRejectsOutOfAllowlistNamespace(t *testing.T) {
	f := &fakeWorkloadLogs{lines: providers.LogResult{{Message: "secret: SHOULD-NOT-LEAK"}}}
	tool := PodLogsTool{Logs: f, IncidentNamespace: "apps", AllowedNamespaces: []string{"flux-system"}}
	// kube-system is neither the incident namespace nor on the allowlist.
	out, err := tool.Call(context.Background(), `{"namespace":"kube-system"}`)
	if err != nil {
		t.Fatalf("the guard must return a non-fatal error string to the model, not a Go error: %v", err)
	}
	// Must not have queried the cluster — the guard runs BEFORE fetching.
	if f.called {
		t.Fatal("out-of-allowlist namespace must not reach the cluster log source")
	}
	// The message must be explanatory: name the rejected namespace and tell the
	// model where the allowed set comes from.
	if !strings.Contains(out, `namespace "kube-system" is not permitted for pod_logs`) {
		t.Fatalf("expected an explanatory rejection naming the namespace, got: %q", out)
	}
	if !strings.Contains(out, "pod_log_namespaces") {
		t.Fatalf("rejection should point at the pod_log_namespaces allowlist, got: %q", out)
	}
}
