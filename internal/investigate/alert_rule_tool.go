// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// AlertRuleTool lets the model read the DEFINITION of the alerting rule that
// fired — above all the expression it thresholds — so a threshold alert is judged
// against the series it actually thresholds instead of a plausible-looking
// neighbour.
//
// It exists because guessing that series wrong inverts the conclusion, and the
// guess is invisible in the write-up. Two real RDSCriticalLatency investigations
// did it: the rule thresholds `aws_rds_*_latency_maximum`, but one read only
// read_latency_maximum (0-1ms), generalised "all latency series are flat" across
// the other three, and filed "no corroborating signal — likely false positive"
// while write_latency_maximum was spiking to 63.7ms; the other quoted
// write_latency_AVERAGE and asserted it "matches the alert exactly". The rule text
// was one read-only request away both times.
//
// It uses the OPTIONAL providers.AlertRuleReader capability and degrades to a plain
// "unavailable" note whenever the rule cannot be read — a missing endpoint, a 404,
// an unmatched name. A rule definition is corroborating context, never the primary
// evidence, so it must never be able to fail an investigation.
//
// registered in internal/app/investigate.go (alongside the query_metrics tools).
type AlertRuleTool struct {
	Metrics providers.MetricsProvider
}

// Name returns the tool name.
func (t AlertRuleTool) Name() string { return "alert_rule" }

// Description returns the tool description.
func (t AlertRuleTool) Description() string {
	return "Read the alerting rule behind a firing alert — the exact PromQL expression it thresholds, " +
		"its `for:` hold-down, labels and annotations. Call this FIRST for any alert-triggered " +
		"investigation, BEFORE querying metrics: a threshold alert names one specific metric AND one " +
		"specific statistic, and a near-miss series does not corroborate or refute it. " +
		"`aws_rds_write_latency_maximum > 0.050` is NOT judged by aws_rds_write_latency_average " +
		"(a _maximum can spike far above its _average, so the average reads calm while the rule " +
		"correctly fires), nor by the read_latency series that merely shares the prefix. " +
		"Likewise an absolute threshold (`> 0.050s`) is not a capacity-relative one " +
		"(`/ limit > 0.9`) — do not substitute one for the other. Query the rule's own series " +
		"over the incident window with query_metrics_range, then judge the alert against it."
}

// Schema returns the JSON schema for the arguments.
func (t AlertRuleTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"alert":{"type":"string","description":"the alertname exactly as it appears on the incident, e.g. RDSCriticalLatency"}},` +
		`"required":["alert"]}`
}

// alertRuleUnavailable prefixes every degraded outcome. It deliberately reuses the
// "<tool> is unavailable" wording the system prompt already teaches the model to
// read as MISSING DATA rather than as evidence, so a backend with no rules endpoint
// cannot be mistaken for "the alert has no rule, therefore it is bogus".
const alertRuleUnavailable = "alert_rule is unavailable"

// statisticWarning is repeated IN the tool result, not only in the Description,
// because the model chooses its next query while reading the result — and the
// result is what a later reviewer sees in the transcript.
const statisticWarning = "→ query the exact metric AND statistic this expression names before judging the alert: " +
	"a _maximum is not its _average (a spiky maximum reads calm in the average), " +
	"a read_* series is not a write_* one, and an absolute threshold is not a capacity-relative one."

// Call reads the rule definition for an alert name, or degrades gracefully.
func (t AlertRuleTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Alert string `json:"alert"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	name := strings.TrimSpace(in.Alert)
	if name == "" {
		return "", fmt.Errorf("provide the `alert` name (the alertname on the incident, e.g. RDSCriticalLatency)")
	}
	reader, ok := t.Metrics.(providers.AlertRuleReader)
	if !ok {
		return alertRuleUnavailable + " — the configured metrics backend serves no alerting-rule endpoint. " +
			"Infer the thresholded series from the alert name and annotations, and say in data_gaps that the " +
			"rule expression could not be read.", nil
	}
	rules, err := reader.AlertRules(ctx)
	if err != nil {
		// A cancelled/expired context is not "this backend has no rules" — it is the
		// loop's own deadline firing. Return it, because runTool only consults
		// tctx.Err() when Call returns an error: swallowing it here would record
		// result="ok" for a call that never completed and hide a per-tool timeout.
		// runTool still turns the error into an "error: …" tool result, so the
		// investigation continues either way.
		if ctx.Err() != nil {
			return "", err
		}
		// Any OTHER failure (404, 500, an unparseable body) is context, never the
		// finding: report it as missing data instead of an error, so it cannot be read
		// as a fault signal about the incident.
		return fmt.Sprintf("%s — reading the alerting rules failed: %v. Treat the rule expression as "+
			"missing data (note it in data_gaps); it is NOT evidence about the incident.", alertRuleUnavailable, err), nil
	}
	if len(rules) == 0 {
		return alertRuleUnavailable + " — the metrics backend returned no alerting rules (it may evaluate " +
			"them elsewhere, e.g. a separate vmalert/Ruler). Note it in data_gaps.", nil
	}
	matched := matchAlertRules(rules, name)
	if len(matched) == 0 {
		return renderUnmatchedAlert(name, rules), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d rule(s) named %q:\n", len(matched), matched[0].Name)
	for i, r := range matched {
		if i > 0 {
			b.WriteString("\n")
		}
		writeAlertRule(&b, r)
	}
	// More than one rule under one name is a trap of its own: the environments'
	// thresholds differ, so picking either at random can invert the verdict.
	if len(matched) > 1 {
		b.WriteString("\nNOTE: several rules share this alertname (per-environment/per-cluster copies) — " +
			"match the incident's own labels to the right one before using its threshold.\n")
	}
	b.WriteString(statisticWarning + "\n")
	return b.String(), nil
}

// matchAlertRules selects the rules firing under name: exact matches when there are
// any, else a case-insensitive pass. The fallback exists because an alertname
// reaches the model through alert prose and gets recased on the way; without it a
// one-character casing difference degrades to "no rule found", which is precisely
// the dead end this tool removes. Exact wins first so a backend that genuinely
// defines two rules differing only in case is never conflated.
func matchAlertRules(rules []providers.AlertRule, name string) []providers.AlertRule {
	var exact, fold []providers.AlertRule
	folded := foldAlertName(name)
	for _, r := range rules {
		switch {
		case r.Name == name:
			exact = append(exact, r)
		case foldAlertName(r.Name) == folded:
			fold = append(fold, r)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return fold
}

// foldAlertName is the ONE case normaliser for alertnames in this package.
//
// It is a function rather than an inline strings.EqualFold / strings.ToLower
// because internal/foldguard forbids normalising the same value two different ways
// in one package: EqualFold and ToLower disagree (EqualFold folds U+017F to 's',
// ToLower does not), so a value that one of them matches the other can miss. Here
// that would mean the exact-vs-fold matcher and the near-miss ranking below
// disagreeing about which names are "the same name" — routing a rule the matcher
// rejected into the list of names to correct to, or vice versa.
func foldAlertName(s string) string { return strings.ToLower(s) }

// writeAlertRule renders one rule: the expression first (it is the reason the tool
// exists), then the metadata that qualifies how it fires.
func writeAlertRule(b *strings.Builder, r providers.AlertRule) {
	fmt.Fprintf(b, "expr: %s\n", r.Query)
	meta := []string{}
	if r.For > 0 {
		meta = append(meta, "for="+r.For.String())
	} else {
		meta = append(meta, "for=0 (fires on the first evaluation)")
	}
	if r.State != "" {
		meta = append(meta, "state="+r.State)
	}
	if r.Health != "" {
		meta = append(meta, "health="+r.Health)
	}
	if r.Group != "" {
		meta = append(meta, "group="+r.Group)
	}
	if r.File != "" {
		meta = append(meta, "file="+r.File)
	}
	fmt.Fprintf(b, "%s\n", strings.Join(meta, " "))
	// A rule that is not evaluating produces no data — which is indistinguishable
	// from a healthy metric sitting at zero, so it must be stated, not inferred.
	if r.LastError != "" {
		fmt.Fprintf(b, "rule evaluation error: %s (this rule is NOT evaluating cleanly)\n", r.LastError)
	}
	// renderKV is the seed prompt's own label/annotation renderer: sorted k="v", and
	// values clipped, so one pathological annotation cannot dominate the context.
	if s := renderKV(r.Labels, ""); s != "" {
		fmt.Fprintf(b, "labels: %s\n", s)
	}
	if s := renderKV(r.Annotations, ""); s != "" {
		fmt.Fprintf(b, "annotations: %s\n", s)
	}
}

// renderUnmatchedAlert reports a name that matched no rule AND lists the alertnames
// the backend does define, so the model can correct the name instead of dead-ending
// — the same recover-don't-guess shape as noSeriesMatched.
//
// The list is capped at maxToolRows, so plain alphabetical order would bury the one
// name the model needs: a real backend here defines 254 alertnames and sorts
// "RDSCriticalLatency" at index 161, which the 50-row cap can never reach. Names
// that look like the requested one are therefore hoisted first, which is what makes
// the cap harmless on the path that exists to recover from a wrong name.
func renderUnmatchedAlert(name string, rules []providers.AlertRule) string {
	seen := make(map[string]bool, len(rules))
	for _, r := range rules {
		if r.Name != "" {
			seen[r.Name] = true
		}
	}
	folded := foldAlertName(name)
	var near, rest []string
	for _, n := range slices.Sorted(maps.Keys(seen)) {
		// Either direction of containment: the model may have dropped a suffix
		// ("RDSCriticalLatency" for "RDSCriticalLatencyProd") or added one.
		if f := foldAlertName(n); strings.Contains(f, folded) || strings.Contains(folded, f) {
			near = append(near, n)
		} else {
			rest = append(rest, n)
		}
	}
	names := slices.Concat(near, rest)
	var b strings.Builder
	fmt.Fprintf(&b, "no alerting rule named %q on this backend — the alert may be evaluated elsewhere, or the "+
		"name may differ. Do NOT read this as the alert being bogus; note it in data_gaps if it stays unresolved.\n"+
		"%d alertname(s) this backend does define (closest first):\n", name, len(names))
	renderRows(&b, len(names), "more", func(i int) {
		fmt.Fprintf(&b, "%s\n", names[i])
	})
	return b.String()
}
