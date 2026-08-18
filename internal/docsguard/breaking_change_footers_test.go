// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// This file guards the ONE mechanism that carries migration prose — not just a subject
// line — from a commit into CHANGELOG.md: the `BREAKING CHANGE:` footer.
//
// It exists because that mechanism failed silently and shipped. The 0.15.0 release notes
// told operators "The default is unchanged at 100000" while the shipped default was
// 400000, twice over, and both instances trace to the footer parser rather than to
// anything a human wrote wrong in isolation:
//
//   - release-please's toConventionalChangelogFormat keeps a SINGLE breaking.text, filled
//     by the FIRST body-level node it finds. A squash body carrying two footers therefore
//     renders the FIRST one — and in a stacked branch the first is the OLDEST, i.e. the
//     superseded one. 8d26929's correct footer (400000, the derived quarter, the
//     compaction trigger, the measured overshoot) never reached the changelog at all.
//   - A branch stacked on that one inherited the footer. 0ef4507's subject is
//     `fix(eval):` with no `!`, but the inherited footer made release-please render it as
//     breaking anyway — emitting the same wrong paragraph a second time, under a scope
//     that never made a breaking change.
//
// Both are mechanical and both are visible in git history before the release PR is ever
// generated, which is where this guard reads them.

// breakingFooterRE matches a conventional-commits BREAKING CHANGE footer token: it must
// open a line, and both the spec's spellings count. Matching anywhere in the line would
// fire on prose that merely quotes the token (this very repo's commit bodies discuss it
// at length), so the anchor is deliberately line-anchored.
var breakingFooterRE = regexp.MustCompile(`(?m)^BREAKING[ -]CHANGE:`)

// conventionalSubjectRE splits a conventional-commit subject into its type, optional
// scope and the `!` that marks it breaking — the same shape release-please parses.
var conventionalSubjectRE = regexp.MustCompile(`^([a-zA-Z]+)(\([^()]*\))?(!)?:`)

// countBreakingFooters returns how many BREAKING CHANGE footers a commit BODY carries.
// More than one is the defect: release-please keeps a single breaking.text and fills it
// from the first, so every footer after the first is silently discarded — and the one
// that survives is the one written earliest, which in a stacked branch is the one later
// work superseded.
func countBreakingFooters(body string) int {
	return len(breakingFooterRE.FindAllString(body, -1))
}

// subjectMarksBreaking reports whether a conventional-commit subject carries the `!`
// that declares the change breaking. A subject with no `!` whose body carries a footer
// is still rendered as breaking by release-please, so the two must agree or the
// changelog attributes a migration to a scope that never made one.
func subjectMarksBreaking(subject string) bool {
	m := conventionalSubjectRE.FindStringSubmatch(subject)
	return m != nil && m[3] == "!"
}

// footerExemptions are commits ALREADY ON MAIN when this guard landed. They cannot be
// rewritten — main is protected and every SHA below is public — so the remedy for them
// is the corrected prose in website/content/docs/operations/upgrade-uninstall.md and a
// hand-correction of the release PR's changelog, not a history rewrite.
//
// They are not dead weight once 0.15.0 is tagged and they leave the scanned window:
// TestBreakingFooterExemptionsAreStillEarned asserts each still resolves and still
// violates, so they go on serving as real-history fixtures proving this file's detector
// fires on the exact commits that caused the incident. Delete an entry only when its
// claim stops being true.
var footerExemptions = map[string]string{
	"8d26929403971aac24b4f5b71cdd2bb82f50bc4c": "two footers: the stacked branch's superseded " +
		"100000 note won over the correct 400000 one. Already on main; corrected in the upgrade docs.",
	"0ef4507090d6859e93532fcf5d88087310465e40": "two footers inherited from the branch it was " +
		"stacked on, under a `fix(eval):` subject with no `!`. Already on main.",
}

// commitRecord is one commit as this guard reads it: the subject release-please parses
// and the body it mines footers from.
type commitRecord struct {
	SHA     string
	Subject string
	Body    string
}

// TestNoCommitCarriesTwoBreakingChangeFooters fails a commit whose body declares the
// breaking change twice. Only the first survives into CHANGELOG.md, so the second is
// prose the author believed they had published and nobody will ever read.
func TestNoCommitCarriesTwoBreakingChangeFooters(t *testing.T) {
	for _, c := range releaseWindow(t) {
		if _, exempt := footerExemptions[c.SHA]; exempt {
			continue
		}
		if n := countBreakingFooters(c.Body); n > 1 {
			t.Errorf("commit %s (%s) carries %d BREAKING CHANGE: footers.\n"+
				"release-please keeps ONE breaking.text and fills it from the FIRST body-level "+
				"node, so the other %d are silently dropped — and in a stacked branch the first "+
				"is the one later commits superseded. Squash the migration into a SINGLE footer "+
				"stating what is true of the merged change.",
				c.SHA[:8], c.Subject, n, n-1)
		}
	}
}

// TestBreakingFooterRequiresABangSubject fails a commit whose body declares a breaking
// change while its subject does not.
//
// release-please renders such a commit under ⚠ BREAKING CHANGES regardless, attributed
// to the subject's scope. That is how a `fix(eval):` commit came to announce a
// migration to investigation.max_tokens_per_investigation — a key it does not own —
// duplicating the note under a second heading. The `!` is also what the version bump
// reads, so a footer without one is a migration the release number does not reflect.
func TestBreakingFooterRequiresABangSubject(t *testing.T) {
	for _, c := range releaseWindow(t) {
		if _, exempt := footerExemptions[c.SHA]; exempt {
			continue
		}
		if countBreakingFooters(c.Body) > 0 && !subjectMarksBreaking(c.Subject) {
			t.Errorf("commit %s carries a BREAKING CHANGE: footer but its subject has no `!`:\n"+
				"  %s\n"+
				"release-please renders it under ⚠ BREAKING CHANGES anyway, attributed to that "+
				"scope. Add the `!` (type(scope)!: …) so the subject, the changelog and the "+
				"version bump agree — or drop the footer if the change is not breaking. PRs are "+
				"squash-merged, so this is the PR TITLE.",
				c.SHA[:8], c.Subject)
		}
	}
}

// TestBreakingFooterExemptionsAreStillEarned keeps the exemption list honest in both
// directions: every entry must name a commit this repo actually contains, and that
// commit must still violate a rule above. An exemption that matches nothing is a licence
// nobody is using, and it would go on silencing whatever SHA later collided with it.
func TestBreakingFooterExemptionsAreStillEarned(t *testing.T) {
	for sha, why := range footerExemptions {
		out, err := gitTry("rev-parse", "--verify", "--quiet", sha+"^{commit}")
		if err != nil || strings.TrimSpace(out) != sha {
			t.Errorf("exempted commit %s (%q) does not resolve in this repository — a stale "+
				"exemption silences nothing it names and would cover any SHA that later matched",
				sha, why)
			continue
		}
		c := readCommit(t, sha)
		if countBreakingFooters(c.Body) <= 1 && subjectMarksBreaking(c.Subject) {
			t.Errorf("exempted commit %s no longer violates either rule — delete the exemption "+
				"rather than leaving a licence that grants nothing", sha)
		}
	}
}

// TestBreakingFooterDetectorsFlip is the mutation test. Each rule has to fire on the
// shape that actually shipped and stay quiet on the shape that is correct — including
// on prose that merely MENTIONS the footer token, which this repo's commit bodies do at
// length and which an un-anchored matcher would count as a footer.
func TestBreakingFooterDetectorsFlip(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"no footer", "Just a body explaining the change.\n", 0},
		{"one footer", "Body.\n\nBREAKING CHANGE: the key means something else now.\n", 1},
		{"hyphenated spelling", "Body.\n\nBREAKING-CHANGE: the key means something else now.\n", 1},
		{"two footers, the shipped defect", "Body.\n\nBREAKING CHANGE: default unchanged at 100000.\n" +
			"\nMore body.\n\nBREAKING CHANGE: default raised to 400000.\n", 2},
		{"mentioned in prose, not a footer", "The repo has never marked one: no `!` in any " +
			"subject, no BREAKING CHANGE: footer in 1000+ commits.\n", 0},
		{"indented, so not a footer token", "Body.\n\n  BREAKING CHANGE: quoted in a list.\n", 0},
		{"lowercase is not the token", "Body.\n\nbreaking change: not the spec spelling.\n", 0},
	} {
		if got := countBreakingFooters(tc.body); got != tc.want {
			t.Errorf("countBreakingFooters(%s) = %d, want %d", tc.name, got, tc.want)
		}
	}

	for _, tc := range []struct {
		subject string
		want    bool
	}{
		{"feat(investigate)!: cap what one investigation may spend", true},
		{"fix(gitops)!: confine the forge credential to the forge's own git host", true},
		{"fix!: a breaking change with no scope", true},
		{"fix(eval): bound lore eval, and report what the CLI's model calls spend", false},
		{"feat(investigate): cap what one investigation may spend", false},
		{"docs: not breaking", false},
		{"a subject that is not conventional at all", false},
		{"fix(scope): a body that later says BREAKING CHANGE: is still not a `!` subject", false},
	} {
		if got := subjectMarksBreaking(tc.subject); got != tc.want {
			t.Errorf("subjectMarksBreaking(%q) = %v, want %v", tc.subject, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Reading the window. Everything below exists to make this guard fail rather than
// quietly scan nothing.
//
// The failure mode being designed against is specific and this repo has been bitten by
// it before (see hack/check-anchors.sh, whose zero-check is "the load-bearing part"):
// a guard that reads the environment passes vacuously when the environment is thinner
// than it assumed. actions/checkout defaults to fetch-depth: 1 — ONE commit and NO
// tags — so a history guard dropped into CI unexamined reports success having examined
// nothing at all. ci.yaml therefore sets fetch-depth: 0, and every probe below fails
// loudly rather than degrading to a pass.

// windowCache memoises the scan so the two rule tests do not each shell out to git.
var windowCache []commitRecord

// releaseWindow returns every commit since the last release tag — the exact set
// release-please will parse into the next CHANGELOG entry, which is what makes this the
// right range: a footer defect is caught while it can still be fixed on the branch,
// before the release PR is generated from it.
//
// Failing loudly is the point of every branch in here. Note that "is this repository
// shallow" is NOT the question asked: a clone can report shallow and still contain the
// whole window (this repo's own working clones do), while a depth-1 clone that fetched
// a tag would answer the question the wrong way round. The honest test is whether the
// window itself is walkable, which is what merge-base --is-ancestor proves.
func releaseWindow(t *testing.T) []commitRecord {
	t.Helper()
	if windowCache != nil {
		return windowCache
	}
	if _, err := gitTry("rev-parse", "--git-dir"); err != nil {
		t.Fatalf("cannot read git history (%v).\nThis guard reads the commits release-please "+
			"will parse; with no repository it can only scan nothing and pass, which is the "+
			"inert-guard failure it exists to prevent.", err)
	}

	base, err := gitTry("describe", "--tags", "--abbrev=0", "HEAD")
	if err != nil {
		t.Fatalf("no release tag is reachable from HEAD (%v).\nThe scanned range is "+
			"<last tag>..HEAD, so without tags this guard has no window and would pass "+
			"vacuously. In CI this means the checkout is shallow: set fetch-depth: 0 "+
			"(actions/checkout defaults to 1 commit and no tags).", err)
	}
	base = strings.TrimSpace(base)

	// At the release commit itself HEAD *is* the tag, so <tag>..HEAD is legitimately
	// empty. Step back to the previous tag and re-scan the window that was just
	// released rather than reporting an empty scan — the guard must never have nothing
	// to look at, and the commits it would skip here are exactly the released ones.
	if rev(t, base) == rev(t, "HEAD") {
		prev, err := gitTry("describe", "--tags", "--abbrev=0", "HEAD~1")
		if err != nil {
			t.Fatalf("HEAD is the release tag %s and no earlier tag is reachable (%v) — "+
				"the window cannot be established", base, err)
		}
		base = strings.TrimSpace(prev)
	}

	// Proves the ENTIRE window is present in the object store. A clone truncated
	// between base and HEAD fails here instead of silently yielding a short list.
	if _, err := gitTry("merge-base", "--is-ancestor", base, "HEAD"); err != nil {
		t.Fatalf("%s is not walkable from HEAD (%v).\nThe clone is truncated inside the range "+
			"this guard claims to scan, so it would examine only part of it. Fetch full "+
			"history (fetch-depth: 0).", base, err)
	}

	out, err := gitTry("log", "--format=%H%x1f%s%x1f%b%x1e", base+"..HEAD")
	if err != nil {
		t.Fatalf("git log %s..HEAD: %v", base, err)
	}
	window := parseCommits(out)
	if len(window) == 0 {
		t.Fatalf("scanned 0 commits in %s..HEAD — this guard is inert.\nEither the range is "+
			"wrong or the clone cannot see the history it claims to check; a guard that "+
			"examines nothing must fail, not report success.", base)
	}
	windowCache = window
	return window
}

// parseCommits splits the record-separated `git log` output this file asks for.
func parseCommits(out string) []commitRecord {
	var commits []commitRecord
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		parts := strings.SplitN(rec, "\x1f", 3)
		if len(parts) != 3 {
			continue
		}
		commits = append(commits, commitRecord{SHA: parts[0], Subject: parts[1], Body: parts[2]})
	}
	return commits
}

// readCommit reads one commit by SHA, for the exemption audit.
func readCommit(t *testing.T, sha string) commitRecord {
	t.Helper()
	out, err := gitTry("log", "-1", "--format=%H%x1f%s%x1f%b%x1e", sha)
	if err != nil {
		t.Fatalf("git log -1 %s: %v", sha, err)
	}
	c := parseCommits(out)
	if len(c) != 1 {
		t.Fatalf("git log -1 %s returned %d commits", sha, len(c))
	}
	return c[0]
}

// rev resolves a revision to its commit SHA.
func rev(t *testing.T, s string) string {
	t.Helper()
	out, err := gitTry("rev-parse", s+"^{commit}")
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", s, err)
	}
	return strings.TrimSpace(out)
}

// gitTry runs git in the repo root, returning its stdout. stderr is folded into the
// error so a failure says what git actually complained about.
func gitTry(args ...string) (string, error) {
	root, err := moduleDir()
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return string(out), nil
}

// moduleDir returns the repo root without needing a *testing.T, so gitTry can be used
// from probes that report an error rather than failing outright.
func moduleDir() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", errors.New("not inside a git working tree")
	}
	return strings.TrimSpace(string(out)), nil
}
