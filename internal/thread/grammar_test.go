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
