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

// reservedBundleFiles are the OKF bundle files a curation PR touches ALONGSIDE
// the entry it files (see UpdateIndex and UpdateLog, and the forge callers that
// apply them). Subtracting them from a pull request's changed paths is what
// leaves the entry itself.
var reservedBundleFiles = []string{"index.md", "log.md"}

// EntryFile picks the ONE catalog entry among a pull/merge request's changed
// paths.
//
// It exists so a LATER commit onto that request — an operator note appended to
// the entry it carries, see thread.Forge.AppendToEntryOnPR — can find the file
// to edit without any caller having to remember where it was first written. A
// RunLore curation request changes the entry plus, at most, the reserved pair
// above, so what remains after removing them is the entry.
//
// Anything other than exactly one candidate is an ERROR rather than a guess.
// The caller's next move is a commit into a pull request a human is reviewing,
// and committing into the wrong file there is strictly worse than refusing:
// refusing degrades to a comment, and a wrong write silently rewrites somebody
// else's change with no signal that it happened.
//
// WHAT IT DOES NOT GUARANTEE, stated plainly because an earlier version of this
// comment overclaimed it: the result is "the one changed path that is a
// non-reserved .md", which is not the same proposition as "the catalog entry".
// A request that renamed the entry and also touched some other markdown resolves
// to whichever of the two the filter leaves, with no error, because there is
// nothing here to compare against. Nor can it detect a TRUNCATED listing: one
// page of a large request that happens to hold exactly one non-reserved .md
// resolves just as cleanly to the wrong file. Callers therefore verify that what
// they got is an entry BEFORE writing to it (HasFrontmatter) and refuse a listing
// that may have been cut. This function narrows the candidates; it is not the
// last gate.
//
// It lives here, beside the functions that put those reserved files on the
// branch in the first place, and is shared by both forge clients for the reason
// this file's header gives: two copies of a rule about the bundle's shape drift,
// and a KB would then behave differently depending on which forge hosts it.
func EntryFile(changed []string) (string, error) {
	var entries []string
	for _, p := range changed {
		if !strings.HasSuffix(p, ".md") || slices.Contains(reservedBundleFiles, p) {
			continue
		}
		entries = append(entries, p)
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("want exactly one catalog entry among the changed files, found %d: %v", len(entries), entries)
	}
	return entries[0], nil
}

// AppendBlock adds block to the end of an OKF entry's markdown, separated from
// what is already there by one blank line and terminated by a newline.
//
// One blank line and NOTHING else: no "---" rule, no heading of its own. A
// horizontal rule would be a second "---" in a file whose FIRST one delimits the
// YAML frontmatter, and while every parser RunLore ships takes the first match
// (see catalog.SplitFrontmatter, forge/github.frontmatterBlock), inviting a
// reader of the raw file to wonder is not worth a divider. The blocks callers
// append already open with their own heading, which separates them on the page.
//
// An empty block returns existing untouched — appending nothing must not rewrite
// the file, since the caller's commit is a real change in a human's pull request.
func AppendBlock(existing []byte, block string) []byte {
	block = strings.Trim(block, "\n")
	if block == "" {
		return existing
	}
	cur := strings.TrimRight(string(existing), "\n")
	if cur == "" {
		return []byte(block + "\n")
	}
	return []byte(cur + "\n\n" + block + "\n")
}

// HasFrontmatter reports whether raw opens with a CLOSED YAML frontmatter fence,
// i.e. whether it is an OKF entry rather than something else that happens to be
// markdown.
//
// It exists for one caller shape and one failure: an append that reads the entry,
// appends to what it read, and writes the result back. If the read yields
// nothing, AppendBlock returns the block ALONE (that is its correct behaviour —
// it is how a first draft is built), and the write then replaces a whole entry
// with the newest note. Frontmatter, every earlier note, gone, and the write
// reports success because it carried a valid blob sha.
//
// The empty case is not hypothetical. GitHub's contents API answers a blob over
// 1 MB with an EMPTY content field and encoding "none", which base64-decodes
// without error, so a "successful" read hands back zero bytes. Callers must treat
// a false here as a refusal, not a reason to create the file.
//
// The fence must be FIRST: leading blank lines or an HTML comment before it mean
// the file is not what the caller thinks it is, and guessing is the behaviour
// this exists to prevent. A lone opening fence with no close is likewise refused.
func HasFrontmatter(raw []byte) bool {
	s := string(raw)
	const fence = "---\n"
	if !strings.HasPrefix(s, fence) {
		return false
	}
	return strings.Contains(s[len(fence):], "\n---")
}

// NoteMarker renders the invisible idempotency marker an appended note carries,
// keyed to that note. It is an HTML comment so it never renders in the entry a
// human reviews or the catalog indexes.
//
// The key must identify ONE note. A marker matching any note would suppress
// every note after the first — the original bug wearing a different hat.
func NoteMarker(key string) string { return "<!-- runlore-note:" + key + " -->" }

// WithNoteMarker returns block carrying the marker for key, so the HasNoteMarker
// check that runs before the NEXT append to the same entry finds it.
//
// It is the write half of that pair and lives beside it for the reason this
// file's header gives: both forge clients build this block, and two copies of a
// rule about the bundle's bytes drift. A marker written one way and looked for
// another is not a cosmetic drift — it is the idempotency gone, silently, on
// whichever forge fell behind.
//
// An empty key means "no idempotency", the same thing it means to HasNoteMarker:
// the block is returned unmarked rather than carrying a keyless marker that
// every later note would then match and be dropped by.
func WithNoteMarker(block, key string) string {
	if key == "" {
		return block
	}
	return block + "\n\n" + NoteMarker(key)
}

// HasNoteMarker reports whether raw already carries the note keyed by key, so a
// replayed delivery appends nothing a second time.
//
// A comment is visibly duplicate and dies at merge; a duplicated APPEND is
// permanent catalog content that kb_search then indexes twice. Retries are real:
// the Slack event dedupe is per-process and its seen-set is cleared wholesale
// when full, so a restart, a failover or a busy channel all replay.
//
// An empty key means "no idempotency", never "matches everything" — the latter
// would drop every note from any caller that omitted a key.
func HasNoteMarker(raw []byte, key string) bool {
	if key == "" {
		return false
	}
	return strings.Contains(string(raw), NoteMarker(key))
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
