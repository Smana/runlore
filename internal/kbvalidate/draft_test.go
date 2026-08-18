// SPDX-License-Identifier: Apache-2.0

package kbvalidate

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestDraftResourceShape is the draft-time gate on the one frontmatter field
// recall matches structurally. Two live drafts (#518) shipped a `resource` that
// could not work, in the two opposite ways:
//
//   - "observability/ip-10-11-189-250.ec2.internal (cluster=shared, instance
//     i-0fd8c3c351590a3a0)" — whitespace, so the catalog's CI gate rejected it
//     loudly and the PR sat unmergeable for four days.
//   - "argocd/essentials,monitoring,argocd-app-of-apps" — no whitespace, so it
//     PASSED the gate and merged, yet recall compares `resource` by string
//     equality: no incident's workload is ever that string, so the entry was
//     dead on arrival with nothing to surface it.
//
// Both must be settled here, at draft time, before the pull request exists — for
// EVERY entry writer, which is why the pairing lives in this package rather than
// in the curator that first needed it. The table pins what gets WRITTEN and
// whether the draft path has anything to say about it — repaired silently when
// the leading namespace/name token is unambiguous, warned about when it is not.
func TestDraftResourceShape(t *testing.T) {
	tests := []struct {
		name         string
		ref          string
		wantResource string
		wantReason   string // substring; "" means the value is usable as-is
	}{{
		name:         "a valid namespace/name is written untouched and needs no warning",
		ref:          "tooling/harbor-registry",
		wantResource: "tooling/harbor-registry",
	}, {
		name:         "the live whitespace+parenthetical draft keeps only the identifier",
		ref:          "observability/ip-10-11-189-250.ec2.internal (cluster=shared, instance i-0fd8c3c351590a3a0)",
		wantResource: "observability/ip-10-11-189-250.ec2.internal",
	}, {
		name:         "the live comma-joined list keeps its first object, which is a real one",
		ref:          "argocd/essentials,monitoring,argocd-app-of-apps",
		wantResource: "argocd/essentials",
	}, {
		name:         "a bare name with no namespace cannot be repaired, so it is warned about",
		ref:          "harbor-registry",
		wantResource: "harbor-registry",
		wantReason:   "bare namespace",
	}, {
		name:         "empty is legitimately scopeless — no value, no warning",
		ref:          "",
		wantResource: "",
	}, {
		// recall's resourceAgrees normalizes the pod-hash suffix off BOTH sides before
		// comparing, so a hash-bearing ref still matches. It must not be warned about
		// and must not be mangled: it is a well-formed namespace/name.
		name:         "a pod-template-hash suffix stays acceptable",
		ref:          "tooling/harbor-registry-59598dbd57-ltkzw",
		wantResource: "tooling/harbor-registry-59598dbd57-ltkzw",
	}, {
		name:         "a ref with more than one slash is not a namespace/name",
		ref:          "tooling/harbor/registry",
		wantResource: "tooling/harbor/registry",
		wantReason:   "namespace/name",
	}, {
		name:         "an empty namespace half is not a namespace/name either",
		ref:          "/harbor-registry",
		wantResource: "/harbor-registry",
		wantReason:   "namespace/name",
	}, {
		// The class the shape check missed while it only counted slashes: each of the
		// four below clears the merge gate (no whitespace) and carries no character
		// EntryResourceRef cuts at, yet none can ever equal a Workload.Ref(). Before
		// the charset rule they shipped with no warning at all — #518's own worse half.
		name:         "an uppercase name can never name a Kubernetes object",
		ref:          "tooling/Harbor",
		wantResource: "tooling/Harbor",
		wantReason:   "namespace/name",
	}, {
		name:         "an underscore is not legal in a Kubernetes name",
		ref:          "tooling/harbor_registry",
		wantResource: "tooling/harbor_registry",
		wantReason:   "namespace/name",
	}, {
		name:         "a separator the cut does not know about is still reported",
		ref:          "argocd/essentials|monitoring",
		wantResource: "argocd/essentials|monitoring",
		wantReason:   "namespace/name",
	}, {
		name:         "a trailing dot is not a legal name either",
		ref:          "tooling/harbor.",
		wantResource: "tooling/harbor.",
		wantReason:   "namespace/name",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := DraftResource(tt.ref)
			if got != tt.wantResource {
				t.Errorf("DraftResource(%q) resource = %q, want %q", tt.ref, got, tt.wantResource)
			}
			switch {
			case tt.wantReason == "" && reason != "":
				t.Errorf("DraftResource(%q) warned %q, want no warning", tt.ref, reason)
			case tt.wantReason != "" && !strings.Contains(reason, tt.wantReason):
				t.Errorf("DraftResource(%q) warned %q, want it to mention %q", tt.ref, reason, tt.wantReason)
			}
			// The reason is derived from the value that ships, so re-deriving it from
			// that value must give the same answer — that is what lets the curator warn
			// from the finished entry rather than from the raw ref.
			if again, againReason := DraftResource(got); again != got || againReason != reason {
				t.Errorf("DraftResource is not idempotent: %q → (%q, %q) → (%q, %q)", tt.ref, got, reason, again, againReason)
			}
		})
	}
}

// TestWarnDraftReportsBothHalvesOfADefectiveDraft pins what WarnDraft is for:
// the merge gate the catalog's CI runs, AND the recall-index shape that gate does
// not police at all. One entry carries a defect of each kind, because a report
// that only ever ran one of the two loops would pass a test that carried only one.
func TestWarnDraftReportsBothHalvesOfADefectiveDraft(t *testing.T) {
	var logs bytes.Buffer
	WarnDraft(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), nil, providers.KBEntry{
		// An Incident with no resource fails the gate; the alert-side index clears
		// it (no whitespace) yet can never equal a Workload.Ref().
		Type: "Incident", Title: "Core Argo CD Applications stuck OutOfSync",
		Description:   "the ApplicationSet lost syncPolicy.automated",
		AlertResource: "argocd/essentials|monitoring", Tags: []string{"runlore", "incident"},
		Body: "## Symptom\n\ns\n\n## Cause\n\nc\n\n## Resolution\n\nr\n",
	})

	out := logs.String()
	for _, want := range []string{"merge gate", "recall index", "argocd/essentials|monitoring", "alert_resource"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report never mentioned %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("WarnDraft reports, it never fails the write; got:\n%s", out)
	}
}

// TestWarnDraftIsSilentOnACleanConcept is the type distinction, checked at the
// level that owns it. A Concept is abstract knowledge: OKF omits `resource` for
// it and ValidateStructural requires one for Incident only, so the entry thread
// capture files — a human's note, with no workload of its own — must draw no
// complaint at all. Warning here would fire on every note ever captured, which is
// indistinguishable from not warning.
func TestWarnDraftIsSilentOnACleanConcept(t *testing.T) {
	var logs bytes.Buffer
	WarnDraft(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), nil, providers.KBEntry{
		Type: "Concept", Title: "Operator note: this recurs after every spot reclaim",
		Description: "Operator knowledge from @alice via slack",
		Tags:        []string{"operator-note", "slack"},
		Body:        "alice: this recurs after every spot reclaim\n",
	})
	if out := logs.String(); out != "" {
		t.Errorf("a clean Concept must produce no report at all; got:\n%s", out)
	}
}

// TestWarnDraftSurvivesANilLogger: the one thing this function must never do is
// cost the caller the entry it was about to file, and a panic on a missing logger
// would do exactly that on the path whose whole premise is that a defective entry
// still beats a lost one.
func TestWarnDraftSurvivesANilLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// An entry with something to report, so the fallback logger is actually
	// WRITTEN to: a clean one would never reach a log call and would pass with or
	// without the guard.
	WarnDraft(context.Background(), nil, nil, providers.KBEntry{Type: "Incident", Title: "t", AlertResource: "argocd/a|b"})
}

// draftDefectsSeries is the exported name of the counter WarnDraft writes. It is
// spelled out rather than derived, because the series name is the operator's
// interface: a rename that nothing here notices is a dashboard and an alert rule
// pointed at nothing.
const draftDefectsSeries = "runlore_kb_draft_defects_total"

// meteredDefects installs a REAL SDK meter provider backed by a manual reader and
// returns the instrument set bound to it, plus a reader that totals
// draftDefectsSeries BY ITS `defect` LABEL.
//
// By label, not in aggregate, deliberately: the two conditions want different
// responses from an operator — an unusable recall index is silent forever, while
// a merge-gate failure blocks the PR and a human will see it — so a test that
// summed the series whole would pass with the labels folded together, which is
// the one thing this metric must not do. The provider is global, so a test using
// this must not run in parallel with another that does; the cleanup restores the
// no-op provider. Mirrors internal/thread's meteredInstruments.
func meteredDefects(t *testing.T) (*telemetry.Metrics, func() map[string]int64) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func() map[string]int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		byDefect := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != draftDefectsSeries {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("%s is not an int64 sum (%T)", draftDefectsSeries, md.Data)
				}
				for _, dp := range sum.DataPoints {
					v, found := dp.Attributes.Value("defect")
					if !found {
						t.Fatalf("%s carries a data point with no `defect` label — an unlabelled "+
							"series folds the two conditions back together, which is what the label is for", draftDefectsSeries)
					}
					byDefect[v.AsString()] += dp.Value
				}
			}
		}
		return byDefect
	}
}

// TestWarnDraftCountsEachConditionUnderItsOwnLabel is the whole point of the
// counter. WarnDraft already logs both conditions, and a log line is not a number:
// runlore_curations_total{kind="pr",result="opened"} counts the pull request as a
// success whether the entry it carries is recallable or not, so a deployment
// steadily filling its catalog with unmatchable entries looks exactly like a
// healthy one. The damage compounds — every unusable entry is permanent catalog
// weight that never repays its cost — which is why it has to be alertable, and
// alertable per condition: an unusable recall index is silent forever, while a
// merge-gate failure blocks at PR time where a human meets it anyway.
func TestWarnDraftCountsEachConditionUnderItsOwnLabel(t *testing.T) {
	m, defects := meteredDefects(t)
	var logs bytes.Buffer

	// One entry with a defect of each kind — an Incident with no `resource` fails
	// the gate, while its alert-side index clears the gate (no whitespace) and can
	// never equal a Workload.Ref().
	WarnDraft(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), m, providers.KBEntry{
		Type: "Incident", Title: "Core Argo CD Applications stuck OutOfSync",
		Description:   "the ApplicationSet lost syncPolicy.automated",
		AlertResource: "argocd/essentials|monitoring", Tags: []string{"runlore", "incident"},
		Body: "## Symptom\n\ns\n\n## Cause\n\nc\n\n## Resolution\n\nr\n",
	})

	want := map[string]int64{"unrecallable_resource": 1, "merge_gate": 1}
	if got := defects(); !reflect.DeepEqual(got, want) {
		t.Errorf("%s by defect = %v, want %v", draftDefectsSeries, got, want)
	}
	// Counting is ADDED to the report, never substituted for it: the log lines are
	// what the troubleshooting page tells an operator to grep once the metric has
	// told them to go looking.
	if !strings.Contains(logs.String(), "merge gate") || !strings.Contains(logs.String(), "recall index") {
		t.Errorf("the counter must not replace the log lines an operator greps; got:\n%s", logs.String())
	}
}

// TestWarnDraftCountsOnePerDraftNotOnePerDefectiveField pins the counter's unit:
// one DRAFT, per condition. An entry with two unusable index keys is still one
// unusable entry, and an entry that trips four gate rules is still one entry that
// cannot merge — so counting log lines instead would make the number depend on how
// wrong a draft happens to be, and it could no longer be read against the count of
// entries filed.
func TestWarnDraftCountsOnePerDraftNotOnePerDefectiveField(t *testing.T) {
	for _, tt := range []struct {
		name  string
		entry providers.KBEntry
		want  map[string]int64
	}{{
		name: "both recall index keys are unusable, and both clear the merge gate",
		entry: providers.KBEntry{
			Type: "Incident", Title: "Core Argo CD Applications stuck OutOfSync",
			Description: "the ApplicationSet lost syncPolicy.automated",
			// Neither carries whitespace, so ValidateStructural passes both; neither is
			// shaped namespace/name, so recall can never match either.
			Resource: "argocd/essentials|monitoring", AlertResource: "tooling/Harbor|Registry",
			Tags: []string{"runlore", "incident"},
			Body: "## Symptom\n\ns\n\n## Cause\n\nc\n\n## Resolution\n\nr\n",
		},
		want: map[string]int64{"unrecallable_resource": 1},
	}, {
		name: "several merge-gate rules fail on one entry with a clean index",
		entry: providers.KBEntry{
			// No type, no title and no description: three separate gate errors.
			Resource: "tooling/harbor-registry", Tags: []string{"runlore"},
			Body: "## Symptom\n\ns\n\n## Cause\n\nc\n\n## Resolution\n\nr\n",
		},
		want: map[string]int64{"merge_gate": 1},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			m, defects := meteredDefects(t)
			WarnDraft(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), m, tt.entry)
			if got := defects(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("%s by defect = %v, want %v", draftDefectsSeries, got, tt.want)
			}
		})
	}
}

// TestWarnDraftCountsNothingForACleanDraft keeps the counter honest in the same
// way TestWarnDraftIsSilentOnACleanConcept keeps the log honest. A metric that
// ticks on every entry ever filed cannot be alerted on, and an operator note is
// legitimately a Concept with no `resource` at all.
func TestWarnDraftCountsNothingForACleanDraft(t *testing.T) {
	m, defects := meteredDefects(t)
	WarnDraft(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), m, providers.KBEntry{
		Type: "Concept", Title: "Operator note: this recurs after every spot reclaim",
		Description: "Operator knowledge from @alice via slack",
		Tags:        []string{"operator-note", "slack"},
		Body:        "alice: this recurs after every spot reclaim\n",
	})
	if got := defects(); len(got) != 0 {
		t.Errorf("a clean draft must record nothing at all; got %v", got)
	}
}

// TestWarnDraftWithoutMetricsStillReports is the nil-safety contract every other
// optional *telemetry.Metrics in RunLore follows, on the one path that must never
// cost the caller its entry. Telemetry not being wired is an ordinary
// configuration, and it must leave the report — and the filing — untouched.
func TestWarnDraftWithoutMetricsStillReports(t *testing.T) {
	var logs bytes.Buffer
	WarnDraft(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), nil, providers.KBEntry{
		Type: "Incident", Title: "t", AlertResource: "argocd/a|b",
	})
	if !strings.Contains(logs.String(), "recall index") {
		t.Errorf("a nil metric set must not cost the report; got:\n%s", logs.String())
	}
}
