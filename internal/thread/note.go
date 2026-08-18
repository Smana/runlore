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

// noteTitlePrefix leads every generated entry title. What follows it is the
// NOTE's own claim, not the finding's headline — see noteEntryTitle.
const noteTitlePrefix = "Operator note: "

// The description's parts are each bounded on their own, so the line as a whole
// is bounded without a final truncation — which would cut the provenance clause,
// the one part that must never be lost (see conceptDescription).
const (
	// maxNoteDescriptionClaim gives the note's own words room for roughly two
	// sentences. Bigger than the title's share deliberately: the description is
	// the half of instant recall's root-cause claim that can afford to be
	// complete, and the merge gate puts no limit on it (only `title` is capped).
	maxNoteDescriptionClaim = 200
	// maxNoteDescriptionContext bounds the finding title carried as context. 120
	// is the merge gate's own title limit, which curator.CapTitle already caps
	// every curated title to — so an ordinary finding arrives here WHOLE, and the
	// alert's distinguishing words (the ones that make the note reachable from
	// the alert again) are not the ones a cut takes off the end.
	maxNoteDescriptionContext = 120
	// maxNoteAuthor bounds a chat handle. Defensive: an author string comes from
	// the transport, and nothing else here caps it.
	maxNoteAuthor = 40
)

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
	return SingleLine(redact.Secrets(s))
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
// title prefix "KB: Operator note: ", and two humans correcting the same
// recurring incident write about the same thing in much the same words, so
// their titles score near the top of that fallback. Without this label RunLore
// would silently close a human's correction as a "duplicate" of another
// human's correction, which defeats the entire premise of thread capture.
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

	// Derived ONCE, from the same bounded text the body above files, and shared by
	// both identity fields — see noteClaim and noteClaimSource.
	claim := noteClaim(n.Text, maxBytes)
	return providers.KBEntry{
		Type:        "Concept",
		Title:       noteEntryTitle(tc, claim),
		Description: conceptDescription(tc, n, claim),
		// Narrowed to the one whitespace-free value the merge gate accepts — the
		// body's Context section above still carries tc.Resource in full. See
		// providers.EntryResourceRef for what this closes and why it narrows
		// rather than drops.
		Resource:    providers.EntryResourceRef(tc.Resource),
		Tags:        []string{"operator-note", transportName(tc.Transport)},
		ExtraLabels: []string{noteForgeLabel},
		Body:        body.String(),
		// Fingerprint, Confidence and Provenance are deliberately unset: the dedup
		// fingerprint identifies a CURATED FINDING, and stamping a note with one
		// would collide it with the real entry in curator dedup and ByFingerprint.
	}
}

// noteEntryTitle names the entry after the NOTE, not after the finding the note
// was written in reply to.
//
// The finding's title used to BE the identity ("Operator note: <finding
// title>"), and that is wrong twice over.
//
// It files a note that CORRECTS a finding under the headline of the very thing
// it corrects. Live, the reranker then matched such an entry and quoted the
// refuted hypothesis back as its grounds for matching.
//
// And it truncated on a byte boundary, so the title routinely ended mid-word
// ("… stu…"). That is not cosmetic: instant recall builds its whole root-cause
// claim as `title + " — " + description` (investigate.recalledInvestigation),
// and a Concept note has no Cause/Resolution sections to add to it — by
// construction, since escapeOKFSections exists to stop a note forging them. So
// those two fields ARE the claim verify judges, verify correctly rejected an
// unintelligible one every time, and a corrected entry could never take the
// cheap instant-recall path it exists to earn.
//
// The finding is not dropped, it is demoted from identity to CONTEXT:
// conceptDescription names it, and the body's "Thread:" line and Context section
// carry it in full.
// claim is the note's own words, already bounded and defused by noteClaim; the
// title re-cuts it to its own smaller budget.
func noteEntryTitle(tc Context, claim string) string {
	if claim != "" {
		return noteTitlePrefix + truncateWords(claim, maxNoteTitle-len(noteTitlePrefix))
	}
	// Only reachable when the note is empty or is nothing but markup:
	// Responder.Handle answers an empty "note:" without writing, and freeform
	// files nothing for an empty draft. Naming the finding still beats a bare
	// "Operator note", and "on" reads as ABOUT the finding rather than as a claim
	// the note makes about it.
	if s := noteLine(tc.Title); s != "" {
		return truncateWords("Operator note on "+s, maxNoteTitle)
	}
	return "Operator note"
}

// noteClaim renders the note's own leading words as one bounded, single-line
// claim: drawn from at most maxBytes of the message (noteClaimSource), redacted,
// squeezed and defused (noteLine), stripped of the Markdown a chat reply opens
// with, and cut on a WORD boundary.
//
// It is bounded to maxNoteDescriptionClaim — the largest budget any caller has —
// and callers needing less re-cut the result, so this runs ONCE per entry rather
// than once per field. Two calls over a 1 MiB message cost ~1 s and ~86 MB
// between them; see noteClaimSource for the rest of that story.
//
// It returns "" for a note that has no word in it at all — a lone bullet, a row
// of punctuation. "Operator note: -" is not an identity, and the caller has a
// better fallback than a title that says nothing.
func noteClaim(text string, maxBytes int) string {
	s := stripLeadingMarkup(noteLine(noteClaimSource(text, maxBytes)))
	if !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return ""
	}
	return truncateWords(s, maxNoteDescriptionClaim)
}

// noteClaimSource bounds the raw message a claim is drawn from to maxBytes — the
// operator's own notify.thread.max_note_bytes, the same cap the BODY is filed
// under.
//
// Two reasons, and the second is the one that makes maxBytes the right number
// rather than merely a small one.
//
// Cost. Nothing upstream bounds Note.Text: DefaultMaxNoteBytes is applied inside
// NoteBody -> noteText -> capNoteText, which is downstream of here, while a
// Matrix event carries up to 64 KiB and a Slack request body up to 1 MiB. Run
// unbounded, one claim over a 1 MiB message costs ~500 ms and ~43 MB in
// redact.Secrets and strings.Fields alone — roughly what the whole entry cost
// before claims existed — on a path any channel member can trigger. Bounded, the
// same claim costs ~3.7 ms and ~0.35 MB.
//
// Coherence. The title and description must never quote words the entry BODY
// does not contain, and the body holds maxBytes of the message. Drawing the
// claim from more text than the body kept would let an entry be titled after a
// sentence a reviewer cannot find anywhere in it.
//
// The trailing partial token is dropped whenever the cut actually bites, and
// that is a REDACTION guarantee, not tidiness. noteText redacts BEFORE it caps,
// precisely because redact.Secrets needs a whole token to recognise one — the
// GitHub rule wants 20+ suffix characters, the JWT rule three segments — so a
// half-cut secret is one it no longer matches, and the surviving prefix would
// ship verbatim into a title. Here the cut necessarily comes first (redacting
// first is the cost this function exists to avoid), so the cut is made harmless
// instead: a secret can only ever be severed AT the cut, and the severed piece
// is exactly the last token. Dropping it means no fragment of a secret can reach
// a claim, whatever maxBytes an operator configures — which a "maxBytes is much
// larger than the claim budget" argument would NOT give, since config.Validate
// rejects only a negative one.
//
// A message with no whitespace at all in its first maxBytes is one unbroken
// token — a base64 blob, not a sentence — and yields no claim rather than a
// possibly-severed one. The title then falls back to naming the finding.
func noteClaimSource(text string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxNoteBytes
	}
	if len(text) <= maxBytes {
		return text
	}
	cut := cutToRuneBoundary(text, maxBytes)
	i := strings.LastIndexFunc(cut, unicode.IsSpace)
	if i < 0 {
		return ""
	}
	return cut[:i]
}

// conceptDescription renders the entry's one-line description.
//
// It is the only provenance a catalog listing or a search hit shows — the body's
// heading never reaches either — so it has to make the same human-wrote-it /
// model-drafted-it distinction NoteBody's header does, otherwise the distinction
// survives only for a reader who opens the entry.
//
// It also has to carry SIGNAL, which it used to carry none of: a fixed sentence
// naming the author and the transport, byte-identical across every operator
// note, on the field with three consumers that all read it as content. It is
// half of what instant recall hands verify as the root-cause claim (see
// noteEntryTitle), it is what the reranker is shown for each candidate
// (investigate.renderRerankCandidates), and it is indexed for lexical search
// (catalog.Entry's search text).
//
// So it renders three things in this order:
//
//   - the provenance clause FIRST, because a listing that clips the line must
//     never clip the fact that RunLore, not the human, wrote the text;
//   - the finding the note annotates, as context — it is what keeps the note
//     reachable from the alert that produced it, and it must not be the note's
//     identity (see noteEntryTitle);
//   - the note's own claim, at greater length than the title had room for.
//
// Every part is bounded on its own rather than the whole line truncated at the
// end, so the provenance clause can never be the part that gets cut.
// claim is the note's own words, already bounded and defused by noteClaim.
func conceptDescription(tc Context, n Note, claim string) string {
	who := truncateWords(noteLine(n.Author), maxNoteAuthor)
	var b strings.Builder
	if n.modelDrafted() {
		fmt.Fprintf(&b, "Note drafted by RunLore from @%s's %s thread message, pending review", who, transportName(tc.Transport))
	} else {
		fmt.Fprintf(&b, "Operator knowledge from @%s via %s", who, transportName(tc.Transport))
	}
	if s := truncateWords(noteLine(tc.Title), maxNoteDescriptionContext); s != "" {
		fmt.Fprintf(&b, ", on the finding %q", s)
	}
	if claim != "" {
		fmt.Fprintf(&b, ": %s", claim)
	} else {
		b.WriteString(".")
	}
	return b.String()
}

// noteLine prepares one identity field — an author, an alert-derived title, the
// note text — for a single-line YAML title or description: noteField redacts it
// and maps line breaks (and the control/format runes that render as one) to
// spaces, then strings.Fields squeezes the runs they leave behind and trims the
// ends.
//
// One function rather than three composed at each call site, because no caller
// wants any part alone: a squeezed but unredacted field would put a secret in an
// entry's title, which outlives the pull request it arrived on (see
// ConceptEntry), and an undefused one is the hole below.
//
// The defusal is what makes this safe to put in FRONTMATTER, and it is the half
// noteField does not do. A title and a description are not read only out of the
// YAML: okf.UpdateIndex interpolates both into index.md as `- [%s](%s) — %s`,
// okf.UpdateLog interpolates the title into log.md, and github.prBody puts the
// description in the pull-request body — three markdown renderers, none of which
// defuses anything (prBody applies the image regex alone, and that copy still
// carries the raw-HTML gap its own comment admits). All three write a PERMANENT,
// merged repository file.
//
// Two concrete failures, both measured before this was added: a note reading
// `<img src="https://evil.example/px.gif">` put a live tracking pixel into
// index.md and log.md, firing from every reader's browser; and a note reading
// `boom](evil)` produced `- [Operator note: boom](evil) ## Incidents](path.md)`,
// so the index's own link FOR THAT ENTRY pointed at the attacker's URL.
//
// It runs BEFORE every caller's truncation, not after, because the defusals
// EXPAND — up to 3x for neutralizeImages, 2.5x for neutralizeHTML — and each
// caller then bounds the result to a budget it states. Defusing afterwards would
// make every one of those budgets a lie by the same factor: maxNoteAuthor and
// maxNoteDescriptionContext are applied here and never re-cut, and the
// finding-fallback title in noteEntryTitle is capped once at maxNoteTitle, which
// is what keeps it inside the merge gate's 120 bytes.
//
// The note's own claim is the one field that would survive the wrong order, and
// only by accident: noteClaim bounds it and noteEntryTitle re-cuts it, so the
// title comes out short either way while the DESCRIPTION carries the whole
// expansion. Ordering it here means no field depends on that accident.
//
// A cut landing inside an escape ("&lt;" cut to "&l") is cosmetic and cannot
// re-expose markup: neither defusal's output contains a "<" or a "![" for a
// later cut to uncover.
//
// escapeOKFSections is deliberately NOT applied. It defuses ATX headings, which
// are a BODY concern: a heading has to start a line, and this is a single line
// interpolated mid-line into every one of those sinks. Frontmatter is never
// parsed for OKF sections at all.
func noteLine(s string) string {
	return defuseMarkup(strings.Join(strings.Fields(noteField(s)), " "))
}

// defuseMarkup neutralises the two markup shapes an identity field can smuggle
// into a renderer: image syntax and raw HTML. It is the pair noteText applies to
// the note BODY, minus the heading escape — see noteLine for why.
func defuseMarkup(s string) string { return neutralizeImages(neutralizeHTML(s)) }

// leadingMarkupMarkers are the one-character Markdown markers a chat reply
// opens a line with. Only a marker followed by a SPACE counts, so "-1 replica"
// keeps its minus sign.
const leadingMarkupMarkers = ">-*+"

// stripLeadingMarkup removes the Markdown a chat reply routinely opens with — a
// bullet, a blockquote marker, an ATX heading — so a note's title reads as its
// claim rather than as "- the cause was …".
//
// Heading detection goes through atxHeadingText, the same parse the read path
// performs, rather than a second spelling of it.
//
// Bounded rather than looped to a fixed point: nesting deeper than a few levels
// is not something a chat reply does, and a bound is one less way for untrusted
// text to control how long this runs.
//
// So it strips up to four levels, not all of them: "- - - - - - - - the cause"
// keeps the markers the bound did not reach. That is the deliberate trade and
// not a case worth chasing — the cost is a slightly uglier title on input nobody
// writes by accident, while the alternative hands an unbounded loop to text
// anyone in the channel can send.
func stripLeadingMarkup(s string) string {
	s = strings.TrimSpace(s)
	for range 4 {
		if h := atxHeadingText(s); h != "" {
			s = h
			continue
		}
		if len(s) >= 2 && strings.IndexByte(leadingMarkupMarkers, s[0]) >= 0 && s[1] == ' ' {
			s = strings.TrimSpace(s[2:])
			continue
		}
		break
	}
	return s
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
// inside a code fence is not a heading to a Markdown renderer — nor, any longer,
// to either of RunLore's section parsers.
//
// It started that way because the two parsers disagreed. catalog.Entry.Section
// tracked fences; kbvalidate.Sections walked every line with no fence state at
// all, and HasIncidentSections is built on it. This function once mirrored the
// first parser exactly — which read as careful, and was the hole. A note that
// wrapped
//
//	## Symptom / ## Cause / ## Resolution
//
// in a ``` fence was escaped NOWHERE and parsed EVERYWHERE the fence-blind
// parser looks, so HasIncidentSections returned true for a body a stranger typed
// into a chat thread.
//
// That disagreement is now closed at the source: both parsers take their answer
// from catalog.FencedLines, so a fenced heading is a heading to neither. The
// UNION rule stays anyway, which makes the escaping here the SECOND of two
// independent defences rather than the only one. Keeping it costs a visible
// backslash inside a pasted code block. Dropping it would put a THIRD markdown
// parser on the write path — this function would have to track fences itself to
// know which headings to skip — and would stake the whole property on the read
// path never regressing. TestSectionsAndCatalogSectionAgreeOnHeadings pins the
// two parsers to each other; this keeps a note safe even if that pin is relaxed.
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

// SingleLine collapses every rune that can break or reorder a line — any
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
func SingleLine(s string) string {
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

// truncateWords shortens s to around n bytes like truncate, but cuts on a WORD
// boundary wherever one is available.
//
// truncate's rune-boundary cut is right for a machine-generated field nobody
// reads as a sentence, and wrong for anything a model then has to UNDERSTAND: it
// produced entry titles ending "… stu…", which verify rejected as
// unintelligible rather than as false — see noteEntryTitle for the whole path.
//
// The word boundary is only taken when it keeps at least half of what was cut
// to. A single unbroken token longer than the budget — a URL, a hash, one very
// long word — has no boundary to cut on, and backing off to an early space would
// throw away nearly everything the field had room for; there, the rune boundary
// is the honest cut. Trailing punctuation left dangling by the cut is dropped so
// the mark reads as a continuation, not as a comma followed by an ellipsis.
//
// Like truncate, the result can exceed n by the ellipsis' 2 extra bytes — but
// only for a positive n. A budget of zero or less leaves no room even for the
// mark, so it yields "" rather than a bare "…" that would break the bound this
// comment states; unlike truncate, which panics outright at n == 0.
//
// It assumes valid UTF-8, which every call site guarantees by piping through
// noteLine (strings.Map replaces each invalid byte with U+FFFD). Called directly
// on invalid UTF-8 it can emit invalid UTF-8: cutToRuneBoundary walks back over
// continuation bytes, and a lone continuation byte is not one.
func truncateWords(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	kept := cutToRuneBoundary(s, n-1)
	if i := strings.LastIndexByte(kept, ' '); i > 0 && 2*i >= len(kept) {
		kept = kept[:i]
	}
	return strings.TrimRight(kept, " ,;:—-") + "…"
}

// truncate shortens s to around n bytes: it cuts at or before byte index n-1
// (walking back to a rune boundary) and appends a 3-byte "…", so the result
// can be up to n+2 bytes in the worst case. Bytes, because that is what the
// validator's limit counts.
//
// A budget of n <= 0 yields "", matching cutToRuneBoundary's guard rather than
// diverging from it. Without it `cut := n - 1` is -1, the walk-back's `cut > 0`
// condition keeps it there, and s[:-1] panics on any non-empty s. No shipped
// caller passes a non-positive n today — they all pass a positive constant — so
// this is defence against a future one, not a live fix.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
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
