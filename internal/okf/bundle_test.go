// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func bundleEntry() providers.KBEntry {
	return providers.KBEntry{
		Type: "Incident", Title: "Harbor down",
		Description: "valkey down", Fingerprint: "deadbeefcafebabe",
	}
}

func TestUpdateIndexAppendsToTypeSection(t *testing.T) {
	existing := `---
okf_version: "0.1"
type: Index
---
# Catalog

## Playbooks

- [HelmRelease upgrade failure](playbooks/helmrelease.md)

## Incidents

- [Old incident](incidents/old.md)
`
	got := string(UpdateIndex([]byte(existing), bundleEntry(), "incidents/harbor-down-deadbeef.md"))
	want := "- [Harbor down](incidents/harbor-down-deadbeef.md) — valkey down"
	if !strings.Contains(got, want) {
		t.Fatalf("index missing %q:\n%s", want, got)
	}
	// The new line must land inside the Incidents section, not after Playbooks.
	if strings.Index(got, want) < strings.Index(got, "## Incidents") {
		t.Fatalf("entry landed outside its type section:\n%s", got)
	}
	// The existing Playbooks section must be untouched.
	if !strings.Contains(got, "- [HelmRelease upgrade failure](playbooks/helmrelease.md)") {
		t.Fatalf("existing sections must be preserved:\n%s", got)
	}
}

func TestUpdateIndexCreatesMissingSection(t *testing.T) {
	existing := "# Catalog\n\n## Playbooks\n\n- [P](p.md)\n"
	got := string(UpdateIndex([]byte(existing), bundleEntry(), "incidents/h.md"))
	if !strings.Contains(got, "## Incidents") {
		t.Fatalf("missing new ## Incidents section:\n%s", got)
	}
	if !strings.Contains(got, "- [Harbor down](incidents/h.md) — valkey down") {
		t.Fatalf("missing entry line:\n%s", got)
	}
}

func TestUpdateLogCreatesAndPrepends(t *testing.T) {
	// No log yet → a fresh OKF log: H1 title, newest-first date heading, bold
	// action word.
	got := string(UpdateLog(nil, bundleEntry(), "incidents/h.md", "2026-07-03"))
	for _, want := range []string{"# ", "## 2026-07-03", "* **Creation**: Added [Harbor down](incidents/h.md)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("fresh log missing %q:\n%s", want, got)
		}
	}

	// Existing log with an older date → the new date heading goes FIRST (newest
	// first), older entries preserved below.
	existing := "# Catalog update log\n\n## 2026-06-20\n\n* **Creation**: Added [Old](o.md).\n"
	got = string(UpdateLog([]byte(existing), bundleEntry(), "incidents/h.md", "2026-07-03"))
	i, j := strings.Index(got, "## 2026-07-03"), strings.Index(got, "## 2026-06-20")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("dates must be newest-first:\n%s", got)
	}

	// Same-day second entry → reuse the existing date heading, no duplicate.
	got = string(UpdateLog([]byte(got), bundleEntry(), "incidents/h2.md", "2026-07-03"))
	if strings.Count(got, "## 2026-07-03") != 1 {
		t.Fatalf("same-day entries must share one date heading:\n%s", got)
	}
	if !strings.Contains(got, "(incidents/h2.md)") {
		t.Fatalf("second same-day entry missing:\n%s", got)
	}
}

// TestEntryFilePicksTheEntryNotTheBundleUpkeep: a RunLore curation request
// changes the entry plus, at most, index.md and log.md. Only the entry is what
// a later commit (an appended operator note) must edit — writing into log.md
// instead would corrupt the bundle and lose the note in one move.
func TestEntryFilePicksTheEntryNotTheBundleUpkeep(t *testing.T) {
	got, err := EntryFile([]string{"index.md", "concepts/oom-1755.md", "log.md"})
	if err != nil {
		t.Fatalf("EntryFile: %v", err)
	}
	if got != "concepts/oom-1755.md" {
		t.Errorf("EntryFile = %q, want the entry", got)
	}
}

// TestEntryFileRefusesToGuess: the caller's next move is a commit into a pull
// request a human is reviewing. Anything other than exactly one candidate must
// be an error — a refusal degrades to a comment, while a wrong write silently
// rewrites somebody else's change.
func TestEntryFileRefusesToGuess(t *testing.T) {
	for name, changed := range map[string][]string{
		"nothing at all":     nil,
		"upkeep only":        {"index.md", "log.md"},
		"two entries":        {"concepts/a.md", "incidents/b.md"},
		"no markdown at all": {"Makefile", "go.mod"},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := EntryFile(changed); err == nil {
				t.Errorf("EntryFile(%v) = %q, want an error rather than a guess", changed, got)
			}
		})
	}
}

func TestAppendBlockKeepsWhatIsAlreadyThere(t *testing.T) {
	got := string(AppendBlock([]byte("---\ntype: Concept\n---\n\nfirst note\n"), "second note"))
	want := "---\ntype: Concept\n---\n\nfirst note\n\nsecond note\n"
	if got != want {
		t.Errorf("AppendBlock =\n%q\nwant\n%q", got, want)
	}
}

// TestAppendBlockEmptyBlockChangesNothing: the caller's next move is a real
// commit in a human's pull request, so appending nothing must not rewrite the
// file — an empty-diff commit is noise a reviewer has to read.
func TestAppendBlockEmptyBlockChangesNothing(t *testing.T) {
	existing := []byte("body\n")
	if got := string(AppendBlock(existing, "\n\n")); got != "body\n" {
		t.Errorf("AppendBlock with an empty block = %q, want the file untouched", got)
	}
}

// TestAppendBlockNormalisesSpacing pins the separation to exactly one blank
// line however ragged either side is, so a note appended after a file that
// already ends in three newlines does not drift further from one that does not.
func TestAppendBlockNormalisesSpacing(t *testing.T) {
	if got := string(AppendBlock([]byte("body\n\n\n"), "\n\nnote\n\n")); got != "body\n\nnote\n" {
		t.Errorf("AppendBlock = %q, want exactly one blank line between the blocks", got)
	}
	if got := string(AppendBlock(nil, "note")); got != "note\n" {
		t.Errorf("AppendBlock onto an empty file = %q, want no leading blank line", got)
	}
}

// TestHasFrontmatterRefusesWhatWouldReplaceAnEntry is the guard that stands
// between a read returning nothing and AppendBlock returning the note ALONE.
// The two together are how an entry gets replaced by the newest note appended
// to it, so the empty case is the one that matters most here.
func TestHasFrontmatterRefusesWhatWouldReplaceAnEntry(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    bool
	}{
		"a real entry":     {"---\ntype: Concept\n---\n\nbody\n", true},
		"empty":            {"", false},
		"plain markdown":   {"# Notes\n\nnot an entry\n", false},
		"fence not first":  {"\n---\ntype: Concept\n---\n", false},
		"fence unfinished": {"---", false},
		"html then fence":  {"<!-- draft -->\n---\ntype: Concept\n---\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := HasFrontmatter([]byte(tc.content)); got != tc.want {
				t.Errorf("HasFrontmatter(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestNoteMarkerIsInvisibleAndKeyed: the marker must not render in the entry a
// human or the catalog reads, and it must identify ONE note — a marker that
// matched any note would suppress every note after the first, which is the
// original bug wearing a different hat.
func TestNoteMarkerIsInvisibleAndKeyed(t *testing.T) {
	m := NoteMarker("abc123")
	if !strings.HasPrefix(m, "<!--") || !strings.HasSuffix(m, "-->") {
		t.Errorf("NoteMarker = %q, want an HTML comment so it never renders", m)
	}
	if !strings.Contains(m, "abc123") {
		t.Errorf("NoteMarker = %q, want it to carry the key", m)
	}
	entry := []byte("---\ntype: Concept\n---\n\nnote one\n\n" + m + "\n")
	if !HasNoteMarker(entry, "abc123") {
		t.Error("HasNoteMarker must find the marker it wrote — otherwise a replay appends the note twice")
	}
	if HasNoteMarker(entry, "def456") {
		t.Error("HasNoteMarker matched a DIFFERENT key — every later note in the thread would be silently dropped")
	}
	// An empty key means "no idempotency", never "matches everything": the latter
	// would drop every note on any caller that omitted a key.
	if HasNoteMarker(entry, "") {
		t.Error("an empty key must never match")
	}
}
