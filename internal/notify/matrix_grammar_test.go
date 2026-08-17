// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// TestMatrixMentionGrammar pins Fix 1: thread.Parse strips only Slack's
// "<@U…>" mention encoding. Real Matrix clients never send that shape — the
// body carries a bare MXID, a localpart, or a display name instead — so
// without stripping the mention Matrix-side FIRST, the mention token sits in
// front of "note:"/"reinvestigate:" and every addressed message falls
// through to IntentFreeform with the mention still attached.
//
// Deliberately NOT in the "<@runlore:hs> note: …" fixture style the rest of
// this package's tests use for handleMessage routing: that shape is exactly
// what real Matrix clients never produce, which is why the defect this test
// pins was invisible before.
func TestMatrixMentionGrammar(t *testing.T) {
	const self = "@runlore:hs"
	tests := []struct {
		name       string
		body       string
		wantIntent thread.Intent
		wantText   string
	}{
		// bare MXID
		{"bare MXID + note", "@runlore:hs note: it was a bad deploy", thread.IntentNote, "it was a bad deploy"},
		{"bare MXID + reinvestigate", "@runlore:hs reinvestigate: go look again", thread.IntentReinvestigate, "go look again"},
		{"bare MXID + neither", "@runlore:hs is anyone around?", thread.IntentFreeform, "is anyone around?"},
		// localpart only
		{"localpart + note", "runlore note: it was a bad deploy", thread.IntentNote, "it was a bad deploy"},
		{"localpart + reinvestigate", "runlore reinvestigate: go look again", thread.IntentReinvestigate, "go look again"},
		{"localpart + neither", "runlore is anyone around?", thread.IntentFreeform, "is anyone around?"},
		// display name that happens to match the localpart case-insensitively
		// (Matrix's own suggested display name IS the localpart, so this is the
		// common case, not a corner case)
		{"display name + note", "RunLore: note: it was a bad deploy", thread.IntentNote, "it was a bad deploy"},
		{"display name + reinvestigate", "RunLore: reinvestigate: go look again", thread.IntentReinvestigate, "go look again"},
		{"display name + neither", "RunLore: is anyone around?", thread.IntentFreeform, "is anyone around?"},
		// leading comma variant: TrimLeft(rest, ":,") applies to this shape too.
		{"bare MXID + comma + note", "@runlore:hs, note: it was a bad deploy", thread.IntentNote, "it was a bad deploy"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, _ := stripSelfMention(tc.body, self, "")
			got := thread.Parse(text)
			if got.Intent != tc.wantIntent {
				t.Errorf("Intent = %v, want %v (body %q)", got.Intent, tc.wantIntent, tc.body)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text = %q, want %q (body %q)", got.Text, tc.wantText, tc.body)
			}
		})
	}
}

// TestStripSelfMentionWholeTokenBoundary pins the isAlphanumericByte guard
// inside stripSelfMention itself: "runlored" must not have its leading
// "runlore" stripped as if it were the bare localpart followed by
// punctuation — the same whole-token rule containsWord/addressed apply,
// exercised here at the stripping site directly rather than only indirectly
// through a Parse-level assertion.
func TestStripSelfMentionWholeTokenBoundary(t *testing.T) {
	const body = "runlored note: it was a bad deploy"
	got, stripped := stripSelfMention(body, "@runlore:hs", "")
	if stripped {
		t.Fatalf("stripped = true, want false — %q must not be treated as the localpart", "runlore")
	}
	if got != body {
		t.Fatalf("body = %q, want it returned unchanged", got)
	}
}

// TestStripSelfMentionRuneBoundary is stripSelfMention's companion to
// TestContainsWordRuneBoundary: the same pre-fix bug — a boundary check that
// read one BYTE immediately after a matched candidate and asked whether it
// was ASCII-alphanumeric — lived here too, on the post-match side only
// (stripSelfMention matches a LEADING token, so there is no "before the
// match" side to check). A UTF-8 continuation byte from a non-ASCII letter
// or digit right after "runlore" was never ASCII-alphanumeric, so
// "runloré note: x" was wrongly treated as the bare localpart "runlore"
// followed by a boundary, stripped down to "é note: x", and handed to
// thread.Parse as if the message genuinely addressed RunLore. Includes a
// non-ASCII DIGIT case distinct from the letter cases for the same reason as
// TestContainsWordRuneBoundary: IsDigit and IsLetter are separate
// predicates.
func TestStripSelfMentionRuneBoundary(t *testing.T) {
	const self = "@runlore:hs"
	tests := []struct {
		name         string
		body         string
		wantStripped bool
		wantText     string
	}{
		{
			name:         "non-ASCII letter immediately after the candidate is not a boundary (the bug)",
			body:         "runloreé note: x", // "runlore" immediately followed by 'é', not "runlore" with its 'e' replaced
			wantStripped: false,
			wantText:     "runloreé note: x",
		},
		{
			name:         "non-ASCII digit immediately after the candidate is not a boundary",
			body:         "runlore３ note: x", // U+FF13 FULLWIDTH DIGIT THREE: IsDigit true, IsLetter false
			wantStripped: false,
			wantText:     "runlore３ note: x",
		},
		{
			name:         "punctuation neighbour is still a boundary — must still strip",
			body:         "runlore, note: x",
			wantStripped: true,
			wantText:     "note: x",
		},
		{
			name:         "existing ASCII case is unaffected — must still not strip",
			body:         "runlored note: x",
			wantStripped: false,
			wantText:     "runlored note: x",
		},
		{
			name:         "candidate is the entire body",
			body:         "runlore",
			wantStripped: true,
			wantText:     "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stripped := stripSelfMention(tc.body, self, "")
			if stripped != tc.wantStripped {
				t.Errorf("stripped = %v, want %v (body %q)", stripped, tc.wantStripped, tc.body)
			}
			if got != tc.wantText {
				t.Errorf("text = %q, want %q (body %q)", got, tc.wantText, tc.body)
			}
		})
	}
}

// TestStripSelfMentionCombiningMarkBoundary is stripSelfMention's companion
// to TestContainsWordCombiningMarkBoundary: TestStripSelfMentionRuneBoundary
// already pins NFC (precomposed) "é" as a non-boundary, but NFC alone
// does not cover NFD (decomposed) "é" — 'e' (U+0065, already the
// candidate's own trailing rune) followed by U+0301 COMBINING ACUTE ACCENT as
// a separate rune. Matching the bare-localpart candidate "runlore" against
// NFD "runloré" stops right after the plain 'e', and the next rune is the
// combining mark alone — neither a letter nor a digit — so the
// pre-fix predicate misread it as a boundary and stripped "runlore" off the
// front of the accented word "runloré", handing thread.Parse "́ note: x"
// (a lone combining mark plus the rest) as if the message genuinely
// addressed RunLore. Matrix clients send NFD in practice (macOS input
// methods default to it), so this is not exotic.
func TestStripSelfMentionCombiningMarkBoundary(t *testing.T) {
	const self = "@runlore:hs"
	tests := []struct {
		name         string
		body         string
		wantStripped bool
		wantText     string
	}{
		{
			name:         "NFD combining acute accent immediately after the candidate is not a boundary",
			body:         "runloré note: x", // 'e' (candidate's own trailing rune) + U+0301 spells NFD "runloré", not "runlore" + boundary
			wantStripped: false,
			wantText:     "runloré note: x",
		},
		{
			name:         "punctuation neighbour is still a boundary — must still strip (regression guard)",
			body:         "runlore, note: x",
			wantStripped: true,
			wantText:     "note: x",
		},
		{
			name:         "candidate is the entire body (regression guard)",
			body:         "runlore",
			wantStripped: true,
			wantText:     "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, stripped := stripSelfMention(tc.body, self, "")
			if stripped != tc.wantStripped {
				t.Errorf("stripped = %v, want %v (body %q)", stripped, tc.wantStripped, tc.body)
			}
			if got != tc.wantText {
				t.Errorf("text = %q, want %q (body %q)", got, tc.wantText, tc.body)
			}
		})
	}
}

// TestMatrixMentionGrammarUnrelatedDisplayNameNotStripped pins
// stripSelfMention's own behaviour in isolation: a display name unrelated to
// the localpart (e.g. an operator-configured "Ops Bot") is not stripped
// unless that exact name was actually resolved and passed in — "" here
// simulates a profile lookup that never ran or never learned it.
//
// This is NOT, by itself, a safe degradation for an ordinary freeform
// question — a nuisance at worst, since thread.Parse falls through to
// IntentFreeform either way. For the RESERVED "reinvestigate:" prefix it
// would have been the actual bug this file's
// TestMatrixHandleMessageUnrelatedDisplayNameReinvestigateBackstop pins,
// below, if thread.Parse itself did not ALSO match a reserved prefix
// anywhere in the text (not only at position 0) — see thread.Parse's doc.
// Because it does, the unstripped mention token here changes nothing about
// whether "reinvestigate:" is still recognised downstream.
func TestMatrixMentionGrammarUnrelatedDisplayNameNotStripped(t *testing.T) {
	text, stripped := stripSelfMention("Ops Bot: note: it was a bad deploy", "@runlore:hs", "")
	if stripped {
		t.Fatal("stripped = true, want false — an unrelated, unresolved display name must not match")
	}
	got := thread.Parse(text)
	if got.Intent != thread.IntentFreeform {
		t.Fatalf("Intent = %v, want %v — an unrelated display name is not stripped", got.Intent, thread.IntentFreeform)
	}
}

// fakeReinvestigateForge is thread.Forge, recording whether either write
// entry point was ever called. It is used to prove that
// IntentReinvestigate — reached only once the mention is stripped
// Matrix-side — never writes to the knowledge base, no matter which route
// write() would otherwise have taken.
type fakeReinvestigateForge struct {
	mu        sync.Mutex
	opened    int
	commented int
}

func (f *fakeReinvestigateForge) OpenPR(context.Context, providers.KBEntry) (providers.Ref, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return providers.Ref{URL: "https://forge.example/pull/1"}, nil
}

func (f *fakeReinvestigateForge) CommentOnPR(context.Context, int, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commented++
	return nil
}

func (f *fakeReinvestigateForge) IsPROpen(context.Context, int) (bool, error) { return true, nil }

func (f *fakeReinvestigateForge) counts() (opened, commented int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened, f.commented
}

// TestMatrixHandleMessageReinvestigateIsReservedNotFreeform is the end-to-end
// regression test for Fix 1's severity-1 consequence: before the mention was
// stripped Matrix-side, "@runlore:hs reinvestigate: go look again" parsed as
// IntentFreeform (the mention token still attached, so "reinvestigate:" never
// matched as a prefix) and IntentFreeform is treated exactly like an explicit
// note — opening a junk knowledge-base PR containing the sentence. Driven
// through the REAL handleMessage → Dispatch → thread.Mention.HandleMention →
// thread.Responder.Handle pipeline (a real Registry entry, a real Responder,
// a fake Forge), not a hand-built thread.Parse call, so a regression anywhere
// in that chain — not just in the grammar — would also be caught here.
func TestMatrixHandleMessageReinvestigateIsReservedNotFreeform(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Put(thread.Context{Root: "$root-ours", Transport: "matrix", Channel: room, TriggerKey: "trig-ours"}); err != nil {
		t.Fatalf("registry Put: %v", err)
	}
	forge := &fakeReinvestigateForge{}
	responder := &thread.Responder{Forge: forge, Registry: reg, Log: matrixTestLog()}
	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	mention := &thread.Mention{Responder: responder, Registry: reg, Replier: rep, Log: matrixTestLog()}

	dispatch := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	busy := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog(), WithThreadCapture(mention, dispatch, busy))
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply-reinvestigate"}
	e.Content.Body = "@runlore:hs reinvestigate: go look again"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	f.handleMessage(context.Background(), e)
	waitForReplies(t, rep)

	if got := rep.snapshot(); len(got) != 1 || got[0].text != thread.ReinvestigateNotSupportedReply {
		t.Fatalf("replies = %+v, want exactly one reply %q", got, thread.ReinvestigateNotSupportedReply)
	}
	if opened, commented := forge.counts(); opened != 0 || commented != 0 {
		t.Fatalf("forge: opened=%d commented=%d, want 0/0 — the reserved reinvestigate: prefix must never write to the knowledge base", opened, commented)
	}
}

// newBackstopFixture builds the shared scaffolding the three tests below
// need: a registry with one root of RunLore's own, a fake forge, a real
// Responder/Mention wired through it, and a fresh MatrixFeedback with
// WithThreadCapture — identical in shape to
// TestMatrixHandleMessageReinvestigateIsReservedNotFreeform's setup, factored
// out because all three share it verbatim. Named for the transport-side
// backstop these tests originally pinned; that backstop has since been
// removed as redundant (see handleMessage's doc comment) once thread.Parse
// itself started matching a reserved prefix anywhere in the text — the
// fixture and the tests built on it stayed, since the scenarios they cover
// (an unstripped mention token hiding a reserved command, or a genuine note)
// are still exactly what needs pinning, just via the ordinary Dispatch path
// now instead of a dedicated one.
func newBackstopFixture(t *testing.T) (*MatrixFeedback, *fakeReinvestigateForge, *fakeMentionReplier) {
	t.Helper()
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	t.Cleanup(srv.Close)

	reg, err := thread.NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := reg.Put(thread.Context{Root: "$root-ours", Transport: "matrix", Channel: room, TriggerKey: "trig-ours"}); err != nil {
		t.Fatalf("registry Put: %v", err)
	}
	forge := &fakeReinvestigateForge{}
	responder := &thread.Responder{Forge: forge, Registry: reg, Log: matrixTestLog()}
	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	mention := &thread.Mention{Responder: responder, Registry: reg, Replier: rep, Log: matrixTestLog()}

	dispatch := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	busy := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog(), WithThreadCapture(mention, dispatch, busy))
	f.self = self
	return f, forge, rep
}

// backstopEvent builds one m.thread reply from @alice:hs rooted at
// $root-ours, addressed via MSC3952 m.mentions, carrying body — the shape
// every test below sends through handleMessage.
func backstopEvent(body string) matrixEvent {
	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply-backstop"}
	e.Content.Body = body
	e.Content.Mentions.UserIDs = []string{"@runlore:hs"}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"
	return e
}

// TestMatrixHandleMessageUnrelatedDisplayNameReinvestigateBackstop pins the
// residual gap left by stripSelfMention alone: a Matrix bot account whose
// profile display name differs from its localpart (an ordinary operator
// choice, e.g. "Ops Bot", not a corner case) strips nothing, so
// "Ops Bot: reinvestigate: go look again" still carries "reinvestigate:"
// behind an unrecognised mention token by the time it reaches handleMessage.
// addressed() says yes (m.mentions), stripSelfMention leaves the mention
// attached, and the text is handed to thread.Parse via the ordinary
// Dispatch → HandleMention → Responder.Handle pipeline exactly like any
// other addressed message — no special-cased transport-side backstop
// involved (there used to be one; see handleMessage's doc comment for why it
// was removed as redundant). thread.Parse's own reserved-anywhere match (not
// only at position 0) is what refuses this, proven here end-to-end with a
// fake forge so this pins BEFORE any forge call is possible — not merely
// that some parse function returns the right enum.
func TestMatrixHandleMessageUnrelatedDisplayNameReinvestigateBackstop(t *testing.T) {
	f, forge, rep := newBackstopFixture(t)
	// No selfDisplayName resolved — the profile lookup never ran, or ran and
	// failed, or the homeserver has none set: exactly the state Fix 1's
	// lookup leaves a fresh or unlucky listener in.

	f.handleMessage(context.Background(), backstopEvent("Ops Bot: reinvestigate: go look again"))
	waitForReplies(t, rep)

	if got := rep.snapshot(); len(got) != 1 || got[0].text != thread.ReinvestigateNotSupportedReply {
		t.Fatalf("replies = %+v, want exactly one reply %q", got, thread.ReinvestigateNotSupportedReply)
	}
	if opened, commented := forge.counts(); opened != 0 || commented != 0 {
		t.Fatalf("forge: opened=%d commented=%d, want 0/0 — an unrelated display name must not open the reinvestigate: request as a knowledge-base PR", opened, commented)
	}
}

// TestMatrixHandleMessageUnrelatedDisplayNameResolvedViaProfileLookup is the
// companion proving the display-name-stripping path itself: once the display
// name IS known (a successful profile lookup at Run start, simulated here by
// setting selfDisplayName directly), stripSelfMention removes "Ops Bot:" like
// any other mention form, thread.Parse sees "reinvestigate:" at position 0
// the ordinary way, and Responder.Handle's own reserved-prefix case answers —
// the same reply as the unstripped case above, since thread.Parse refuses a
// reserved prefix identically whether or not a mention token precedes it.
func TestMatrixHandleMessageUnrelatedDisplayNameResolvedViaProfileLookup(t *testing.T) {
	f, forge, rep := newBackstopFixture(t)
	f.selfDisplayName = "Ops Bot"

	f.handleMessage(context.Background(), backstopEvent("Ops Bot: reinvestigate: go look again"))
	waitForReplies(t, rep)

	if got := rep.snapshot(); len(got) != 1 || got[0].text != thread.ReinvestigateNotSupportedReply {
		t.Fatalf("replies = %+v, want exactly one reply %q", got, thread.ReinvestigateNotSupportedReply)
	}
	if opened, commented := forge.counts(); opened != 0 || commented != 0 {
		t.Fatalf("forge: opened=%d commented=%d, want 0/0 — resolving the display name must route through Responder.Handle's reserved case, not write anything", opened, commented)
	}
}

// TestMatrixHandleMessageDisplayNameStrippedGenuineNoteStillRecorded proves
// thread.Parse's reserved-anywhere match is narrow: prose that merely uses
// the word "reinvestigate" mid-sentence — no trailing ':' — must still be
// recorded as a genuine note, once the message is addressed in a form
// stripSelfMention actually recognises (the resolved display name, mirroring
// TestMatrixHandleMessageUnrelatedDisplayNameResolvedViaProfileLookup above).
//
// This test used to run with the display name left UNSTRIPPED ("Ops Bot:
// note: …" with no selfDisplayName set) and still pass — but only because
// the write happened via IntentFreeform's old behaviour of writing exactly
// like IntentNote (the security-audit bug FreeformNotRecordedReply's doc
// comment describes; see internal/thread/responder.go). Now that Handle
// never writes for IntentFreeform, an unrecognised leading token yields
// IntentFreeform unconditionally — which never reaches a forge call no
// matter what reservedTokenIndex decides — so that scenario can no longer
// prove anything about reservedTokenIndex's narrowness specifically: it would
// pass (opened == 0) for the wrong reason. Stripping the mention first is
// what lets "note:" actually reach position 0 in thread.Parse and this test
// exercise the intended thing again: a genuine note containing the bare word
// "reinvestigate" is recorded, not refused.
func TestMatrixHandleMessageDisplayNameStrippedGenuineNoteStillRecorded(t *testing.T) {
	f, forge, rep := newBackstopFixture(t)
	f.selfDisplayName = "Ops Bot"

	f.handleMessage(context.Background(), backstopEvent("Ops Bot: note: we should reinvestigate the network config next time"))
	waitForReplies(t, rep)

	got := rep.snapshot()
	if len(got) != 1 {
		t.Fatalf("replies = %+v, want exactly one", got)
	}
	if got[0].text == thread.ReinvestigateNotSupportedReply {
		t.Fatalf("reply = %q, want the note recorded, not refused — the word \"reinvestigate\" appears without a trailing ':'", got[0].text)
	}
	if opened, _ := forge.counts(); opened != 1 {
		t.Fatalf("forge.opened = %d, want 1 — a genuine note must still be recorded even though the mention token could not be stripped", opened)
	}
}
