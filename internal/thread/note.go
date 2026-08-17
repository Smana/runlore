// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// maxNoteTitle bounds a generated entry title well inside the validator's own
// limit, which counts BYTES — an accented title hits it at roughly half as many
// characters.
const maxNoteTitle = 90

// DefaultMaxNoteBytes bounds one human message's INPUT, in bytes, before it
// is written to the knowledge base or shown to a model.
//
// It exists because neither bound above it is a bound on this: a Matrix event
// can carry 64 KiB and a Slack request body 1 MiB, while model.max_tokens caps
// only what a model WRITES. Without this, one message is ~16k–250k input
// tokens, on a path any channel member can trigger, at a cost nothing else
// counts. 8 KiB is far more than a human types into a chat reply and far less
// than a pathological one.
//
// This bounds the INPUT capNoteText caps, not an exact bound on NoteBody's
// rendered output, for two reasons. The markup defusals run on the capped text
// afterwards (see noteText) — the widest of them, neutralizeImages, rewrites
// each "![" (2 bytes) to "!&#91;" (6 bytes), an exact 3x expansion per
// occurrence pinned by TestNeutralizeImagesWorstCaseExpansionFactor;
// neutralizeHTML's is narrower at 2.5x and escapeOKFSections adds one byte per
// defused heading line. And a model-drafted note (see
// Note) renders TWO independently capped blocks, the draft and the human's
// quoted message, because a reviewer cannot judge the first without the
// second. A pathological all-"![" message on that route can therefore render a
// body up to roughly 6x this many bytes (~8 KiB capped in -> ~48 KiB rendered,
// worst case); an explicit "note:", which renders one block, stays at ~3x.
//
// That amplification is deliberately left open rather than closed by capping
// the ALREADY-neutralized text to an exact byte figure: doing so would
// truncate more of the human's own words than this cap needs to, purely to
// make this comment's number exact — see NoteBody's doc comment on why the
// human's verbatim wording is the point. The ~48 KiB pathological worst case
// is nowhere near the ~1 MiB / 250k-token cost this constant exists to
// bound, so the amplification does not reopen that gap — it only means this
// constant is a close, not exact, bound on the rendered body.
const DefaultMaxNoteBytes = 8 << 10

// Note is one knowledge-base-bound note: the text to file, the human it came
// from, and — when RunLore's own model wrote that text rather than the human
// typing it — the message it was drafted from.
//
// The type exists because there are two routes into the same write path and a
// KB reviewer's judgement turns entirely on telling them apart: an explicit
// "note:" carries a named engineer's own words, while the freeform route
// carries a model's paraphrase of them. Filing the second under the first's
// provenance turns an injected chat message into an apparent human attestation
// — and a human merge is the last gate protecting the recall path from a note
// body (see escapeOKFSections), so that is precisely the signal that gate reads.
type Note struct {
	// Author is the human whose thread message produced this note, exactly as
	// the chat transport reported it. Provenance, not proof of identity.
	Author string
	// Text is what gets filed.
	Text string
	// DraftedFrom is the human's ORIGINAL message when RunLore's chat model
	// wrote Text, and empty when Author typed Text themselves after an explicit
	// "note:" — so it is both the discriminator and the evidence a reviewer
	// weighs the draft against.
	//
	// One field rather than a flag plus a message, deliberately: two fields that
	// can disagree would let model-drafted text render under the human's
	// verbatim header through nothing worse than a forgotten assignment, which
	// is the exact defect this type exists to make unrepresentable. Use
	// HumanNote / ProposedNote rather than a bare literal.
	DraftedFrom string
}

// HumanNote is a note the human typed themselves, after an explicit "note:".
// Their words are filed verbatim under their name — see NoteBody.
func HumanNote(author, text string) Note {
	return Note{Author: author, Text: text}
}

// ProposedNote is note content RunLore's chat model drafted from a human's
// freeform message. Both are carried on purpose: draft is what would be filed,
// message is what a reviewer has to weigh it against. The model never wrote
// the message and the human never wrote the draft, and NoteBody renders both
// facts.
func ProposedNote(author, message, draft string) Note {
	return Note{Author: author, Text: draft, DraftedFrom: message}
}

// modelDrafted reports whether RunLore's model, not the human, wrote n.Text.
func (n Note) modelDrafted() bool { return n.DraftedFrom != "" }

// NoteBody renders a thread reply as a KB-bound note: a provenance header
// naming who said it, where and when, followed by the note text (subject to
// the maxBytes cap below).
//
// It renders TWO provenance headers, one per route, and which one it picks is
// the security-relevant part:
//
//   - An explicit "note:" (Note.DraftedFrom empty) files the human's words
//     verbatim under their name. Verbatim is the point: their exact wording is
//     the evidence a reviewer weighs, and anything that rewrites it makes the
//     note less trustworthy than the chat message it came from. Truncation is
//     the one deliberate exception — see capNoteText — and it always leaves a
//     visible mark, so "verbatim" never silently becomes "verbatim up to a
//     point nobody can see."
//   - A model-drafted note (Note.DraftedFrom set) says so in the heading and
//     the first line, attributes the text to RunLore rather than to the human,
//     and quotes the human's actual message underneath it. A reviewer must be
//     able to see what was really said and decide whether the draft is a fair
//     reading of it; without the quote the only remaining record of the human's
//     own words is the chat thread, which the PR page does not show.
//
// Every block of untrusted text is redacted on the way out — see noteText.
//
// maxBytes bounds each block of untrusted text before rendering; <= 0 falls
// back to DefaultMaxNoteBytes — never "unlimited", since an unbounded note is
// exactly the input-cost gap this parameter exists to close (see capNoteText).
// ConceptEntry (the standalone-PR write route) calls this too, so both forge-
// write routes apply the identical cap.
func NoteBody(tc Context, n Note, at time.Time, maxBytes int) string {
	who := noteField(n.Author)
	var b strings.Builder
	if n.modelDrafted() {
		b.WriteString("### 🤖 Proposed operator note — drafted by RunLore\n\n")
		fmt.Fprintf(&b, "RunLore's chat model wrote this from **@%s**'s message via %s on %s. @%s did not write it.\n",
			who, transportName(tc.Transport), at.UTC().Format(time.RFC3339), who)
	} else {
		fmt.Fprintf(&b, "### 📝 Operator note\n\nFrom **@%s** via %s on %s.\n",
			who, transportName(tc.Transport), at.UTC().Format(time.RFC3339))
	}
	if s := noteField(tc.Title); s != "" {
		fmt.Fprintf(&b, "Thread: %s\n", s)
	}
	b.WriteString("\n")
	if n.modelDrafted() {
		b.WriteString("**Proposed note:**\n\n")
	}
	b.WriteString(noteText(n.Text, maxBytes))
	b.WriteString("\n")
	if n.modelDrafted() {
		fmt.Fprintf(&b, "\n**What @%s actually wrote:**\n\n", who)
		b.WriteString(blockquote(noteText(n.DraftedFrom, maxBytes)))
		b.WriteString("\n")
	}
	return b.String()
}

// noteText prepares one block of untrusted text for a forge write: redacted,
// capped, then defused as markdown.
//
// Redaction runs FIRST, before the cap, for the same two reasons chat.go's
// renderContext gives on the provider egress. redact.Secrets needs the whole
// token to recognise it — the GitHub rule wants 20+ suffix characters, the JWT
// rule three base64url segments — so capping first hands it a half-cut secret
// it no longer matches, and the surviving prefix ships verbatim. And a mask can
// be LONGER than what it replaced, so redacting after the cap would break
// capNoteText's "at most maxBytes, always" guarantee.
//
// That a forge write needs redacting at all is the point: the knowledge base is
// not a lesser egress than the model provider. It is a git repository a whole
// team reads, a catalog the recall path serves from, and — unlike a provider
// call — permanent. A message reaching one masked and the other verbatim is not
// a boundary.
//
// The three markup defusals run LAST, on the capped text, so a heading or a
// tag that the cut itself exposed at the boundary is still defused — and so
// their expansion (see DefaultMaxNoteBytes) is applied to bounded input rather
// than to the whole message.
func noteText(s string, maxBytes int) string {
	s = capNoteText(redact.Secrets(s), maxBytes)
	return neutralizeImages(neutralizeHTML(escapeOKFSections(s)))
}

// noteField prepares one short identity field — an author, an alert-derived
// title, a resource ref — for interpolation into rendering around a note:
// redacted, and flattened to a single line.
//
// Flattening is not cosmetic here. These fields are interpolated into the
// provenance header and the context list, NOT through noteText, so a newline in
// one of them would put attacker-chosen text at the start of a line in the
// entry body — a heading of its own, which no later escape would catch because
// escapeOKFSections only ever sees the note TEXT, never these fields.
//
// It used to also guard a fence-desynchronisation trick: a "```" smuggled in
// here could flip the fence state escapeOKFSections tracked and carry a later
// heading through unescaped. That specific route is gone — escapeOKFSections no
// longer tracks fences, because kbvalidate's parser does not either — but the
// flattening still matters for the plain-heading case above.
func noteField(s string) string {
	return singleLine(redact.Secrets(s))
}

// blockquote prefixes EVERY line of s with "> ", so a quoted block cannot end
// early: quoting only the first line would let a second line of the quoted
// text render as body prose, which on the model-drafted route is exactly the
// confusion the quote exists to prevent.
//
// "Line" means what it means to the renderer, which is why the split goes
// through mandatoryBreaks rather than over "\n" alone. This was the third and
// last copy of that "\n"-only split — the two chat quoters QuoteUntrusted
// replaced were the others — and it is the one that reaches the ENTRY BODY on
// the forge, where "**What @alice actually wrote:**" is the framing at stake.
// It was materially less exposed than the chat sinks: an HTML <blockquote>
// keeps its quote bar around a break CSS honours inside it, whereas a chat
// client's "> " prefix is per source line. Same defect shape all the same, and
// this package has been bitten by it four times.
//
// It normalises breaks only. Unlike QuoteUntrusted it neither strips status
// glyphs nor marks the content untrusted, and neither is missing: this text has
// already been through noteText (redaction, the cap, and the three markdown
// defusals), the forge is not a chat system with an escaper to defer to, and
// the entry's own framing is markdown bold rather than the status glyphs a chat
// reply leads with.
func blockquote(s string) string {
	lines := strings.Split(mandatoryBreaks.Replace(s), "\n")
	for i, ln := range lines {
		lines[i] = "> " + ln
	}
	return strings.Join(lines, "\n")
}

// noteTruncationMarker renders the visible mark capNoteText leaves in place of
// the words it cut, naming how many INPUT bytes were dropped. It leads with the
// ellipsis so it reads as a continuation of the kept text.
//
// "Input" bytes, deliberately: capNoteText runs BEFORE neutralizeImages (see
// NoteBody), which can expand the KEPT portion afterwards, so this count
// describes what was cut from the human's original message, not the size of
// NoteBody's final rendered output — see DefaultMaxNoteBytes' doc comment for
// that distinction.
func noteTruncationMarker(dropped int) string {
	return fmt.Sprintf("…\n\n_(truncated — %d input bytes dropped)_", dropped)
}

// capNoteText bounds text to maxBytes (falling back to DefaultMaxNoteBytes
// when maxBytes <= 0) on a rune boundary. Unlike truncate — used elsewhere in
// this file for machine-generated fields like a title, where a silent
// shortening is harmless — this leaves a marker naming how many INPUT bytes
// were dropped: the human must be able to tell their words were cut, and a KB
// reviewer must not read a silently-shortened note as complete.
//
// The returned value is at most maxBytes bytes, ALWAYS. The marker is reserved
// INSIDE the budget rather than appended after the cut, because a cap that the
// act of marking the cut then breaks is not a cap — the previous form overshot
// by the marker's own ~45 bytes on every truncated note.
//
// The reservation is sized from len(text), not from the actual dropped count:
// dropped can never exceed len(text), and a smaller number never renders as
// more decimal digits, so formatting the marker with len(text) is a safe upper
// bound on the marker's own length. Sizing it from the actual count instead
// would be circular — the count depends on where the cut lands, which depends
// on the reservation.
//
// PATHOLOGICAL CASE: a maxBytes smaller than the marker itself (roughly 45
// bytes) leaves no room for both the human's words and the mark saying they
// were cut. The cap wins: the result is the marker alone, itself cut to
// maxBytes on a rune boundary, so the bound holds unconditionally and nothing
// underflows or panics. Every shipped path passes DefaultMaxNoteBytes or an
// operator's notify.thread.max_note_bytes, and config.Validate already rejects
// a negative one, so a cap that small is a misconfiguration this degrades
// predictably under rather than a case any real deployment reaches.
func capNoteText(text string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxNoteBytes
	}
	if len(text) <= maxBytes {
		return text
	}
	reserved := len(noteTruncationMarker(len(text)))
	if maxBytes < reserved {
		return cutToRuneBoundary(noteTruncationMarker(len(text)), maxBytes)
	}
	kept := cutToRuneBoundary(text, maxBytes-reserved)
	return kept + noteTruncationMarker(len(text)-len(kept))
}

// cutToRuneBoundary returns the longest prefix of s that is at most n bytes
// long and never splits a UTF-8 rune. It appends nothing — unlike truncate,
// whose "…" would itself push the result past the caller's budget, which is
// the whole reason capNoteText cannot reuse it.
func cutToRuneBoundary(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// noteForgeLabel marks a standalone KB PR opened from a human's thread reply
// (see ConceptEntry) so curate's auto-closing passes (dedup, the stale sweep)
// can recognise and skip it — see isOperatorNote in internal/curate.
//
// It matters because ConceptEntry deliberately leaves Fingerprint unset (a
// note is not a curated finding — see the comment below), so two notes are
// ALWAYS compared by dedup's title-Jaccard fallback. Every note PR shares the
// title prefix "KB: Operator note: <finding title>", so two notes on the same
// recurring incident score a Jaccard of 1.0 — the strongest possible dedup
// match. Without this label RunLore would silently close a human's
// correction as a "duplicate" of another human's correction, which defeats
// the entire premise of thread capture.
//
// internal/thread depends only on providers and internal/catalog by design,
// so this literal is duplicated — not imported — in internal/curate. Kept in
// sync by hand, the same tradeoff already made for neutralizeImages below.
const noteForgeLabel = "runlore-operator-note"

// ConceptEntry builds the standalone KB entry for a note that has no open PR to
// land on — a recall (which never curates), a skipped verdict, or a coalesced
// finding.
//
// The type is Concept, not Incident, deliberately: kbvalidate requires the
// Symptom/Cause/Resolution body sections and a `resource` for Incident only. A
// bare operator note has neither, so typing it Concept clears the merge gate
// honestly instead of fabricating evidence sections nobody wrote.
//
// maxBytes is forwarded to NoteBody unchanged — see its doc comment for the
// <= 0 fallback — so this route and the comment-on-PR route in
// Responder.write apply the identical cap.
//
// Every context field it renders is redacted, not only the note text NoteBody
// handles: this route writes a whole KB FILE, which the catalog then indexes
// and recall serves — a secret landing in an entry's title or trigger key
// outlives the pull request it arrived on. Redaction is idempotent, so a field
// NoteBody already masked costs nothing here.
func ConceptEntry(tc Context, n Note, at time.Time, maxBytes int) providers.KBEntry {
	title := "Operator note"
	if tc.Title != "" {
		title = "Operator note: " + noteField(tc.Title)
	}
	title = truncate(title, maxNoteTitle)

	var body strings.Builder
	body.WriteString(NoteBody(tc, n, at, maxBytes))
	if tc.RecalledEntry != "" || tc.TriggerKey != "" || tc.Resource != "" {
		body.WriteString("\n### Context\n\n")
		if tc.RecalledEntry != "" {
			fmt.Fprintf(&body, "- Corrects or extends: `%s`\n", noteField(tc.RecalledEntry))
		}
		if tc.Resource != "" {
			fmt.Fprintf(&body, "- Resource: `%s`\n", noteField(tc.Resource))
		}
		if tc.TriggerKey != "" {
			fmt.Fprintf(&body, "- Trigger key: `%s`\n", noteField(tc.TriggerKey))
		}
		if tc.Verdict != "" {
			fmt.Fprintf(&body, "- Verdict at delivery: `%s`\n", noteField(string(tc.Verdict)))
		}
	}

	return providers.KBEntry{
		Type:        "Concept",
		Title:       title,
		Description: truncate(conceptDescription(tc, n), maxNoteTitle*2),
		Resource:    tc.Resource,
		Tags:        []string{"operator-note", transportName(tc.Transport)},
		ExtraLabels: []string{noteForgeLabel},
		Body:        body.String(),
		// Fingerprint, Confidence and Provenance are deliberately unset: the dedup
		// fingerprint identifies a CURATED FINDING, and stamping a note with one
		// would collide it with the real entry in curator dedup and ByFingerprint.
	}
}

// conceptDescription renders the entry's one-line description, which is the
// only provenance a catalog listing or a search hit shows — the body's
// heading never reaches either. It therefore has to make the same
// human-wrote-it / model-drafted-it distinction NoteBody's header does,
// otherwise the distinction survives only for a reader who opens the entry.
func conceptDescription(tc Context, n Note) string {
	if n.modelDrafted() {
		return fmt.Sprintf("Note drafted by RunLore from @%s's %s thread message, pending review.", noteField(n.Author), transportName(tc.Transport))
	}
	return fmt.Sprintf("Operator knowledge captured from a %s thread by @%s.", transportName(tc.Transport), noteField(n.Author))
}

// transportName normalises an empty transport so rendering never emits "via ".
func transportName(t string) string {
	if t == "" {
		return "chat"
	}
	return t
}

// neutralizeImages defuses Markdown image syntax (`![...](...)`) so a note
// cannot embed a remote image (a tracking pixel, or a request fired from a
// reviewer's browser) into a PR body via that syntax. The URL survives as
// ordinary text — a reviewer must still be able to see what was linked.
//
// The raw-HTML half of the same problem is neutralizeHTML's, below; this
// function is kept to the "![" rewrite alone because
// TestNeutralizeImagesWorstCaseExpansionFactor pins its exact 3x expansion,
// which DefaultMaxNoteBytes' doc comment reasons from.
// internal/forge/github.neutralizeImages and internal/forge/gitlab.
// neutralizeImages share this function's name and purpose but are separate,
// regex-based implementations that STILL have the HTML gap — none of the
// three is a shared helper, and only this one is on the thread-note path.
func neutralizeImages(s string) string {
	return strings.ReplaceAll(s, "![", "!&#91;")
}

// htmlTagStart matches a "<" that begins a tag, an end tag, a comment or a
// declaration — i.e. every form a renderer treats as markup. The following
// character is captured so it can be put back.
var htmlTagStart = regexp.MustCompile(`<([A-Za-z!/?])`)

// neutralizeHTML defuses raw HTML in a note body, closing the gap
// neutralizeImages' own comment used to admit: `<img src="https://evil.example/
// px.gif">` passed through untouched, and both GitHub and GitLab render raw
// HTML in PR/MR bodies. On a PR page under review that image is a tracking
// pixel reporting to whoever wrote the note that a human is reading it.
//
// It escapes the "<", not the whole tag: `&lt;` renders as a literal "<", so
// the reviewer sees exactly the text that was submitted while the renderer
// sees no markup — the same neutralise-don't-censor tradeoff neutralizeImages
// makes with the image URL.
//
// It is deliberately narrower than escaping every "<": a bare less-than in
// prose ("replicas < 3", "latency < 200ms") is arithmetic, and rewriting it
// would plant noise in every note that mentions a threshold. Only a "<"
// immediately followed by a tag-, end-tag-, comment- or declaration-opening
// character is markup. Narrowing it this way also keeps the worst-case
// expansion at 2.5x per occurrence, under neutralizeImages' 3x, so
// DefaultMaxNoteBytes' stated bound still holds.
func neutralizeHTML(s string) string {
	return htmlTagStart.ReplaceAllString(s, "&lt;$1")
}

// okfSectionNames are the OKF body sections a note body must never introduce.
// It restates internal/kbvalidate's requiredIncidentSections, which is
// unexported; TestOKFSectionNamesMatchTheMergeGate pins the two to be the same
// SET by exercising kbvalidate.HasIncidentSections, so a section added to or
// removed from the gate fails a test rather than silently leaving a forgeable
// heading behind.
//
// Restating rather than importing keeps internal/thread — a chat-transport
// layer — off the merge-gate package, which is a CI concern. The pin is what
// makes that safe.
var okfSectionNames = []string{"Symptom", "Cause", "Resolution"}

// escapeOKFSections defuses ATX headings in untrusted text whose heading text
// collides with an OKF section name, by escaping the leading "#".
//
// A note is EVIDENCE FOR A REVIEWER, never a section of the entry it lands in.
// A note body containing "## Cause" used to reach a merged Concept entry
// verbatim, after which catalog.Entry.Section("Cause") returned the note's own
// words and investigate/recall's instant-recall short-circuit served them to
// an on-call as "Known cause: …" — no model and no fence anywhere in that
// path, and recall gates only on status (retired/draft), with no entry-type
// check that would exclude an operator note.
//
// Escaping, not stripping: "\#" renders as a literal "#", so the reviewer
// still reads the words the human (or the model) wrote, and only the structure
// the parser reads is removed. Demoting the level would not work at all —
// catalog.headingText accepts every level from "#" to "######", so "### Cause"
// is just as much a section as "## Cause".
//
// Fenced headings are escaped TOO, and deliberately so, even though a heading
// inside a code fence is not a heading to a Markdown renderer.
//
// The reason is that RunLore has TWO section parsers and they disagree.
// catalog.Entry.Section (catalog/section.go:25) tracks fences and skips what is
// inside them. kbvalidate.Sections (kbvalidate/kbvalidate.go:208) does not: it
// walks every line and calls heading(line) with no fence state at all, and
// HasIncidentSections is built on it. This function used to mirror the first
// one exactly — which read as careful, and was the hole. A note that wrapped
//
//	## Symptom / ## Cause / ## Resolution
//
// in a ``` fence was escaped NOWHERE and parsed EVERYWHERE the fence-blind
// parser looks, so HasIncidentSections returned true for a body a stranger
// typed into a chat thread.
//
// So the rule is the UNION, not either parser: escape every shape ANY reader
// accepts as a section. Over-escaping costs a visible backslash inside a pasted
// code block; under-escaping costs a forged entry. Aligning the two parsers is
// the better long-term fix, but kbvalidate is the merge gate and changing what
// it accepts reaches every existing entry, so that is not this function's call
// to make.
//
// Callers must still flatten anything they interpolate around this text (see
// NoteBody): identity fields are single-lined before they reach here.
func escapeOKFSections(s string) string {
	if !strings.Contains(s, "#") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		h := atxHeadingText(strings.TrimSpace(ln))
		if h == "" {
			continue
		}
		for _, name := range okfSectionNames {
			if strings.EqualFold(h, name) {
				lines[i] = strings.Replace(ln, "#", `\#`, 1)
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// atxHeadingText mirrors catalog.headingText — the ATX parse the READ path
// actually performs — because internal/catalog does not export it. Matching
// that parse is the whole job: a shape this accepts but Section does not costs
// a needless escape, while a shape Section accepts and this does not is a hole.
// TestNoteBodyCannotForgeAnOKFSection pins the pair against
// catalog.Entry.Section itself for every ATX spelling, so the mirror is
// checked against the real parser rather than against this comment.
func atxHeadingText(line string) string {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return ""
	}
	return strings.TrimSpace(line[i:])
}

// singleLine collapses every rune that can break or reorder a line — any
// Unicode whitespace other than the plain space, every control character (Cc)
// and every format character (Cf) — to a space, so text pulled from untrusted
// sources (an alert title, in particular) can never break a single-line YAML
// title. kbvalidate rejects any title containing \r or \n outright.
//
// The three categories, and why each is here rather than just \r and \n:
//
//   - unicode.IsSpace covers U+2028 LINE SEPARATOR and U+2029 PARAGRAPH
//     SEPARATOR, which YAML 1.1 counts as line breaks and which many
//     renderers and tokenizers break on. Those are the ones that made the
//     old Cc-only check a real gap rather than a pedantic one: chatSafe is
//     the only thing keeping the UNFENCED framing region of the chat prompt
//     single-line (see chat.go's renderContext), so a U+2028 in an
//     alert-derived title could plausibly forge a framing line there.
//   - unicode.IsControl is Cc, the category the previous implementation
//     tested, and is kept.
//   - unicode.Cf is the format category: U+200B ZERO WIDTH SPACE, U+FEFF, and
//     the bidi controls U+202E / U+061C, which reorder how a line RENDERS
//     without changing a byte of what it says — a title that reads one way to
//     a reviewer and another to the parser.
//
// The plain space is excluded so ordinary text is left exactly as written;
// everything else in those categories becomes one, which keeps the result the
// same length in runes and never joins two words.
func singleLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' {
			return r
		}
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

// truncate shortens s to around n bytes: it cuts at or before byte index n-1
// (walking back to a rune boundary) and appends a 3-byte "…", so the result
// can be up to n+2 bytes in the worst case. Bytes, because that is what the
// validator's limit counts.
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
