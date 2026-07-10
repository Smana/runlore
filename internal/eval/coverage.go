// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"sort"
	"sync"

	"github.com/Smana/runlore/internal/investigate"
)

// toolSource maps each investigation tool to its data-source group. submit_findings
// and any unknown tool map to "" (ignored by coverage).
var toolSource = map[string]string{
	"what_changed":           "gitops",
	"gitops_resource_status": "gitops",
	"gitops_tree":            "gitops",
	"pod_status":             "kubernetes",
	"kube_events":            "kubernetes",
	"controller_logs":        "kubernetes",
	"query_metrics":          "metrics",
	"query_logs":             "logs",
	"network_drops":          "network",
	"cloud_what_changed":     "aws",
	"cloud_resource_health":  "aws",
	"kb_search":              "kb",
}

// Call is one recorded tool invocation during a live investigation.
type Call struct {
	Name   string
	Args   string
	Output string
	Err    string
}

// Recorder collects tool calls made during one investigation run. Safe for
// concurrent use (the loop is sequential today, but tools may fan out later).
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

func (r *Recorder) record(c Call) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

// Calls returns a copy of the recorded calls in order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Call, len(r.calls))
	copy(out, r.calls)
	return out
}

// recordingTool wraps an investigate.Tool, recording every call (name, args,
// output, error) before returning the inner result unchanged.
type recordingTool struct {
	inner investigate.Tool
	rec   *Recorder
}

func (t recordingTool) Name() string        { return t.inner.Name() }
func (t recordingTool) Description() string { return t.inner.Description() }
func (t recordingTool) Schema() string      { return t.inner.Schema() }

func (t recordingTool) Call(ctx context.Context, args string) (string, error) {
	out, err := t.inner.Call(ctx, args)
	c := Call{Name: t.inner.Name(), Args: args, Output: out}
	if err != nil {
		c.Err = err.Error()
	}
	t.rec.record(c)
	return out, err
}

// wrap decorates each tool with the recorder.
func wrap(tools []investigate.Tool, rec *Recorder) []investigate.Tool {
	out := make([]investigate.Tool, len(tools))
	for i, tl := range tools {
		out[i] = recordingTool{inner: tl, rec: rec}
	}
	return out
}

// Coverage is the deterministic data-source coverage result for one run.
type Coverage struct {
	Touched     []string // mandatory source groups actually exercised
	Missing     []string // mandatory groups never touched
	Bonus       []string // optional groups touched
	CrossSignal bool     // >=2 distinct source groups exercised
	ToolErrors  []string // distinct tool names that returned an error
	Ratio       float64  // |touched| / |expected|  (1.0 when no expected sources)
}

// ScoreCoverage computes coverage of the mandatory expected sources from the
// recorded calls. optional sources count as Bonus and never affect Ratio.
func ScoreCoverage(expected, optional []string, calls []Call) Coverage {
	seen := map[string]bool{}
	errored := map[string]bool{}
	var cov Coverage
	for _, c := range calls {
		if c.Err != "" && !errored[c.Name] {
			errored[c.Name] = true
			cov.ToolErrors = append(cov.ToolErrors, c.Name)
		}
		if grp := toolSource[c.Name]; grp != "" {
			seen[grp] = true
		}
	}
	cov.CrossSignal = len(seen) >= 2
	for _, e := range expected {
		if seen[e] {
			cov.Touched = append(cov.Touched, e)
		} else {
			cov.Missing = append(cov.Missing, e)
		}
	}
	for _, o := range optional {
		if seen[o] {
			cov.Bonus = append(cov.Bonus, o)
		}
	}
	sort.Strings(cov.Touched)
	sort.Strings(cov.Missing)
	sort.Strings(cov.Bonus)
	if len(expected) == 0 {
		cov.Ratio = 1.0
	} else {
		cov.Ratio = float64(len(cov.Touched)) / float64(len(expected))
	}
	return cov
}
