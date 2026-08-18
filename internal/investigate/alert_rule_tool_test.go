// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// fakeRuleMetrics implements providers.MetricsProvider plus the OPTIONAL
// providers.AlertRuleReader capability, so the tool's happy path and its error
// path are both exercised against a real type assertion.
type fakeRuleMetrics struct {
	rules []providers.AlertRule
	err   error
	calls int
}

func (f *fakeRuleMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return nil, nil
}

func (f *fakeRuleMetrics) QueryRange(context.Context, string, providers.TimeWindow, time.Duration) (providers.Matrix, error) {
	return nil, nil
}

func (f *fakeRuleMetrics) LabelValues(context.Context, string, []string, providers.TimeWindow) ([]string, error) {
	return nil, nil
}

func (f *fakeRuleMetrics) AlertRules(context.Context) ([]providers.AlertRule, error) {
	f.calls++
	return f.rules, f.err
}

// plainMetrics implements ONLY providers.MetricsProvider — a backend with no rules
// endpoint (e.g. a bare remote-read gateway).
type plainMetrics struct{}

func (plainMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return nil, nil
}

func (plainMetrics) QueryRange(context.Context, string, providers.TimeWindow, time.Duration) (providers.Matrix, error) {
	return nil, nil
}

func (plainMetrics) LabelValues(context.Context, string, []string, providers.TimeWindow) ([]string, error) {
	return nil, nil
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
			name: "name match is case-insensitive so a reworded alertname still resolves",
			provider: &fakeRuleMetrics{rules: []providers.AlertRule{rdsRule,
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
			provider: &fakeRuleMetrics{rules: []providers.AlertRule{
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
			name:     "backend with no rules endpoint degrades to unavailable",
			provider: plainMetrics{},
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
	without := (&LoopInvestigator{Tools: []Tool{QueryMetricsTool{Metrics: plainMetrics{}}}}).system()
	if strings.Contains(without, "alert_rule") {
		t.Fatalf("prompt must not mention alert_rule when the tool is absent:\n%s", without)
	}
	with := (&LoopInvestigator{Tools: []Tool{
		QueryMetricsTool{Metrics: plainMetrics{}},
		AlertRuleTool{Metrics: &fakeRuleMetrics{}},
	}}).system()
	if !strings.Contains(with, "alert_rule") {
		t.Fatalf("prompt must direct the model at alert_rule when registered:\n%s", with)
	}
}
