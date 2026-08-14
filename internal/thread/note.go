// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"fmt"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// maxNoteTitle bounds a generated entry title well inside the validator's own
// limit, which counts BYTES — an accented title hits it at roughly half as many
// characters.
const maxNoteTitle = 90

// NoteBody renders a human's thread reply as a KB-bound note: a provenance
// header naming who said it, where and when, followed by their words verbatim.
//
// Verbatim is the point. The human's exact wording is the evidence a reviewer
// weighs; anything that rewrites it makes the note less trustworthy than the
// Slack message it came from.
func NoteBody(tc Context, author, text string, at time.Time) string {
	var b strings.Builder
	b.WriteString("### 📝 Operator note\n\n")
	fmt.Fprintf(&b, "From **@%s** via %s on %s.\n",
		author, transportName(tc.Transport), at.UTC().Format(time.RFC3339))
	if tc.Title != "" {
		fmt.Fprintf(&b, "Thread: %s\n", tc.Title)
	}
	b.WriteString("\n")
	b.WriteString(neutralizeImages(text))
	b.WriteString("\n")
	return b.String()
}

// ConceptEntry builds the standalone KB entry for a note that has no open PR to
// land on — a recall (which never curates), a skipped verdict, or a coalesced
// finding.
//
// The type is Concept, not Incident, deliberately: kbvalidate requires the
// Symptom/Cause/Resolution body sections and a `resource` for Incident only. A
// bare operator note has neither, so typing it Concept clears the merge gate
// honestly instead of fabricating evidence sections nobody wrote.
func ConceptEntry(tc Context, author, text string, at time.Time) providers.KBEntry {
	title := "Operator note"
	if tc.Title != "" {
		title = "Operator note: " + tc.Title
	}
	title = truncate(title, maxNoteTitle)

	var body strings.Builder
	body.WriteString(NoteBody(tc, author, text, at))
	if tc.RecalledEntry != "" || tc.TriggerKey != "" || tc.Resource != "" {
		body.WriteString("\n### Context\n\n")
		if tc.RecalledEntry != "" {
			fmt.Fprintf(&body, "- Corrects or extends: `%s`\n", tc.RecalledEntry)
		}
		if tc.Resource != "" {
			fmt.Fprintf(&body, "- Resource: `%s`\n", tc.Resource)
		}
		if tc.TriggerKey != "" {
			fmt.Fprintf(&body, "- Trigger key: `%s`\n", tc.TriggerKey)
		}
		if tc.Verdict != "" {
			fmt.Fprintf(&body, "- Verdict at delivery: `%s`\n", tc.Verdict)
		}
	}

	return providers.KBEntry{
		Type:        "Concept",
		Title:       title,
		Description: truncate(fmt.Sprintf("Operator knowledge captured from a %s thread by @%s.", transportName(tc.Transport), author), maxNoteTitle*2),
		Resource:    tc.Resource,
		Tags:        []string{"operator-note", transportName(tc.Transport)},
		Body:        body.String(),
		// Fingerprint, Confidence and Provenance are deliberately unset: the dedup
		// fingerprint identifies a CURATED FINDING, and stamping a note with one
		// would collide it with the real entry in curator dedup and ByFingerprint.
	}
}

// transportName normalises an empty transport so rendering never emits "via ".
func transportName(t string) string {
	if t == "" {
		return "chat"
	}
	return t
}

// neutralizeImages defuses markdown image syntax so a note cannot embed a remote
// image (a tracking pixel, or a request from the reviewer's browser) into a PR
// body. The URL survives as ordinary text — a reviewer must still be able to see
// what was linked.
func neutralizeImages(s string) string {
	return strings.ReplaceAll(s, "![", "!&#91;")
}

// truncate shortens s to at most n BYTES, on a rune boundary, appending an
// ellipsis when it cuts. Bytes, because that is what the validator's limit
// counts.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 1
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// isRuneStart reports whether b begins a UTF-8 rune (i.e. is not a continuation
// byte).
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
