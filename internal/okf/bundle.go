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
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// UpdateIndex appends the entry's link line to its "## <Type>s" section of an
// OKF index (creating the section at the end when absent). Links are relative to
// the bundle root, matching the seed index style.
//
// Callers apply this ONLY to a bundle that already has an index.md: an index's
// structure is the owner's choice, so RunLore never invents one.
func UpdateIndex(existing []byte, e providers.KBEntry, entryPath string) []byte {
	section := "## " + e.Type + "s"
	line := fmt.Sprintf("- [%s](%s) — %s", e.Title, entryPath, e.Description)

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
	out := append(append(append([]string{}, lines[:end]...), line), lines[end:]...)
	return []byte(strings.Join(out, "\n") + "\n")
}

// UpdateLog records the entry in an OKF log: flat date-grouped entries, newest
// first, bold action word (§7). A nil/empty existing log gets the standard shape,
// so callers CREATE log.md when the bundle lacks it — unlike index.md, its shape
// is fully specified by the spec, so there is nothing to impose.
func UpdateLog(existing []byte, e providers.KBEntry, entryPath, date string) []byte {
	heading := "## " + date
	line := fmt.Sprintf("* **Creation**: Added [%s](%s).", e.Title, entryPath)

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
		out := append(append(append([]string{}, lines[:i+1]...), "", line), lines[i+1:]...)
		return []byte(strings.Join(out, "\n") + "\n")
	}
	// New (newest) date: insert the heading after the H1 title, before older dates.
	at := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "## ") {
			at = i
			break
		}
	}
	out := append(append(append([]string{}, lines[:at]...), heading, "", line, ""), lines[at:]...)
	return []byte(strings.Join(out, "\n") + "\n")
}
