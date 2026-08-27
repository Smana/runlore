// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/redact"
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
	// "cloud", not the vendor. These two tools are one lens whose backing provider is
	// an operator's choice, and scoring a GCP investigation as having covered "aws"
	// reported the wrong thing about what an investigation actually looked at.
	"cloud_what_changed":    "cloud",
	"cloud_resource_health": "cloud",
	"kb_search":             "kb",
}

// SourceGroups returns every data-source group a scenario may name, sorted.
//
// Derived from toolSource rather than listed separately, because the two hand-maintained
// copies of this list were how a rename went wrong: the group was renamed "aws" -> "cloud"
// in Go while two scenario files and the rubric kept saying "aws", and since the name is
// an untyped string shared between Go and YAML with no validation at either end, the
// scenario that exists to exercise the cloud lens scored Ratio 0.0 with Missing=[aws] on
// every run — silently, and while the tools it names were being used correctly.
func SourceGroups() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range toolSource {
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// ValidateSourceGroups reports an error for every name in groups that is not a real
// data-source group, naming the field it came from and listing the valid values.
//
// A rename should fail loudly at load rather than score zero forever.
func ValidateSourceGroups(field string, groups []string) error {
	valid := map[string]bool{}
	for _, g := range SourceGroups() {
		valid[g] = true
	}
	var errs []error
	for _, g := range groups {
		if !valid[g] {
			errs = append(errs, fmt.Errorf("unknown source group %q in %s; valid: %s",
				g, field, strings.Join(SourceGroups(), ", ")))
		}
	}
	return errors.Join(errs...)
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

// Call delegates, records, and returns the inner result UNCHANGED — the loop
// applies its own redaction to the copy the model sees.
//
// What is recorded is redacted here, because this decorator sits inside the
// loop: investigate applies redact.Secrets to a tool's return value only after
// the tool has returned, so the recorder would otherwise hold the raw cluster
// evidence. `lore eval --live --record` then writes that to disk as a replayable
// case (app.RunEvalLive → eval.WriteCase), which a human may later promote into
// the tracked fixture corpus — putting a live cluster secret in git. Recording
// is an egress just like the model call is, so it gets the same treatment.
//
// The recorded error text is redacted too: backends build their errors from the
// upstream response body ("logs status 401: …"), which can carry the credential
// the request was rejected for.
func (t recordingTool) Call(ctx context.Context, args string) (string, error) {
	out, err := t.inner.Call(ctx, args)
	c := Call{Name: t.inner.Name(), Args: args, Output: redact.Secrets(out)}
	if err != nil {
		c.Err = redact.Secrets(err.Error())
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
