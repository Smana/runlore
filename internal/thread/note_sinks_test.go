// SPDX-License-Identifier: Apache-2.0

// This file is package thread_test, not package thread, on purpose: it reaches
// into internal/okf to exercise the REAL sinks an entry's identity fields flow
// into, and internal/thread's own dependency set is deliberately narrow (see the
// note on noteForgeLabel). An external test package keeps that promise intact —
// its imports are the test binary's, not the package's.
package thread_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

var sinkAt = time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC)

// noteEntry builds the entry a thread note files, for a note the attacker wrote.
func noteEntry(t *testing.T, note string) providers.KBEntry {
	t.Helper()
	return thread.ConceptEntry(
		thread.Context{Transport: "slack", Title: "OOM in payments"},
		thread.HumanNote("alice", note),
		sinkAt, thread.DefaultMaxNoteBytes,
	)
}

// TestNoteIdentityFieldsAreDefusedInEveryBundleSink is the test whose absence
// made this invisible: everything pinned the entry's own fields, and nothing
// pinned what the FORGE then writes with them.
//
// An entry's title and description are not read only out of its YAML. Both are
// interpolated raw into index.md by okf.UpdateIndex ("- [%s](%s) — %s"), the
// title into log.md by okf.UpdateLog, and the description into the pull-request
// body by github.prBody — permanent, merged, markdown-rendered repository files.
// Since the note-identity fix, both fields are arbitrary chat text from any
// channel member, where before this the description was a fixed sentence and the
// title was alert-derived.
func TestNoteIdentityFieldsAreDefusedInEveryBundleSink(t *testing.T) {
	const pixel = `<img src="https://evil.example/px.gif">`
	const image = `![x](https://evil.example/y.png)`

	tests := []struct {
		name string
		note string
		// banned must not appear in ANY rendered sink: each is live markup.
		banned []string
		// kept must survive somewhere: neutralise, never censor — a reviewer has
		// to be able to read exactly what was submitted.
		kept string
		// escaped is the defused spelling of the markup character itself. It is
		// what separates ESCAPING from STRIPPING: deleting "<img" also removes the
		// live markup and also leaves the URL behind, so `kept` alone cannot tell
		// the two apart, and a censoring implementation would pass.
		escaped string
	}{
		{
			name:    "raw HTML is a tracking pixel firing from every reader's browser",
			note:    "the cause was " + pixel,
			banned:  []string{pixel, "<img"},
			kept:    "evil.example/px.gif",
			escaped: "&lt;img",
		},
		{
			name:    "image markdown is the same pixel by another syntax",
			note:    "the cause was " + image,
			banned:  []string{image, "!["},
			kept:    "evil.example/y.png",
			escaped: "!&#91;",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := noteEntry(t, tt.note)
			sinks := map[string]string{
				"entry title":       e.Title,
				"entry description": e.Description,
				"index.md":          string(okf.UpdateIndex([]byte("# Index\n\n## Concepts\n"), e, "concepts/n.md")),
				"log.md":            string(okf.UpdateLog(nil, e, "concepts/n.md", "2026-08-14")),
			}
			for where, got := range sinks {
				for _, b := range tt.banned {
					if strings.Contains(got, b) {
						t.Errorf("%s renders live markup %q:\n%s", where, b, got)
					}
				}
			}
			if !strings.Contains(e.Description, tt.kept) {
				t.Errorf("the submitted text must survive as inert text, not be censored: %q", e.Description)
			}
			if !strings.Contains(e.Description, tt.escaped) {
				t.Errorf("the markup character must be ESCAPED (%q), not deleted — a reviewer has to see exactly what was submitted: %q",
					tt.escaped, e.Description)
			}
		})
	}
}

// TestNoteTitleCannotHijackTheBundleLink covers the third vector, which the
// image/HTML defusal above does NOT close and must not be assumed to: a bare "]"
// is not markup either function knows about.
//
// A title of `boom](https://evil.example) x` rendered as
// `- [boom](https://evil.example) x](concepts/n.md) — …`, so the index's own
// link FOR THAT ENTRY pointed at the attacker's URL while still looking like the
// entry's row. The fix is in okf, where the "a title is a link label" assumption
// is made — see okf.linkText for why there rather than here.
//
// This asserts the rendered SINKS, not the entry's fields: "](" is ordinary text
// in a YAML title and defusing it there would put escapes in the catalog for a
// rendering concern that belongs to the renderer.
func TestNoteTitleCannotHijackTheBundleLink(t *testing.T) {
	for _, note := range []string{
		"boom](https://evil.example) more",
		// A backslash before the bracket is the second half of the escape, and
		// the reason linkText escapes "\" too. Escaping only the brackets turns
		// `\]` into `\\]` — an escaped BACKSLASH followed by a live bracket — so
		// the label closes anyway and the hijack works through the fix.
		`boom\](https://evil.example) more`,
		`boom\\](https://evil.example) more`,
	} {
		t.Run(note, func(t *testing.T) {
			e := noteEntry(t, note)
			for name, doc := range map[string]string{
				"index.md": string(okf.UpdateIndex([]byte("# Index\n\n## Concepts\n"), e, "concepts/n.md")),
				"log.md":   string(okf.UpdateLog(nil, e, "concepts/n.md", "2026-08-14")),
			} {
				line := ""
				for _, l := range strings.Split(doc, "\n") {
					if strings.Contains(l, "concepts/n.md") {
						line = l
					}
				}
				if line == "" {
					t.Fatalf("%s has no line for the entry:\n%s", name, doc)
				}
				// The property is where the line's FIRST link points: that link is
				// the entry's own, and the hijack works by ending its label early so
				// the target becomes the attacker's URL instead of the entry's path.
				if target := firstLinkTarget(line); target != "concepts/n.md" {
					t.Errorf("%s: the entry's own link points at %q, want %q: %q", name, target, "concepts/n.md", line)
				}
				// Neutralise, never censor: the reviewer still reads what was sent.
				if !strings.Contains(line, "evil.example") {
					t.Errorf("%s dropped the submitted text instead of escaping it: %q", name, line)
				}
				// And the bracket itself survives, backslash-escaped. Deleting the
				// brackets would also stop the hijack and also keep the URL, so the
				// check above cannot tell escaping from stripping on its own.
				if !strings.Contains(line, `\]`) {
					t.Errorf("%s stripped the title's bracket instead of escaping it, so the index no longer says what the entry is called: %q", name, line)
				}
			}
		})
	}
}

// TestNoteIdentityFieldBudgetsSurviveTheDefusal pins the ORDER the defusal runs
// in, which is invisible to a test that only checks the markup is gone.
//
// neutralizeImages expands "![" 3x and neutralizeHTML "<x" 2.5x. noteLine defuses
// BEFORE each caller truncates, so every stated budget bounds the expanded text.
// Defusing afterwards would inflate each field by that factor while every test
// asserting "no live markup" still passed — the description worst of all, since
// nothing downstream re-cuts it.
func TestNoteIdentityFieldBudgetsSurviveTheDefusal(t *testing.T) {
	hostile := strings.Repeat("![a](b) ", 200)
	e := thread.ConceptEntry(
		thread.Context{Transport: "slack", Title: strings.Repeat("<img x> ", 100)},
		thread.HumanNote(strings.Repeat("<b>", 40), hostile),
		sinkAt, thread.DefaultMaxNoteBytes,
	)
	// maxNoteDescriptionClaim(200) + Context(120) + author(40) + fixed framing,
	// each +2 for its ellipsis. Comfortably under 500 when the order is right;
	// 2.5-3x over it when it is not.
	if len(e.Description) > 500 {
		t.Errorf("Description is %d bytes, past the sum of its stated part budgets: %q", len(e.Description), e.Description)
	}
	// The finding-fallback title is capped exactly once, so its budget is the one
	// standing between an expansion and the merge gate's 120-byte limit.
	fallback := thread.ConceptEntry(
		thread.Context{Transport: "slack", Title: strings.Repeat("<img x> ", 100)},
		thread.HumanNote("alice", "- "), // no words: falls back to naming the finding
		sinkAt, thread.DefaultMaxNoteBytes,
	)
	if len(fallback.Title) > 120 {
		t.Errorf("fallback Title is %d bytes, over kbvalidate's 120-byte limit: %q", len(fallback.Title), fallback.Title)
	}
}

// TestNoteIdentityFieldsStayInsideTheMergeGateWhenDefused pins the ordering the
// defusal depends on. neutralizeImages expands "![" 3x and neutralizeHTML "<x"
// 2.5x, so defusing AFTER the cut would push a title past the 120-byte limit
// kbvalidate enforces — the entry would then fail the very gate the rest of this
// work exists to clear. Defusing first means the cut bounds the expanded text.
func TestNoteIdentityFieldsStayInsideTheMergeGateWhenDefused(t *testing.T) {
	// A note that is nothing but image markup: the worst case for expansion.
	e := noteEntry(t, strings.Repeat("![a](b) ", 200))
	if len(e.Title) > 120 {
		t.Errorf("Title is %d bytes, over kbvalidate's 120-byte limit: %q", len(e.Title), e.Title)
	}
	if strings.Contains(e.Title, "![") {
		t.Errorf("Title still carries live image markup: %q", e.Title)
	}
}

// firstLinkTarget returns the target of the first markdown link in line, or ""
// when there is none. It honours backslash escapes, which is the whole point:
// `\]` is a literal bracket and does NOT end the label, so a test that merely
// counted "](" would call the escaped, harmless form a hijack.
func firstLinkTarget(line string) string {
	open := strings.IndexByte(line, '[')
	if open < 0 {
		return ""
	}
	for i := open + 1; i < len(line); i++ {
		if line[i] == '\\' { // escaped character: skip what it escapes
			i++
			continue
		}
		if line[i] != ']' {
			continue
		}
		rest := line[i+1:]
		if !strings.HasPrefix(rest, "(") {
			return "" // a label that closes but opens no target is not a link
		}
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return ""
		}
		return rest[1:end]
	}
	return ""
}
