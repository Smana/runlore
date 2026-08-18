// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

const (
	// upgradePath is the page an upgrading operator actually opens, and the durable
	// home of the migration notes for each release's breaking changes.
	upgradePath = "../../website/content/docs/operations/upgrade-uninstall.md"
	// changelogPath is the file release-please regenerates. Only its PREAMBLE is
	// hand-maintained — release-please prepends each new release section below it —
	// so the preamble is the one part of this file a guard can meaningfully pin.
	changelogPath = "../../CHANGELOG.md"
)

// TestMigrationNoteStatesTheRaisedDefault pins the number the 0.15.0 breaking-changes
// note exists to communicate, off ApplyDefaults rather than typed in.
//
// This is the defect that shipped: the generated changelog told operators "The default
// is unchanged at 100000" under both `investigate:` and `eval:`, while the shipped
// default had been raised to 400000 in the very commit being described. An operator
// who believed it would have sized their spend against a quarter of the real ceiling.
func TestMigrationNoteStatesTheRaisedDefault(t *testing.T) {
	var c config.Config
	config.ApplyDefaults(&c)
	inv := c.Investigation

	doc := flattenProse(readDoc(t, upgradePath))
	want := fmt.Sprintf("the default is raised from `100000` to `%d`", inv.MaxTokensPerInvestigation)
	if !strings.Contains(doc, want) {
		t.Errorf("%s must state the migration verbatim as %q — this is the number the 0.15.0 "+
			"breaking-changes note got wrong, and the page is the corrected record", upgradePath, want)
	}
	// The opt-out semantics are the other half an operator gets wrong: 0 is NOT the
	// off switch, and a config written to remove the cap with 0 silently gets the
	// bounded default instead.
	if !strings.Contains(doc, "**`0` does not disable it.**") {
		t.Errorf("%s must say plainly that `0` does not disable %s — 0 applies the bounded "+
			"default and -1 is the opt-out", upgradePath, tagOf(inv, "MaxTokensPerInvestigation"))
	}
}

// unchangedDefaultRE captures the claim that shipped wrong, in the shape it shipped in.
var unchangedDefaultRE = regexp.MustCompile(`(?i)default is unchanged at (\d+)`)

// TestNoShippedDocClaimsTheTokenDefaultIsUnchanged is the direct anti-recurrence guard.
//
// The wrong paragraph reached CHANGELOG.md because a superseded BREAKING CHANGE: footer
// won over the correct one (see breaking_change_footers_test.go for the mechanism, now
// guarded). Belt and braces: even if a stale migration paragraph is reintroduced by
// hand — by re-applying an old changelog section, or by copying the superseded footer —
// this fails the moment its number disagrees with what ApplyDefaults actually ships.
//
// It scans the CHANGELOG too, deliberately. That file is generated, but it is generated
// ONTO this branch and merged, so a wrong claim in it is a wrong claim this repo ships.
func TestNoShippedDocClaimsTheTokenDefaultIsUnchanged(t *testing.T) {
	var c config.Config
	config.ApplyDefaults(&c)
	shipped := c.Investigation.MaxTokensPerInvestigation

	for _, path := range []string{changelogPath, configurationPath, upgradePath} {
		doc := flattenProse(readDoc(t, path))
		for _, m := range unchangedDefaultRE.FindAllStringSubmatch(doc, -1) {
			claimed, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			if claimed != shipped {
				t.Errorf("%s claims %q, but ApplyDefaults ships %d. A migration note that "+
					"misstates the default it exists to announce is worse than none — an "+
					"operator sizes their spend against the number they were given.",
					path, m[0], shipped)
			}
		}
	}
}

// TestUnchangedDefaultREFlips is the mutation test for the anchor above: it must fire on
// the paragraph that actually shipped and stay quiet on the corrected one. Without this,
// a regexp that matched nothing would pin nothing while still reporting success.
func TestUnchangedDefaultREFlips(t *testing.T) {
	const shipped = "not a check on the next request's estimated size. The default is unchanged at " +
		"100000, so the same number binds far earlier than it used to"
	const corrected = "not a check on the next request's estimated size, and its default is raised " +
		"from 100000 to 400000 to suit the new meaning"

	got := unchangedDefaultRE.FindStringSubmatch(shipped)
	if got == nil {
		t.Fatal("the anchor does not match the paragraph that actually shipped — it pins nothing")
	}
	if got[1] != "100000" {
		t.Errorf("the anchor must capture the claimed default; got %q", got[1])
	}
	if unchangedDefaultRE.MatchString(corrected) {
		t.Error("the anchor matches the corrected wording — it would fail on a page that is right")
	}
}

// TestMigrationNoteAndConfigurationPageCannotDrift pins the claims that must read the
// same on both pages.
//
// The migration note is a one-shot read at upgrade time; the configuration page is the
// standing reference. They describe one behaviour, and the failure mode this repo keeps
// hitting is that one of the two is corrected and the other is not. Each claim below is
// the SENTENCE that carries it, flattened, so a rewrite on either page fails here rather
// than leaving the two quietly disagreeing.
//
// The derived FIGURES (the per-request quarter, the compaction trigger, the 1.25x
// multiplier) are pinned against the code itself by
// TestSpendCeilingDocsStateTheDerivedThresholds, which covers both pages and the chart;
// that derivation is internal to internal/investigate and cannot be reached from here.
func TestMigrationNoteAndConfigurationPageCannotDrift(t *testing.T) {
	shared := []string{
		// What the ceiling now counts. The whole migration turns on "cumulative".
		"one investigation's model tokens (provider-reported input + output, loop **and** verify",
		// What an operator with an explicit value must understand.
		"still says `100000`, and now",
		// The residual overshoot, which makes the ceiling not an exact cap.
		"the nudge exists to give the model one turn to conclude",
	}
	pages := map[string]string{
		upgradePath:       flattenProse(readDoc(t, upgradePath)),
		configurationPath: flattenProse(readDoc(t, configurationPath)),
	}
	for _, claim := range shared {
		for path, doc := range pages {
			if !strings.Contains(doc, claim) {
				t.Errorf("%s no longer states %q. The migration note and the configuration "+
					"reference describe one behaviour; correcting one and not the other is how "+
					"this page drifted in the first place.", path, claim)
			}
		}
	}
}

// TestChangelogPreambleDoesNotDenyItsOwnReleases catches the third defect in the same
// file: the preamble still read "there are no tagged releases yet, so everything
// currently lives under `[Unreleased]`" while sitting directly above `## [0.14.0]`, with
// no `[Unreleased]` section anywhere in the file.
//
// release-please prepends each release section BELOW this preamble and never rewrites
// it, so the preamble is hand-maintained and drifts silently — it was true exactly once,
// before the first release, and nothing noticed when that stopped being so. It is the
// first thing a reader of the changelog sees.
func TestChangelogPreambleDoesNotDenyItsOwnReleases(t *testing.T) {
	raw := readDoc(t, changelogPath)
	preamble, _, found := strings.Cut(raw, "\n## ")
	if !found {
		t.Fatalf("%s has no `## ` release section — the preamble guard has nothing to compare "+
			"against and would pass trivially", changelogPath)
	}
	releases := regexp.MustCompile(`(?m)^## \[\d+\.\d+\.\d+\]`).FindAllString(raw, -1)
	if len(releases) == 0 {
		t.Fatalf("%s contains no released `## [x.y.z]` section; this guard exists to compare the "+
			"preamble against the releases below it", changelogPath)
	}

	flat := flattenProse(preamble)
	if strings.Contains(flat, "no tagged releases") {
		t.Errorf("%s preamble says there are no tagged releases, but the file lists %d of them "+
			"(most recent: %s)", changelogPath, len(releases), releases[0])
	}
	if strings.Contains(flat, "[Unreleased]") && !strings.Contains(raw, "## [Unreleased]") {
		t.Errorf("%s preamble points the reader at an `[Unreleased]` section that does not exist "+
			"in the file", changelogPath)
	}
}
