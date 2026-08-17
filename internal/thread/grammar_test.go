// SPDX-License-Identifier: Apache-2.0

package thread

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantIntent Intent
		wantText   string
	}{
		{"note prefix", "<@U0BOT> note: the real cause was a spot reclaim", IntentNote, "the real cause was a spot reclaim"},
		{"note prefix uppercase", "<@U0BOT> NOTE: spot reclaim", IntentNote, "spot reclaim"},
		{"note prefix no space after colon", "<@U0BOT> note:spot reclaim", IntentNote, "spot reclaim"},
		{"mention with display name", "<@U0BOT|runlore> note: x", IntentNote, "x"},
		{"no mention at all", "note: x", IntentNote, "x"},
		{"multiple leading mentions", "<@U0BOT> <@U0HUMAN> note: x", IntentNote, "x"},
		{"freeform", "<@U0BOT> did you check the NetworkPolicies?", IntentFreeform, "did you check the NetworkPolicies?"},
		{"reinvestigate reserved", "<@U0BOT> reinvestigate: look at the CNI", IntentReinvestigate, "look at the CNI"},
		{"reinvestigate bare", "<@U0BOT> reinvestigate:", IntentReinvestigate, ""},
		{"reinvestigate behind a filler word", "<@U0BOT> please reinvestigate: the network issue", IntentReinvestigate, "the network issue"},
		{"reinvestigate behind a different filler", "<@U0BOT> can you reinvestigate: this", IntentReinvestigate, "this"},
		{"reinvestigate word without a colon is not reserved", "<@U0BOT> we had to reinvestigate the DNS path and it was stale", IntentFreeform, "we had to reinvestigate the DNS path and it was stale"},
		{"note prefix ahead of a reserved word later is still refused", "<@U0BOT> note: we agreed to reinvestigate: the DNS path next sprint", IntentReinvestigate, "the DNS path next sprint"},
		// Ⱥ (U+023A) and Ⱦ (U+023E) are the only two Unicode code points whose
		// strings.ToLower is a LONGER byte sequence than the original — a byte
		// offset found by scanning a separately lower-cased copy of the text
		// would land past the true position when sliced back against the
		// original, up to and including out of range. The matcher must compare
		// against the original text directly rather than a pre-lowered copy, so
		// this must resolve cleanly rather than panic or mis-slice the returned
		// Text.
		{"byte-length-changing lowercase rune ahead of the reserved prefix", "<@U0BOT> Ⱥ reinvestigate: go look", IntentReinvestigate, "go look"},
		{"empty after mention", "<@U0BOT>", IntentFreeform, ""},
		{"whitespace only", "<@U0BOT>    ", IntentFreeform, ""},
		{"newlines preserved inside the note", "<@U0BOT> note: line one\nline two", IntentNote, "line one\nline two"},
		{"colon in freeform is not a prefix", "<@U0BOT> why: did it fail", IntentFreeform, "why: did it fail"},
		// "note:" is recognised anywhere, on the same terms as "reinvestigate:".
		// Every one of these used to fall through to IntentFreeform, which with
		// model.chat configured means the deterministic, free path silently
		// became a paid model call.
		{"note behind a filler word", "<@U0BOT> please note: the cause was a spot reclaim", IntentNote, "the cause was a spot reclaim"},
		{"note behind text before the mention", "hey <@U0BOT> note: the cause was a spot reclaim", IntentNote, "the cause was a spot reclaim"},
		{"note behind an emoji before the mention", ":wave: <@U0BOT> note: the cause was a spot reclaim", IntentNote, "the cause was a spot reclaim"},
		{"note behind a bare bot name, as Matrix delivers it", "hey runlore note: the cause was X", IntentNote, "the cause was X"},
		{"note behind a filler word uppercase", "<@U0BOT> please NOTE: x", IntentNote, "x"},
		// The false-positive guard is unchanged: the byte before the token must
		// not be alphanumeric, so a word ENDING in "note:" is not a command.
		{"footnote is not the note command", "<@U0BOT> see footnote: at the bottom", IntentFreeform, "see footnote: at the bottom"},
		{"keynote is not the note command", "<@U0BOT> the keynote: was good", IntentFreeform, "the keynote: was good"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.raw)
			if got.Intent != tc.wantIntent {
				t.Errorf("Intent = %v, want %v", got.Intent, tc.wantIntent)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tc.wantText)
			}
		})
	}
}

// TestParseReportsWhetherTheCommandWasAnchored pins Parsed.Anchored, the one
// fact Parse reports so a caller can apply a policy Parse itself must not
// hold: with no chat layer configured, Responder.Handle treats an unanchored
// "note:" as freeform rather than as a write (see Parse and
// TestHandleUnanchoredNoteWithoutChatIsFreeform).
//
// Anchored is measured AFTER leading mentions are stripped, so addressing the
// bot the ordinary way still counts as the start of the message, while a word
// ahead of the mention does not.
func TestParseReportsWhetherTheCommandWasAnchored(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		wantIntent   Intent
		wantAnchored bool
	}{
		{"note with no mention", "note: x", IntentNote, true},
		{"note behind one mention", "<@U0BOT> note: x", IntentNote, true},
		{"note behind several mentions", "<@U0BOT> <@U0HUMAN> note: x", IntentNote, true},
		{"note behind a mention with a display name", "<@U0BOT|runlore> note: x", IntentNote, true},
		{"note behind a filler word", "<@U0BOT> please note: x", IntentNote, false},
		{"note behind text before the mention", "hey <@U0BOT> note: x", IntentNote, false},
		{"note behind an emoji before the mention", ":wave: <@U0BOT> note: x", IntentNote, false},
		{"note behind a bare bot name", "hey runlore note: x", IntentNote, false},
		{"note mid-sentence", "<@U0BOT> the runbook note: link is stale", IntentNote, false},
		{"anchored reinvestigate", "<@U0BOT> reinvestigate: the CNI", IntentReinvestigate, true},
		{"unanchored reinvestigate", "<@U0BOT> please reinvestigate: the CNI", IntentReinvestigate, false},
		// No token produced these, so there is nothing for Anchored to describe.
		{"freeform", "<@U0BOT> did you check the NetworkPolicies?", IntentFreeform, false},
		{"bare mention", "<@U0BOT>", IntentFreeform, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.raw)
			if got.Intent != tc.wantIntent {
				t.Fatalf("Intent = %v, want %v", got.Intent, tc.wantIntent)
			}
			if got.Anchored != tc.wantAnchored {
				t.Errorf("Anchored = %v, want %v", got.Anchored, tc.wantAnchored)
			}
		})
	}
}
