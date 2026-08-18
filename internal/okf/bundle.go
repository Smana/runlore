// SPDX-License-Identifier: Apache-2.0

package okf

// This file is OKF bundle maintenance: the pure functions that keep a bundle's
// reserved index.md / log.md in step with an entry a curation PR adds, so the
// bundle stays self-describing for every OKF consumer (progressive-disclosure
// index, chronological change log) and the reviewer sees the whole change in one
// diff.
//
// They live HERE, not in a forge package, because the shape they produce is the
// OKF spec's, not any one forge's: internal/forge/github applies them with four
// contents-API calls (read + PUT per file), internal/forge/gitlab folds them into
// the single Commits-API call that already writes the entry. Two copies of this
// markdown surgery would drift silently — the bundle would end up shaped
// differently depending on which forge hosted the KB.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// UpdateIndex appends the entry's link line to its "## <Type>s" section of an
// OKF index (creating the section at the end when absent). Links are relative to
// the bundle root, matching the seed index style.
//
// Callers apply this ONLY to a bundle that already has an index.md: an index's
// structure is the owner's choice, so RunLore never invents one.
//
// The title goes through linkText: it is the LABEL of a markdown link, and a "]"
// in it closes the link early — see that function.
func UpdateIndex(existing []byte, e providers.KBEntry, entryPath string) []byte {
	section := "## " + e.Type + "s"
	line := fmt.Sprintf("- [%s](%s) — %s", linkText(e.Title), entryPath, e.Description)

	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == section {
			start = i
			break
		}
	}
	if start == -1 {
		lines = append(lines, "", section, "", line)
		return []byte(strings.Join(lines, "\n") + "\n")
	}
	// Insert at the section's end: just before the next heading, or at EOF.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			end = i
			break
		}
	}
	// Trim the section's trailing blank lines so the new line joins the list.
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return []byte(strings.Join(slices.Insert(lines, end, line), "\n") + "\n")
}

// UpdateLog records the entry in an OKF log: flat date-grouped entries, newest
// first, bold action word (§7). A nil/empty existing log gets the standard shape,
// so callers CREATE log.md when the bundle lacks it — unlike index.md, its shape
// is fully specified by the spec, so there is nothing to impose.
func UpdateLog(existing []byte, e providers.KBEntry, entryPath, date string) []byte {
	heading := "## " + date
	line := fmt.Sprintf("* **Creation**: Added [%s](%s).", linkText(e.Title), entryPath)

	cur := strings.TrimRight(string(existing), "\n")
	if strings.TrimSpace(cur) == "" {
		return []byte("# Knowledge catalog update log\n\n" + heading + "\n\n" + line + "\n")
	}
	lines := strings.Split(cur, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != heading {
			continue
		}
		// Today's heading exists (it is the newest — logs are newest-first):
		// slot the line right under it.
		return []byte(strings.Join(slices.Insert(lines, i+1, "", line), "\n") + "\n")
	}
	// New (newest) date: insert the heading after the H1 title, before older dates.
	at := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "## ") {
			at = i
			break
		}
	}
	return []byte(strings.Join(slices.Insert(lines, at, heading, "", line, ""), "\n") + "\n")
}

// linkTextEscaper escapes the three characters that let an entry TITLE break out
// of the markdown link label it is rendered as. Backslash first, so the escapes
// this adds are not themselves re-escaped.
var linkTextEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)

// linkText prepares a title for use as the LABEL of a markdown link — the "%s"
// in "[%s](path)".
//
// A "]" in the label closes the link early, so everything after it re-parses:
// a title of `boom](https://evil.example) x` rendered `[boom](https://evil.example) x](path.md)`,
// a working link to the attacker's URL where the index's own link to the entry
// should be. Backslash escapes are the CommonMark answer — `\]` is a literal
// "]" and does not close the label — so the title still reads as written.
//
// It escapes rather than strips because these lines are the catalog's human
// index: a title is the one string a reader scans the file for, and silently
// dropping characters from it would make the index disagree with the entry.
//
// It is applied HERE, at the point the structural assumption is made, rather
// than at each producer. Both producers are exposed: internal/curator titles an
// entry from model-written text, and internal/thread titles an operator note
// from a chat message any channel member can send. A defusal in one of them
// would leave the other open, and neither is the place that decided a title
// would be a link label.
func linkText(s string) string { return linkTextEscaper.Replace(s) }
