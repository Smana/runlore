// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeLogReader returns canned pod-log lines and records the call.
type fakeLogReader struct {
	lines              providers.LogResult
	gotNS, gotSelector string
	gotSince           int
}

func (f *fakeLogReader) PodLogs(_ context.Context, q providers.PodLogQuery) (providers.LogResult, error) {
	f.gotNS, f.gotSelector, f.gotSince = q.Namespace, q.LabelSelector, q.SinceMinutes
	return f.lines, nil
}

func TestControllerLogsTool(t *testing.T) {
	r := &fakeLogReader{lines: providers.LogResult{
		{Message: "source-controller-1: ERROR GitRepository/infra-artifact failed to checkout: reference not found"},
		{Message: "source-controller-1: INFO reconciliation finished"},
	}}
	tool := ControllerLogsTool{Logs: r}
	if tool.Name() != "controller_logs" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"controller":"source-controller","resource":"infra-artifact","since_minutes":15}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Scoping: queried flux-system with the controller's app selector and our window.
	if r.gotNS != "flux-system" || r.gotSelector != "app=source-controller" || r.gotSince != 15 {
		t.Fatalf("unexpected query: ns=%q selector=%q since=%d", r.gotNS, r.gotSelector, r.gotSince)
	}
	// Resource filter keeps only the matching line.
	if !strings.Contains(out, "reference not found") {
		t.Fatalf("expected the matching line, got:\n%s", out)
	}
	if strings.Contains(out, "reconciliation finished") {
		t.Fatalf("resource filter should have excluded the non-matching line:\n%s", out)
	}
}

func TestControllerLogsToolDefaultsAndEmpty(t *testing.T) {
	r := &fakeLogReader{lines: providers.LogResult{{Message: "x: line"}}}
	tool := ControllerLogsTool{Logs: r}
	// since defaults to 30 when unset; a resource filter with no match → friendly message.
	out, err := tool.Call(context.Background(), `{"controller":"kustomize-controller","resource":"nope"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if r.gotSince != 30 {
		t.Fatalf("since default = %d, want 30", r.gotSince)
	}
	if !strings.Contains(out, "no matching controller log lines") {
		t.Fatalf("expected empty message, got: %s", out)
	}
}
