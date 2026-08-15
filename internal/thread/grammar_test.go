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
