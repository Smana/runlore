// SPDX-License-Identifier: Apache-2.0

package kbvalidate

import (
	"context"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// The `defect` label vocabulary of runlore_kb_draft_defects_total. The two are
// kept apart because they want opposite responses, and folding them would lose
// the only distinction that matters here: an unusable recall index is silent
// forever, while a draft that fails the merge gate blocks its own pull request
// and a human meets it at review time regardless.
//
// Both name the ENTRY's defect and nothing about the writer that drafted it —
// deliberately, because a thread capture is not a curation and would otherwise
// have no honest value to record under.
const (
	defectUnrecallableResource = "unrecallable_resource"
	defectMergeGate            = "merge_gate"
)

// WarnDraft makes a drafted entry's own defects visible BEFORE the pull request
// exists, and never blocks it: an entry that cannot merge, or cannot be recalled,
// is still knowledge a human can fix — losing the finding over it would be the
// worse trade (#518).
//
// It runs the SAME validator the catalog's CI gate runs (`lore validate-kb` →
// ValidateStructural) rather than restating its rules, which is also what makes
// this package's standing claim — that a RunLore-authored entry passes "by
// construction" — checked instead of merely asserted. Two live drafts proved it
// was not: one carried whitespace in `resource` and sat unmergeable for four days
// before a human noticed, and it is the CI job, four days downstream, that had
// been RunLore's only reader of its own gate.
//
// Then it checks the two recall index keys for a shape the gate does not police
// at all. That is the half that matters more: `resource: argocd/essentials,
// monitoring,argocd-app-of-apps` cleared the gate and merged, and could never
// match anything. providers.EntryResourceRef now repairs that class; this reports
// what repair cannot reach (see DraftResource).
//
// It lives here, and takes a plain KBEntry, because RunLore has TWO entry
// writers and the report is worth exactly as much to each. curator.Curate drafts
// a finding; thread.Responder's standalone-note route drafts a human's
// correction, and on that path an unmergeable entry is the worse failure of the
// two — the human was told in their own thread that it was saved, and it was,
// only never mergeably. Nothing in the report is curator-shaped: an entry's type
// selects its rules (a Concept legitimately has no `resource`, and
// ValidateStructural requires one for Incident only), so both writers are judged
// by what they each claim to be.
//
// It also COUNTS what it reports, under runlore_kb_draft_defects_total, and the
// counter is the half that can be alerted on. A log line is read by whoever is
// already looking; nothing was looking, and runlore_curations_total{result=
// "opened"} scores the pull request a success whether or not the entry inside it
// can ever be recalled — so a deployment steadily filling its catalog with dead
// weight is metrically indistinguishable from a healthy one, which is the worst
// shape this failure can take. The counter is labelled per condition (see the
// defect constants), because the two want different responses.
//
// It stays a REPORT. Nothing here reads back into control flow: the caller opens
// its pull request next either way, and both the counter and the metric set are
// nil-safe, so an unwired telemetry stack costs the report nothing.
//
// log must not be nil in normal use; a nil one falls back to slog.Default()
// rather than panicking on a path whose whole purpose is to not lose an entry.
// m may be nil — the same optional-telemetry contract every other
// *telemetry.Metrics holder in RunLore follows.
func WarnDraft(ctx context.Context, log *slog.Logger, m *telemetry.Metrics, e providers.KBEntry) {
	if log == nil {
		log = slog.Default()
	}
	var mergeGate, unrecallable bool
	// okf.AsEntry, not a projection of our own: the entry is judged as the file
	// that will actually be committed, by the same adapter okf.Render's output
	// parses back into.
	for _, iss := range ValidateStructural(okf.AsEntry(e, okf.Meta{}, "")) {
		// The two messages are separate literals at their own call sites, not one
		// variable logged once. docsguard's troubleshooting guard maps every msg="…"
		// the operations page quotes to a literal at a log call, and a message
		// assembled into a variable is invisible to it — so the page would go on
		// telling operators to grep a string nothing could prove RunLore emits.
		if iss.Severity == SeverityError {
			mergeGate = true
			log.Warn("drafted KB entry fails RunLore's own merge gate; filing it anyway, but the frontmatter needs a human fix before it can merge",
				"field", iss.Field, "issue", iss.Message, "title", e.Title)
			continue
		}
		log.Warn("drafted KB entry: advisory validation warning",
			"field", iss.Field, "issue", iss.Message, "title", e.Title)
	}
	// alert_resource is a SECOND, independent match key (the resource the alert fired
	// on, when the fault sat deeper), so it is held to the same shape as the first.
	for _, k := range []struct{ field, value string }{
		{"resource", e.Resource},
		{"alert_resource", e.AlertResource},
	} {
		if _, reason := DraftResource(k.value); reason != "" {
			unrecallable = true
			log.Warn("drafted KB entry carries a recall index that recall cannot use; filing it anyway",
				"field", k.field, "value", k.value, "reason", reason, "title", e.Title)
		}
	}
	if m == nil {
		return
	}
	// One increment per defect per DRAFT, not per log line: an entry whose two index
	// keys are both unusable is one unusable entry, and an entry that trips four gate
	// rules is one entry that cannot merge. Counting lines would make the number
	// depend on how wrong a draft happened to be, and it could no longer be read
	// against the count of entries filed.
	for _, d := range []struct {
		label   string
		tripped bool
	}{
		{defectUnrecallableResource, unrecallable},
		{defectMergeGate, mergeGate},
	} {
		if d.tripped {
			m.KBDraftDefects.Add(ctx, 1, metric.WithAttributes(attribute.String("defect", d.label)))
		}
	}
}

// DraftResource is the draft path's decision about the `resource:` frontmatter
// field: it returns the value to WRITE, plus the reason that value still cannot
// serve as recall's structural index ("" when it can).
//
// The write side is providers.EntryResourceRef — see it for why a value that
// merely clears the merge gate is not good enough, and for what it repairs.
//
// The reason exists because repair has a hard limit. `resource` is matched by
// string equality against a live workload's "namespace/name" ref, so anything
// else is at best a weaker index and at worst unmatchable — but the draft path
// cannot invent the missing half, and MUST NOT drop the finding over it. So it
// reports, and the caller logs; #518's requirement in one line: an unrecallable
// entry is still better than a lost investigation, as long as it is not silent.
//
// A bare token is deliberately only a warning, not a repair. It is genuinely
// ambiguous: providers.Workload.Ref() renders a bare NAMESPACE when the name is
// unknown (routine on alert-triggered investigations, and recall's matchNamespace
// tier serves it), while a model that wrote a workload name with no namespace
// produces the same shape and will match nothing. Guessing which would either
// mangle a working index or fabricate a namespace.
//
// An empty resource is NOT a defect: it is the honest scopeless entry, and recall
// has a matchScopeless tier for exactly it. Since a NON-EMPTY resource disables
// that tier, a wrong value is strictly worse than none. It is also what an
// ordinary Concept carries — OKF omits `resource` for abstract knowledge — so
// reporting it would fire on every operator note thread capture ever files.
//
// Idempotent — DraftResource(v) for a v it already produced returns v and the same
// reason, which is what lets a caller re-derive the warning from the finished
// entry instead of re-plumbing the raw ref.
func DraftResource(ref string) (resource, reason string) {
	resource = providers.EntryResourceRef(ref)
	if resource == "" {
		return "", "" // legitimately scopeless
	}
	ns, name, ok := strings.Cut(resource, "/")
	switch {
	case !ok:
		return resource, "reads as a bare namespace, so it matches every workload in it rather than one object"
	case !isDNSLabel(ns) || !isDNSSubdomain(name):
		return resource, "is not shaped namespace/name (RFC 1123), so recall's exact match can never agree with it"
	}
	return resource, ""
}

// isDNSLabel and isDNSSubdomain report whether a ref's halves could name a real
// Kubernetes object: a namespace is an RFC 1123 LABEL (lowercase alphanumerics and
// "-"), a name an RFC 1123 SUBDOMAIN (the same, plus "."). Both must start and end
// alphanumeric, and neither may be empty.
//
// The warning is keyed on this ALLOWLIST while EntryResourceRef's repair stays a
// denylist of five observed separators, and the asymmetry is deliberate. Repair is
// destructive, so it only cuts at characters proven impossible; diagnosis is free,
// so it reports everything the charset rules out. Keyed the other way — reporting
// only the five — each new separator a model invented would ship silently until
// someone appended another byte to a string literal, which is the very failure #518
// is about: "argocd/essentials|monitoring" and "tooling/Harbor Registry" both clear
// the merge gate, can never equal a Workload.Ref(), and said nothing.
func isDNSLabel(s string) bool { return isDNS(s, false) }

func isDNSSubdomain(s string) bool { return isDNS(s, true) }

func isDNS(s string, dots bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '.' && dots:
			// Interior punctuation only: the first/last-byte check below rejects a
			// leading or trailing one, which no Kubernetes object may carry either.
		default:
			return false
		}
	}
	return isAlnum(s[0]) && isAlnum(s[len(s)-1])
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}
