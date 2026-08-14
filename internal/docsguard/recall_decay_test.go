// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// ledgerSourcePath is the Go file whose two doc comments state the same
// exclusion rule the learning-loop page states — a third site drifts as easily
// as the first, and all three drifted together once already.
const ledgerSourcePath = "../outcome/ledger.go"

// The three sites below all describe WHICH incident sources are excluded from
// resolve-based recall decay. They shipped a claim the code never implemented:
// that an Alertmanager receiver with send_resolved off is excluded. It cannot be —
// the exclusion is read off the fingerprint (`resolvable := !outcome.Derived(fp)`)
// and send_resolved lives in the operator's receiver config, which RunLore never
// reads. The claim mattered: an operator reading it would expect a frozen entry,
// while the shipped code decays that entry below the floor and stops recalling it.
//
// So both directions of the claim are phrase-anchored here and the truth is
// MEASURED — by driving a real outcome.Ledger through both fingerprint shapes and
// the real recall gate over the result. If someone ever does implement the
// exclusion (a heuristic over recalls-with-no-resolves), the measurement flips and
// these guards demand the opposite prose instead of quietly passing.
var (
	pageExcludesAlertmanagerRE = regexp.MustCompile("Alertmanager without `send_resolved`")
	pageCountsAlertmanagerRE   = regexp.MustCompile(`\*\*An Alertmanager alert is never in that excluded set\*\*`)
	ledgerExcludesAlertmanager = regexp.MustCompile(`Alertmanager with send_resolved off\)`)
	ledgerCountsAlertmanagerRE = regexp.MustCompile(`Alertmanager receiver with send_resolved off`)
	// The second error in the same sentence: an excluded entry was said to sit at
	// the prior forever. It never enters the roll-up at all, so the gate's fail-safe
	// applies instead — a strictly MORE trusting value than the prior.
	pagePriorFreezeRE = regexp.MustCompile(`frozen at the prior`)
)

// recallDecayFacts is what driving the real ledger + the real recall gate says
// about the two fingerprint shapes. Every field is measured, never asserted.
type recallDecayFacts struct {
	prior, floor float64 // the shipped catalog.instant_recall defaults
	realAgg      outcome.Aggregate
	realCounted  bool    // a REAL alert fingerprint's recall entered OpenCounts
	realFactor   float64 // its decay factor after ONE unresolved recall
	priorMean    float64 // Factor of an empty aggregate — what "frozen at the prior" meant
	derivedNames []string
	derived      map[string]bool // synthetic prefix -> did its recall enter OpenCounts
}

// measureRecallDecay records ONE unresolved recall per fingerprint shape in a real
// outcome.Ledger, exactly the way the delivery path does (internal/app/investigate.go
// stamps Resolvable from `!outcome.Derived(fp)`), then reads the roll-up back. This is
// the half that must fail first: a change to the Derived rule or to Aggregate.Factor
// moves these numbers before any prose is consulted.
func measureRecallDecay(t *testing.T) recallDecayFacts {
	t.Helper()
	var c config.Config
	c.Catalog.InstantRecall.Enabled = true
	config.ApplyDefaults(&c)
	ir := c.Catalog.InstantRecall

	l, err := outcome.New(filepath.Join(t.TempDir(), "outcomes.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	const realEntry = "real-alert.md"
	openUnresolvedRecall(t, l, realFingerprint, realEntry)

	f := recallDecayFacts{
		prior: ir.OutcomePrior, floor: ir.OutcomeFloor,
		priorMean: outcome.Aggregate{}.Factor(ir.OutcomePrior),
		derived:   map[string]bool{},
	}
	for _, prefix := range []string{outcome.GitOpsFingerprintPrefix, outcome.ReinvestigateFingerprintPrefix} {
		entry := prefix + "entry.md"
		f.derivedNames = append(f.derivedNames, prefix)
		openUnresolvedRecall(t, l, outcome.DeriveFingerprint(prefix, "apps/worker|HealthCheckFailed"), entry)
		f.derived[prefix] = entryOf(t, l, entry)
	}
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	f.realAgg, f.realCounted = counts[realEntry]
	f.realFactor = f.realAgg.Factor(f.prior)
	return f
}

// realFingerprint is an Alertmanager-shaped fingerprint: the 16 hex chars
// Alertmanager sends, carrying none of the synthetic prefixes.
const realFingerprint = "9f2a7b1c4d5e6f70"

// openUnresolvedRecall appends one recall open and NO resolve, deriving the
// Resolvable flag the way the delivery path does rather than hard-coding it.
func openUnresolvedRecall(t *testing.T, l *outcome.Ledger, fp, entry string) {
	t.Helper()
	resolvable := !outcome.Derived(fp)
	if err := l.Open(outcome.Event{
		Fingerprint: fp, Kind: "recall", Entry: entry, Resolvable: &resolvable, At: time.Now(),
	}); err != nil {
		t.Fatalf("ledger.Open(%s): %v", fp, err)
	}
}

// entryOf reports whether an entry made it into the roll-up recall decay reads.
func entryOf(t *testing.T, l *outcome.Ledger, entry string) bool {
	t.Helper()
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	_, ok := counts[entry]
	return ok
}

// TestRecallDecayExclusionIsFingerprintShaped measures which sources resolve-based
// decay actually excludes, then holds all three prose sites to the measurement in
// both directions. The measured premise comes first on purpose: a rewrite of the
// `resolvable` rule fails here before any doc phrase is looked at.
func TestRecallDecayExclusionIsFingerprintShaped(t *testing.T) {
	f := measureRecallDecay(t)

	// The synthetic shapes are excluded — that half of the claim was always true.
	for _, prefix := range f.derivedNames {
		if f.derived[prefix] {
			t.Errorf("a %q recall entered the decay roll-up: the synthetic fingerprints are "+
				"supposed to be excluded, so every doc site describing the exclusion is now wrong", prefix)
		}
	}
	// And the half that was not: a real alert fingerprint is counted regardless of
	// whether a resolve ever follows, because nothing in the ledger can see
	// send_resolved.
	if !f.realCounted {
		t.Fatalf("a real alert fingerprint's recall no longer enters the decay roll-up (agg %+v) — "+
			"if the send_resolved exclusion was implemented, the three sites below must be "+
			"rewritten to claim it, not left silently correct-by-accident", f.realAgg)
	}
	if f.realAgg.Recalls != 1 || f.realAgg.Resolved != 0 {
		t.Fatalf("fixture drifted: one unresolved recall produced %+v, want Recalls=1 Resolved=0", f.realAgg)
	}

	for _, site := range []struct {
		name, path       string
		excludes, counts *regexp.Regexp
	}{
		{"learning-loop.md §6", learningLoopPath, pageExcludesAlertmanagerRE, pageCountsAlertmanagerRE},
		{"outcome/ledger.go", ledgerSourcePath, ledgerExcludesAlertmanager, ledgerCountsAlertmanagerRE},
	} {
		prose := flattenProse(readDoc(t, site.path))
		if site.excludes.MatchString(prose) {
			t.Errorf("%s lists an Alertmanager receiver with send_resolved off among the sources "+
				"excluded from resolve-based decay, but a real alert fingerprint IS counted (%+v, "+
				"factor %.3f) — the exclusion is drawn on the fingerprint and RunLore never reads "+
				"send_resolved", site.name, f.realAgg, f.realFactor)
		}
		if !site.counts.MatchString(prose) {
			t.Errorf("%s no longer states that an Alertmanager alert is NOT excluded from decay "+
				"(want %v) — that is the claim the measurement supports, and dropping it is how "+
				"the wrong one came back last time", site.name, site.counts)
		}
		// Both sites must still name the pair that IS excluded, so a rewrite cannot
		// leave the exclusion undescribed.
		for _, want := range []string{"GitOps", "reinvestigate"} {
			if !strings.Contains(strings.ToLower(prose), strings.ToLower(want)) {
				t.Errorf("%s no longer names %q as an excluded source", site.name, want)
			}
		}
	}
	// ledger.go carries the claim in TWO comments (Event.Resolvable and
	// applyOpenLocked); one of them silently losing it is exactly how three sites
	// drift into two.
	if n := len(ledgerCountsAlertmanagerRE.FindAllString(flattenProse(readDoc(t, ledgerSourcePath)), -1)); n < 2 {
		t.Errorf("outcome/ledger.go states the Alertmanager case in %d place(s), want both "+
			"Event.Resolvable and applyOpenLocked", n)
	}
}

// TestLearningLoopQuotesTheMeasuredDecayNumbers pins §6's three numbers to the values
// the real ledger and the shipped defaults produce. They are the numbers an operator
// reasons about — "how many unresolved recalls until my entry stops firing?" — so a
// retuned prior/floor or a reworked Factor must not leave the page quoting the old ones.
func TestLearningLoopQuotesTheMeasuredDecayNumbers(t *testing.T) {
	f := measureRecallDecay(t)
	if f.realFactor >= f.floor {
		t.Fatalf("one unresolved recall now yields factor %.3f, at or above the %g floor — §6 says "+
			"ONE recall already stops the entry firing; rewrite it for the new calibration", f.realFactor, f.floor)
	}
	page := flattenProse(readDoc(t, learningLoopPath))
	for _, want := range []string{
		fmt.Sprintf("lands the entry at **%.3f**", f.realFactor),
		fmt.Sprintf("`outcome_floor` of **%g**", f.floor),
		fmt.Sprintf("**not** the %g prior mean", f.priorMean),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("learning-loop.md §6 must state the measured value verbatim as %q", want)
		}
	}
}

// TestRecallDecayGateFailSafeIsFullTrustNotThePrior measures what the REAL recall gate
// does with each fingerprint shape, over the real Catalog + Recall + Ledger stack.
//
// It is the half of §6 the ledger alone cannot show. An excluded entry never enters the
// roll-up, so the gate never computes a factor for it and takes its fail-safe branch
// (absence of evidence must never block a recall). "Frozen at the prior" would mean
// 0.5 — which the floor could still reject — so the two are told apart by raising the
// floor ABOVE every factor the formula can ever return: a gate that still fires is one
// that never consulted a factor at all.
func TestRecallDecayGateFailSafeIsFullTrustNotThePrior(t *testing.T) {
	f := measureRecallDecay(t)

	// The formula's own ceiling: Factor is a posterior mean and never reaches 1, so a
	// floor above 1 rejects every entry the roll-up knows about.
	if ceiling := (outcome.Aggregate{Recalls: 1000, Resolved: 1000}).Factor(f.prior); ceiling >= 1 {
		t.Fatalf("Aggregate.Factor now reaches %v ≥ 1; the above-the-ceiling floor below no longer discriminates", ceiling)
	}
	unreachableFloor := 1.0000001

	derivedFP := outcome.DeriveFingerprint(outcome.GitOpsFingerprintPrefix, "apps/worker|HealthCheckFailed")
	if !recallFires(t, derivedFP, unreachableFloor, true) {
		t.Errorf("a recall whose only history is an excluded (synthetic) fingerprint was rejected at "+
			"floor %v — it should never reach a factor at all, so §6's claim of the 1.0 fail-safe "+
			"(and not the %g prior) is now wrong", unreachableFloor, f.priorMean)
	}
	if recallFires(t, realFingerprint, f.floor, true) {
		t.Errorf("a recall with ONE unresolved real-fingerprint history (factor %.3f) still fires at "+
			"the shipped floor %g on a deployment whose resolve channel is PROVEN live — §6 says "+
			"instant recall stops firing it", f.realFactor, f.floor)
	}
	// The other half of §6's qualification, and the live bug it was written for: the
	// SAME history on a deployment that has never delivered a resolve must still fire.
	// Counting it decays a correct entry on evidence the source cannot provide, which
	// locked instant recall out after a single use (measured: one recall ever, then the
	// same alert re-investigated from scratch three times in 12h).
	if !recallFires(t, realFingerprint, f.floor, false) {
		t.Errorf("a recall with ONE unresolved real-fingerprint history was rejected at floor %g on a "+
			"deployment where NO resolve has ever arrived — with no resolve channel the silence is "+
			"not evidence of failure, and decaying on it disables the entry permanently", f.floor)
	}
	page := flattenProse(readDoc(t, learningLoopPath))
	if pagePriorFreezeRE.MatchString(page) {
		t.Errorf("learning-loop.md §6 still says an excluded entry's trust is frozen at the prior; "+
			"it never enters the roll-up, so the gate hands it the fail-safe, not the %g prior mean", f.priorMean)
	}
	if !strings.Contains(page, "which is **1.0**") {
		t.Error("learning-loop.md §6 must state the gate's fail-safe value for an excluded entry as **1.0**")
	}
}

// recallFires drives the REAL instant-recall gate: a one-entry catalog, a real
// outcome.Ledger holding one unresolved recall under the given fingerprint, and the
// exported LoopInvestigator entry point. It reports whether the gate fired. The
// magnitude gates are tuned low (a one-entry corpus scores small) so the only gate
// that can reject here is outcome decay — proven by the ledger-less baseline, which
// must fire.
//
// resolveChannelLive seeds the ledger with one resolve on an UNRELATED fingerprint.
// That resolve credits nothing (it belongs to no open here) — it exists only to prove
// the deployment's resolve channel works, which is the precondition resolve-based decay
// now requires: silence after a recall is evidence of failure only where a resolve
// could have arrived. Without it the fixture is indistinguishable from an Alertmanager
// receiver running send_resolved:false, where decay is deliberately withheld.
func recallFires(t *testing.T, fp string, floor float64, resolveChannelLive bool) bool {
	t.Helper()
	cat := decayGuardCatalog(t)
	entry := cat.Entries()[0].Path
	if !runRecall(t, cat, nil, floor) {
		t.Fatalf("baseline recall with no ledger history must fire at floor %v; the fixture no longer "+
			"clears the structural/margin gates, so this guard would measure nothing", floor)
	}
	l, err := outcome.New(filepath.Join(t.TempDir(), "outcomes.jsonl"))
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	openUnresolvedRecall(t, l, fp, entry)
	if resolveChannelLive {
		if _, _, err := l.Resolve("0000deadbeef0000", time.Now()); err != nil {
			t.Fatalf("ledger.Resolve (channel-liveness seed): %v", err)
		}
		if !l.ResolveChannelLive() {
			t.Fatal("seeding one resolve must mark the channel live; the liveness signal has moved")
		}
	}
	return runRecall(t, cat, l, floor)
}

// runRecall runs one investigation and reports whether instant recall fired.
func runRecall(t *testing.T, cat *catalog.Catalog, stats investigate.OutcomeStats, floor float64) bool {
	t.Helper()
	var fired bool
	li := &investigate.LoopInvestigator{
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Model:    silentModel{},
		MaxSteps: 1,
		Recall: &investigate.Recall{
			Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
			Outcome: stats, OutcomePrior: 2.0, OutcomeFloor: floor,
		},
		OnRecall: func(d investigate.RecallDecision) { fired = d.Fired },
	}
	if stats == nil {
		li.Recall.Outcome = nil
	}
	// A rejected recall falls through to the ReAct loop, which needs a model; the
	// stub concludes nothing, so the run ends without a finding. Only the recall
	// decision is read, and it is emitted before the loop starts.
	if err := li.Investigate(context.Background(), investigate.Request{
		Title:    "WorkerOOM",
		Message:  "apps/worker pods OOMKilled after memory limit drop",
		Workload: providers.Workload{Namespace: "apps", Name: "worker"},
	}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	return fired
}

// silentModel is a ModelProvider that concludes nothing — enough for the
// fall-through path a rejected recall takes, without a network call.
type silentModel struct{}

func (silentModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	return providers.CompletionResponse{Text: "inconclusive"}, nil
}

// decayGuardCatalog is a one-entry catalog whose stored resource agrees with the
// request workload, so the structural and margin gates pass and outcome decay is
// the only gate under measurement.
func decayGuardCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	const entry = `---
type: Incident
title: worker OOMKilled after memory limit drop
description: apps/worker pods OOMKilled; raise the container memory limit
resource: apps/worker
tags: [oom, memory, worker]
---

# Symptom
apps/worker pods are OOMKilled shortly after a values change lowered the memory limit.
`
	if err := os.WriteFile(filepath.Join(dir, "worker-oom.md"), []byte(entry), 0o600); err != nil {
		t.Fatalf("seed catalog entry: %v", err)
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return cat
}

// goCommentPrefixRE strips the leading "// " of a Go comment continuation line.
var goCommentPrefixRE = regexp.MustCompile(`(?m)^[ \t]*// ?`)

// flattenProse collapses a page or a Go source file into single-spaced text, so a
// phrase anchor survives re-wrapping: a doc comment's "\n\t// " continuation and a
// markdown paragraph's hard wrap both become one space. Without it every anchor
// would also be pinning where the author happened to break the line — and the
// claims these guards check have already been re-wrapped more than once.
func flattenProse(s string) string {
	return strings.Join(strings.Fields(goCommentPrefixRE.ReplaceAllString(s, " ")), " ")
}

// TestRecallDecayClaimREsFlip is the mutation test for the anchors above: each must
// fire on the wording that actually shipped, stay quiet on the wording that replaced
// it, and neither may be satisfied by the page's other prose about resolve channels.
// Inputs are pre-flattened, mirroring how the guards read their files.
func TestRecallDecayClaimREsFlip(t *testing.T) {
	const (
		shippedPageClaim = "sources with no resolve channel (**GitOps failures**, reinvestigate polls, " +
			"Alertmanager without `send_resolved`) are deliberately excluded from resolve-based decay, " +
			"so without feedback their entries' trust is frozen at the prior forever."
		correctedPageClaim = "**An Alertmanager alert is never in that excluded set** — not even when its " +
			"receiver has `send_resolved` off. Resolvability is read off the fingerprint alone."
		shippedLedgerClaim = "false for sources that never emit one (GitOps, reinvestigate, or " +
			"Alertmanager with send_resolved off). A pointer for a three-state distinction:"
		correctedLedgerClaim = "An Alertmanager receiver with send_resolved off is NOT excluded and cannot be: " +
			"send_resolved lives in the operator's receiver config, which RunLore never reads."
		unrelated = "false for sources that never emit one (GitOps, reinvestigate) — no resolved-alert " +
			"webhook can ever match a synthetic fingerprint."
	)
	cases := []struct {
		name, text                                       string
		pageExcl, pageCount, ledgerExcl, ledgerCount, fr bool
	}{
		{name: "shipped page claim", text: shippedPageClaim, pageExcl: true, fr: true},
		{name: "corrected page claim", text: correctedPageClaim, pageCount: true},
		{name: "shipped ledger claim", text: shippedLedgerClaim, ledgerExcl: true},
		{name: "corrected ledger claim", text: correctedLedgerClaim, ledgerCount: true},
		{name: "unrelated exclusion prose is neither", text: unrelated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flat := flattenProse(c.text)
			for _, got := range []struct {
				name string
				re   *regexp.Regexp
				want bool
			}{
				{"pageExcludesAlertmanagerRE", pageExcludesAlertmanagerRE, c.pageExcl},
				{"pageCountsAlertmanagerRE", pageCountsAlertmanagerRE, c.pageCount},
				{"ledgerExcludesAlertmanager", ledgerExcludesAlertmanager, c.ledgerExcl},
				{"ledgerCountsAlertmanagerRE", ledgerCountsAlertmanagerRE, c.ledgerCount},
				{"pagePriorFreezeRE", pagePriorFreezeRE, c.fr},
			} {
				if m := got.re.MatchString(flat); m != got.want {
					t.Errorf("%s.MatchString(%q) = %v, want %v", got.name, flat, m, got.want)
				}
			}
		})
	}
}

// TestFlattenProseJoinsWrappedClaims is the mutation test for the normalizer the
// anchors depend on: a claim split across a wrapped Go doc comment and one split
// across a markdown paragraph must both come back as one line, or every phrase
// anchor above silently stops matching the moment an editor re-wraps a sentence.
func TestFlattenProseJoinsWrappedClaims(t *testing.T) {
	const wrappedComment = "\t// false for sources that never emit one (GitOps, reinvestigate, or\n" +
		"\t// Alertmanager with send_resolved off). A pointer"
	if got, want := flattenProse(wrappedComment),
		"false for sources that never emit one (GitOps, reinvestigate, or Alertmanager with send_resolved off). A pointer"; got != want {
		t.Errorf("flattenProse(wrapped comment) = %q, want %q", got, want)
	}
	const wrappedMarkdown = "channel (**GitOps failures**, reinvestigate polls, Alertmanager without\n`send_resolved`)\nare excluded"
	if got, want := flattenProse(wrappedMarkdown),
		"channel (**GitOps failures**, reinvestigate polls, Alertmanager without `send_resolved`) are excluded"; got != want {
		t.Errorf("flattenProse(wrapped markdown) = %q, want %q", got, want)
	}
	// A markdown link must survive: stripping "//" unconditionally would eat it.
	if got, want := flattenProse("see https://example.test/x for more"), "see https://example.test/x for more"; got != want {
		t.Errorf("flattenProse(url) = %q, want %q", got, want)
	}
}
