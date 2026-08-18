// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"strings"
	"unicode/utf8"
)

// sectionMaxRunes caps a Section excerpt: enough to quote an entry's cause or
// resolution in a chat notification / PR body without reproducing the document.
const sectionMaxRunes = 300

// Section returns the first paragraph of the entry body's "## <name>" markdown
// section, flattened to a single line — the quotable essence of what the entry
// says. Matching is case-insensitive and accepts any ATX heading level. Bold
// markers (**) are stripped: the excerpt is interpolated into Slack mrkdwn and
// PR bodies, where a literal ** renders as stray asterisks. Returns "" when the
// section is absent or empty — callers must treat that as "nothing to quote"
// and never render an empty block.
func (e Entry) Section(name string) string {
	want := strings.TrimSpace(name)
	var para []string
	in := false
	lines := strings.Split(e.Body, "\n")
	fenced := FencedLines(lines)
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		// Fenced code blocks are opaque: a "# comment" line inside one is code,
		// not a section boundary, and commands aren't quotable prose — skip the
		// whole fence (markers included) wherever it appears, so the excerpt is
		// the section's first PROSE paragraph.
		if fenced[i] || strings.HasPrefix(trimmed, "```") {
			continue
		}
		if h := headingText(trimmed); h != "" {
			if in {
				break // next section starts: the excerpt is done
			}
			in = strings.EqualFold(h, want)
			continue
		}
		if !in {
			continue
		}
		if trimmed == "" {
			if len(para) > 0 {
				break // blank line after content: first paragraph is done
			}
			continue // leading blank between the heading and its content
		}
		para = append(para, trimmed)
	}
	s := strings.ReplaceAll(strings.Join(para, " "), "**", "")
	return truncateRunes(s, sectionMaxRunes)
}

// FencedLines reports, per line, whether it sits inside a fenced code block —
// markers included. It is the single answer to "is this line code?" that RunLore's
// two section parsers share: this file's Entry.Section, and kbvalidate.Sections
// (the merge gate). Exported for exactly that reason.
//
// They used to answer it separately, and one of them did not answer it at all.
// Entry.Section tracked fences; kbvalidate.Sections walked every line with no
// fence state, so a "# comment" opening a resolution's command block ended the
// section, and OKF headings wrapped in ``` forged a complete Incident for the
// merge gate while rendering as an inert code sample. thread.escapeOKFSections
// had to escape the UNION of both parsers' heading shapes to cover the gap. One
// implementation is the only version of this that cannot drift back apart.
//
// An UNTERMINATED fence marks nothing. The naive toggle treats a single stray
// ``` as opening a block that never closes, so every heading after it disappears
// — which for the merge gate means rejecting an otherwise complete entry over an
// authoring slip, and for an excerpt means silently having nothing to quote.
// Only a CLOSED pair marks its range, so an unmatched opener degrades to
// ordinary text.
func FencedLines(lines []string) []bool {
	fenced := make([]bool, len(lines))
	open := -1
	for i, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "```") {
			continue
		}
		if open < 0 {
			open = i
			continue
		}
		for j := open; j <= i; j++ {
			fenced[j] = true
		}
		open = -1
	}
	return fenced
}

// headingText returns the text of an ATX markdown heading line ("## Cause" →
// "Cause"), or "" when the line is not a heading.
func headingText(line string) string {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return ""
	}
	return strings.TrimSpace(line[i:])
}

// truncateRunes caps s at n runes, appending … when cut — rune-aware so a
// multibyte character is never split.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n]), " ") + "…"
}
