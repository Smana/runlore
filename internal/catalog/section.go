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
	for _, ln := range strings.Split(e.Body, "\n") {
		trimmed := strings.TrimSpace(ln)
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

// truncateRunes caps s at max runes, appending … when cut — rune-aware so a
// multibyte character is never split.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:max]), " ") + "…"
}
