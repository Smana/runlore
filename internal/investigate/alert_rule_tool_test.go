// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// ruleScoping is how the fake backend treats the `rule_name[]` scoping the tool
// asks for. It is the axis the two-step read exists to survive: the tool cannot
// know which of these it is talking to, and one of them answers a scoped read the
// same way a backend answers a name that has no rule at all.
type ruleScoping int

const (
	// scopingIgnored serves the whole ruleset whatever is asked — a backend
	// predating rule_name[]. Harmless: the client-side match still finds the rule.
	scopingIgnored ruleScoping = iota
	// scopingHonoured filters by exact name, as Prometheus and vmalert do.
	scopingHonoured
	// scopingBroken mishandles the parameter and answers EMPTY. Indistinguishable
	// from "this alertname has no rule" — the false negative the fallback removes.
	scopingBroken
	// scopingRejected refuses the parameter outright (a strict proxy, a 400) while
	// still serving the unscoped read.
	scopingRejected
)

// fakeRuleMetrics embeds the package's plain MetricsProvider double and adds the
// OPTIONAL providers.AlertRuleReader capability, so the tool's happy path and its
// error path are both exercised against a real type assertion. A bare fakeMetrics
// (no AlertRules method) is the backend that lacks the capability entirely.
//
// It behaves like a backend rather than a mock: `rules` is everything it defines,
// and `scoping` decides what a scoped read gets back. `calls` records the names
// each read asked for, which is how the tests pin the request COUNT — the payload
// win is worth nothing if the tool still reads the full ruleset on the happy path.
type fakeRuleMetrics struct {
	fakeMetrics
	rules   []providers.AlertRule
	err     error
	scoping ruleScoping
	calls   [][]string
}

func (f *fakeRuleMetrics) AlertRules(_ context.Context, names ...string) ([]providers.AlertRule, error) {
	f.calls = append(f.calls, names)
	if f.err != nil {
		return nil, f.err
	}
	if len(names) == 0 {
		return f.rules, nil
	}
	switch f.scoping {
	case scopingIgnored:
		// A backend predating rule_name[]: the parameter is simply not read.
	case scopingHonoured:
		var out []providers.AlertRule
		for _, r := range f.rules {
			if slices.Contains(names, r.Name) {
				out = append(out, r)
			}
		}
		return out, nil
	case scopingBroken:
		return nil, nil
	case scopingRejected:
		return nil, errors.New("metrics status 400: unsupported parameter \"rule_name[]\"")
	}
	return f.rules, nil
}

// wantCalls pins the exact sequence of reads: each element is one call\'s names,
// with nil meaning an UNSCOPED read.
func wantCalls(t *testing.T, f *fakeRuleMetrics, want [][]string) {
	t.Helper()
	if !slices.EqualFunc(f.calls, want, slices.Equal) {
		t.Fatalf("AlertRules reads = %v, want %v", f.calls, want)
	}
}

// rdsRule is the real RDSCriticalLatency rule that was misdiagnosed twice: its
// expression thresholds *_maximum, while both investigations reasoned about
// read_latency_maximum and write_latency_AVERAGE.
var rdsRule = providers.AlertRule{
	Name:  "RDSCriticalLatency",
	Query: `(aws_rds_read_latency_maximum > 0.050) or (aws_rds_write_latency_maximum{cluster=~".*"} > 0.050)`,
	For:   5 * time.Minute,
	State: "firing", Health: "ok",
	Group: "rds", File: "/etc/vmalert/rds.yaml",
	Labels:      map[string]string{"severity": "critical"},
	Annotations: map[string]string{"summary": "RDS latency above 50ms"},
}

func TestAlertRuleToolName(t *testing.T) {
	tool := AlertRuleTool{Metrics: &fakeRuleMetrics{}}
	if tool.Name() != "alert_rule" {
		t.Fatalf("name=%q, want alert_rule", tool.Name())
	}
	// The description must teach WHY the rule text matters, or the model has no
	// reason to prefer it over guessing at a metric name.
	d := tool.Description()
	for _, want := range []string{"_maximum", "_average", "threshold"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description must mention %q:\n%s", want, d)
		}
	}
}

func TestAlertRuleToolCall(t *testing.T) {
	tests := []struct {
		name     string
		provider providers.MetricsProvider
		args     string
		want     []string // substrings the rendered result must contain
		notWant  []string
		wantErr  bool
	}{
		{
			name:     "rule found renders the thresholded expression",
			provider: &fakeRuleMetrics{rules: []providers.AlertRule{rdsRule}},
			args:     `{"alert":"RDSCriticalLatency"}`,
			want: []string{
				"RDSCriticalLatency",
				"aws_rds_write_latency_maximum",
				"for=5m",
				"state=firing",
				"severity=\"critical\"",
				"/etc/vmalert/rds.yaml",
			},
		},
		{
			// scopingHonoured is the real trap: `rule_name[]=rdscriticallatency` is an
			// EXACT match on the backend, so the scoped read comes back empty and only
			// the unscoped re-read can still fold-match the name.
			name: "name match is case-insensitive so a reworded alertname still resolves",
			provider: &fakeRuleMetrics{scoping: scopingHonoured, rules: []providers.AlertRule{rdsRule,
				{Name: "KubePodCrashLooping", Query: "rate(kube_pod_container_status_restarts_total[5m]) > 0"}}},
			args: `{"alert":"rdscriticallatency"}`,
			want: []string{"aws_rds_write_latency_maximum"},
			// The other rule must not be dragged in by a loose match.
			notWant: []string{"kube_pod_container_status_restarts_total"},
		},
		{
			name: "multiple rules sharing a name are ALL rendered",
			provider: &fakeRuleMetrics{rules: []providers.AlertRule{
				{Name: "RDSCriticalLatency", Query: "aws_rds_read_latency_maximum > 0.050", Group: "prod", For: 5 * time.Minute},
				{Name: "RDSCriticalLatency", Query: "aws_rds_write_latency_maximum > 0.200", Group: "staging", For: 15 * time.Minute},
			}},
			args: `{"alert":"RDSCriticalLatency"}`,
			want: []string{
				"2 rule(s)",
				"aws_rds_read_latency_maximum > 0.050",
				"aws_rds_write_latency_maximum > 0.200",
				"prod", "staging",
			},
		},
		{
			name: "no matching rule names the alerts that DO exist instead of dead-ending",
			provider: &fakeRuleMetrics{scoping: scopingHonoured, rules: []providers.AlertRule{
				{Name: "KubePodCrashLooping", Query: "x > 1"},
				{Name: "NodeFilesystemAlmostOutOfSpace", Query: "y > 1"},
			}},
			args: `{"alert":"RDSCriticalLatency"}`,
			want: []string{
				"no alerting rule named", "RDSCriticalLatency",
				"KubePodCrashLooping", "NodeFilesystemAlmostOutOfSpace",
			},
		},
		{
			// fakeMetrics implements ONLY providers.MetricsProvider — a backend with no
			// rules endpoint at all (e.g. a bare remote-read gateway).
			name:     "backend with no rules endpoint degrades to unavailable",
			provider: fakeMetrics{},
			args:     `{"alert":"RDSCriticalLatency"}`,
			want:     []string{"alert_rule is unavailable"},
		},
		{
			name:     "backend error degrades to unavailable rather than failing the investigation",
			provider: &fakeRuleMetrics{err: errors.New("metrics status 404: unsupported path")},
			args:     `{"alert":"RDSCriticalLatency"}`,
			want:     []string{"alert_rule is unavailable", "404"},
		},
		{
			name:     "no rules at all degrades to unavailable",
			provider: &fakeRuleMetrics{},
			args:     `{"alert":"RDSCriticalLatency"}`,
			want:     []string{"alert_rule is unavailable"},
		},
		{
			name:     "a missing alert argument is the model's error to fix",
			provider: &fakeRuleMetrics{rules: []providers.AlertRule{rdsRule}},
			args:     `{}`,
			wantErr:  true,
		},
		{
			name:     "malformed args error",
			provider: &fakeRuleMetrics{},
			args:     `not json`,
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := AlertRuleTool{Metrics: tc.provider}.Call(context.Background(), tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Fatalf("output must contain %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Fatalf("output must NOT contain %q:\n%s", w, out)
				}
			}
		})
	}
}

// The rendered rule must warn about the statistic trap in-band: the model reads the
// tool result, not the tool description, when it decides which series to query.
func TestAlertRuleToolResultCarriesTheStatisticWarning(t *testing.T) {
	out, err := AlertRuleTool{Metrics: &fakeRuleMetrics{rules: []providers.AlertRule{rdsRule}}}.
		Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"exact metric", "_maximum", "_average"} {
		if !strings.Contains(out, want) {
			t.Fatalf("result must contain %q:\n%s", want, out)
		}
	}
}

// The system prompt must direct the model at alert_rule when it is registered —
// a tool the model never calls fixes nothing. Mirrors the source_diff gating.
func TestSystemPromptMentionsAlertRuleOnlyWhenRegistered(t *testing.T) {
	without := (&LoopInvestigator{Tools: []Tool{QueryMetricsTool{Metrics: fakeMetrics{}}}}).system()
	if strings.Contains(without, "alert_rule") {
		t.Fatalf("prompt must not mention alert_rule when the tool is absent:\n%s", without)
	}
	with := (&LoopInvestigator{Tools: []Tool{
		QueryMetricsTool{Metrics: fakeMetrics{}},
		AlertRuleTool{Metrics: &fakeRuleMetrics{}},
	}}).system()
	if !strings.Contains(with, "alert_rule") {
		t.Fatalf("prompt must direct the model at alert_rule when registered:\n%s", with)
	}
}

// A cancelled/expired context must surface as an ERROR, not as a cheerful
// "unavailable" string: runTool only classifies a per-tool timeout when Call
// returns an error, so swallowing it would record result="ok" for a call that
// never completed. The investigation still continues — runTool renders the error
// as this tool's result — so this does not weaken graceful degradation.
func TestAlertRuleToolPropagatesContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AlertRuleTool{Metrics: &fakeRuleMetrics{err: context.Canceled}}.
		Call(ctx, `{"alert":"RDSCriticalLatency"}`)
	if err == nil {
		t.Fatal("a cancelled context must be returned as an error, not swallowed into an 'unavailable' result")
	}
	// A plain backend failure on a LIVE context still degrades to a string, so a 404
	// rules endpoint can never abort an investigation.
	out, err := AlertRuleTool{Metrics: &fakeRuleMetrics{err: errors.New("metrics status 404: nope")}}.
		Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("a non-context backend error must still degrade: %v", err)
	}
	if !strings.Contains(out, alertRuleUnavailable) {
		t.Fatalf("want the unavailable degrade, got %q", out)
	}
}

// The unmatched-name list is capped at maxToolRows. When the backend defines more
// alertnames than the cap, the near-miss the model must correct against has to
// survive the cut — alphabetically it would not. This is the real shape of the
// failure: 254 alertnames, with the wanted one sorting past index 160.
func TestAlertRuleUnmatchedListHoistsTheNearMissAboveTheRowCap(t *testing.T) {
	rules := make([]providers.AlertRule, 0, 260)
	for i := 0; i < 260; i++ {
		rules = append(rules, providers.AlertRule{Name: fmt.Sprintf("AAA%03dAlert", i), Query: "up == 0"})
	}
	// Sorts far past the 50-row cap among the AAA* names.
	rules = append(rules, providers.AlertRule{Name: "RDSCriticalLatencyProd", Query: "x > 1"})

	out, err := AlertRuleTool{Metrics: &fakeRuleMetrics{scoping: scopingHonoured, rules: rules}}.
		Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "RDSCriticalLatencyProd") {
		t.Fatalf("the near-miss name must survive the row cap:\n%s", out)
	}
	if strings.Count(out, "\n") > maxToolRows+6 {
		t.Fatalf("output must still be capped at ~maxToolRows rows:\n%s", out)
	}
}

// The happy path must cost ONE scoped read, not a full-ruleset download. The real
// backend serves 278 rules / ~509 KB unscoped; scoped it returns the one rule the
// tool asked for. Pinning the call sequence is what keeps that win from silently
// regressing into "scoped read, then read everything anyway".
func TestAlertRuleToolReadsOnlyTheRequestedRuleOnAHit(t *testing.T) {
	f := &fakeRuleMetrics{scoping: scopingHonoured, rules: []providers.AlertRule{rdsRule,
		{Name: "KubePodCrashLooping", Query: "rate(kube_pod_container_status_restarts_total[5m]) > 0"}}}
	out, err := AlertRuleTool{Metrics: f}.Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "aws_rds_write_latency_maximum") {
		t.Fatalf("the scoped read must still render the rule:\n%s", out)
	}
	wantCalls(t, f, [][]string{{"RDSCriticalLatency"}})
}

// THE CRUX of the two-step read. A backend that MIS-handles rule_name[] answers a
// scoped read with an empty set — which is byte-for-byte what "this alertname has
// no rule" looks like. Concluding "no rule found" from it would reintroduce, through
// the optimisation, the exact false negative this capability exists to remove. An
// empty scoped read must therefore be re-read UNSCOPED before the tool concludes
// anything.
func TestAlertRuleToolRereadsUnscopedWhenScopingReturnsNothing(t *testing.T) {
	f := &fakeRuleMetrics{scoping: scopingBroken, rules: []providers.AlertRule{rdsRule}}
	out, err := AlertRuleTool{Metrics: f}.Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "aws_rds_write_latency_maximum") {
		t.Fatalf("the rule must survive a backend that mishandles rule_name[]:\n%s", out)
	}
	for _, bad := range []string{alertRuleUnavailable, "no alerting rule named"} {
		if strings.Contains(out, bad) {
			t.Fatalf("a mishandled scoping parameter must not read as %q:\n%s", bad, out)
		}
	}
	wantCalls(t, f, [][]string{{"RDSCriticalLatency"}, nil})
}

// A backend that REJECTS rule_name[] outright (a strict proxy answering 400) must
// not turn a previously working lookup into "unavailable" — the scoped read is an
// optimisation, and no optimisation may cost the capability.
func TestAlertRuleToolRereadsUnscopedWhenTheBackendRejectsScoping(t *testing.T) {
	f := &fakeRuleMetrics{scoping: scopingRejected, rules: []providers.AlertRule{rdsRule}}
	out, err := AlertRuleTool{Metrics: f}.Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "aws_rds_write_latency_maximum") {
		t.Fatalf("a backend that refuses rule_name[] must still resolve the rule:\n%s", out)
	}
	wantCalls(t, f, [][]string{{"RDSCriticalLatency"}, nil})
}

// The tension scoping creates: a scoped response cannot contain the OTHER
// alertnames, which is exactly what the unmatched-name recovery list is built from.
// The unscoped second read is what resolves it — a genuinely absent name must still
// be answered with the names the backend does define, not with a bare dead end.
func TestAlertRuleToolRecoveryListSurvivesScoping(t *testing.T) {
	f := &fakeRuleMetrics{scoping: scopingHonoured, rules: []providers.AlertRule{
		{Name: "KubePodCrashLooping", Query: "x > 1"},
		{Name: "NodeFilesystemAlmostOutOfSpace", Query: "y > 1"},
	}}
	out, err := AlertRuleTool{Metrics: f}.Call(context.Background(), `{"alert":"RDSCriticalLatency"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"no alerting rule named", "KubePodCrashLooping", "NodeFilesystemAlmostOutOfSpace"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the recovery list must contain %q:\n%s", want, out)
		}
	}
	wantCalls(t, f, [][]string{{"RDSCriticalLatency"}, nil})
}

// A deadline that expires between the two reads must NOT be reported as "no rule
// found": the unscoped confirmation never happened, so the absence was never
// established. It surfaces as an error, which runTool classifies as the per-tool
// timeout it is (and still renders as this tool's result, so the investigation
// continues).
func TestAlertRuleToolDoesNotConcludeAbsenceWhenTheContextExpires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeRuleMetrics{scoping: scopingBroken, rules: []providers.AlertRule{rdsRule}}
	cancel()
	out, err := AlertRuleTool{Metrics: f}.Call(ctx, `{"alert":"RDSCriticalLatency"}`)
	if err == nil {
		t.Fatalf("an expired context must not be reported as a resolved lookup: %q", out)
	}
	wantCalls(t, f, [][]string{{"RDSCriticalLatency"}})
}
