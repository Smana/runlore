// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
)

// fakeTool is a minimal investigate.Tool for decorator tests.
type fakeTool struct {
	name string
	out  string
	err  error
}

func (f fakeTool) Name() string        { return f.name }
func (f fakeTool) Description() string { return "fake " + f.name }
func (f fakeTool) Schema() string      { return `{"type":"object","properties":{}}` }
func (f fakeTool) Call(context.Context, string) (string, error) {
	return f.out, f.err
}

func TestRecordingToolRecordsCall(t *testing.T) {
	rec := &Recorder{}
	rt := recordingTool{inner: fakeTool{name: "pod_status", out: "phase=Pending"}, rec: rec}
	out, err := rt.Call(context.Background(), `{"namespace":"x"}`)
	if err != nil || out != "phase=Pending" {
		t.Fatalf("delegate broken: out=%q err=%v", out, err)
	}
	if rt.Name() != "pod_status" {
		t.Fatalf("name not delegated: %q", rt.Name())
	}
	calls := rec.Calls()
	if len(calls) != 1 || calls[0].Name != "pod_status" || calls[0].Output != "phase=Pending" {
		t.Fatalf("not recorded: %+v", calls)
	}
}

func TestRecordingToolRecordsError(t *testing.T) {
	rec := &Recorder{}
	rt := recordingTool{inner: fakeTool{name: "cloud_what_changed", err: errors.New("timeout")}, rec: rec}
	if _, err := rt.Call(context.Background(), "{}"); err == nil {
		t.Fatal("want error propagated")
	}
	if c := rec.Calls(); len(c) != 1 || c[0].Err == "" {
		t.Fatalf("error not recorded: %+v", c)
	}
}

// TestRecordingToolRedactsBeforeRecording pins the ordering that matters for
// `lore eval --live --record`: the recorder sits INSIDE the investigation loop,
// which applies redact.Secrets to a tool's return value only after the tool has
// returned. Without redacting here, the recorder holds the raw cluster evidence
// and WriteCase persists it to disk as a replayable case — a file a human may
// later promote into the tracked fixture corpus, at which point the secret is in
// git. Anything leaving the process gets the same redaction the model's copy does.
func TestRecordingToolRedactsBeforeRecording(t *testing.T) {
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	raw := "kind: Secret\ndata:\n  password: c3VwM3JzM2NyZXQ=\n" +
		"controller log: authenticating with " + secret + "\n"

	rec := &Recorder{}
	rt := recordingTool{inner: fakeTool{name: "controller_logs", out: raw}, rec: rec}

	// The loop still receives the tool's own output — the recorder must not change
	// what the investigation reasons over (the loop redacts that copy itself).
	out, err := rt.Call(context.Background(), "{}")
	if err != nil || out != raw {
		t.Fatalf("delegate must return the tool's output unchanged: out=%q err=%v", out, err)
	}

	calls := rec.Calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 recorded call, got %+v", calls)
	}
	for _, leak := range []string{secret, "c3VwM3JzM2NyZXQ="} {
		if strings.Contains(calls[0].Output, leak) {
			t.Errorf("recorded output would be written to disk un-redacted: %q\n -> %q", leak, calls[0].Output)
		}
	}
	// Structure survives, so a recorded case is still a usable replay fixture.
	if !strings.Contains(calls[0].Output, "kind: Secret") {
		t.Errorf("redaction must preserve structure, got %q", calls[0].Output)
	}
}

// TestRecordingToolRedactsRecordedError: a tool error is persisted into the case
// too (and surfaces in the report), and backends build their errors from the
// upstream response body — "logs status 401: {...}" can carry the credential the
// request was rejected for.
func TestRecordingToolRedactsRecordedError(t *testing.T) {
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	rec := &Recorder{}
	rt := recordingTool{
		inner: fakeTool{name: "query_logs", err: errors.New("logs status 401: bad token " + secret)},
		rec:   rec,
	}
	if _, err := rt.Call(context.Background(), "{}"); err == nil {
		t.Fatal("want error propagated")
	}
	if c := rec.Calls(); len(c) != 1 || strings.Contains(c[0].Err, secret) {
		t.Errorf("recorded error must be redacted: %+v", c)
	}
}

func TestScoreCoverage(t *testing.T) {
	calls := []Call{
		{Name: "what_changed", Output: "diff"},
		{Name: "gitops_resource_status", Output: "Ready=False"},
		{Name: "query_logs", Output: "boom"},
		{Name: "cloud_what_changed", Err: "timeout"},
	}
	// "cloud", not "aws": the datasource group names the lens, not the vendor behind
	// it, so a GCP investigation is not scored as having covered AWS.
	cov := ScoreCoverage([]string{"gitops", "logs"}, []string{"cloud"}, calls)
	if cov.Ratio != 1.0 {
		t.Fatalf("want full coverage, got %.2f (touched=%v missing=%v)", cov.Ratio, cov.Touched, cov.Missing)
	}
	if !cov.CrossSignal {
		t.Fatal("want cross-signal true (gitops+logs)")
	}
	if len(cov.Bonus) != 1 || cov.Bonus[0] != "cloud" {
		t.Fatalf("want cloud bonus, got %v", cov.Bonus)
	}
	if len(cov.ToolErrors) != 1 || cov.ToolErrors[0] != "cloud_what_changed" {
		t.Fatalf("want cloud_what_changed flagged, got %v", cov.ToolErrors)
	}

	miss := ScoreCoverage([]string{"gitops", "metrics"}, nil, calls)
	if miss.Ratio != 0.5 || len(miss.Missing) != 1 || miss.Missing[0] != "metrics" {
		t.Fatalf("want 0.5 + metrics missing, got %.2f %v", miss.Ratio, miss.Missing)
	}
}

func TestWrapDecoratesAndRecords(t *testing.T) {
	rec := &Recorder{}
	wrapped := wrap([]investigate.Tool{fakeTool{name: "what_changed", out: "diff"}}, rec)
	if len(wrapped) != 1 || wrapped[0].Name() != "what_changed" {
		t.Fatalf("wrap did not decorate: %+v", wrapped)
	}
	if _, err := wrapped[0].Call(context.Background(), "{}"); err != nil {
		t.Fatal(err)
	}
	if c := rec.Calls(); len(c) != 1 || c[0].Name != "what_changed" {
		t.Fatalf("wrap's tool did not record: %+v", c)
	}
}
