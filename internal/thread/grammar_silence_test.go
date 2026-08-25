// SPDX-License-Identifier: Apache-2.0

package thread

import "testing"

func TestParseSilence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		wantIntent Intent
		wantText   string
	}{
		{"anchored", "silence: 4h", IntentSilence, "4h"},
		{"with a leading mention", "@runlore silence: 24h", IntentSilence, "24h"},
		{"no duration", "silence:", IntentSilence, ""},
		{"unanchored still matches", "please silence: 1h", IntentSilence, "1h"},
		{"case-insensitive", "Silence: 4h", IntentSilence, "4h"},
		{
			// The priority rule: a note that also contains the command is refused
			// as ambiguous rather than filed, exactly as for reinvestigate:.
			name:       "a note containing the command loses to it",
			raw:        "note: we agreed to silence: this until Thursday",
			wantIntent: IntentSilence,
			wantText:   "this until Thursday",
		},
		{
			// Whole-token matching: a longer word ending in the prefix is prose.
			name:       "presilence: is not the command",
			raw:        "presilence: 4h",
			wantIntent: IntentFreeform,
			wantText:   "presilence: 4h",
		},
		{"plain prose is untouched", "can we silence this alert", IntentFreeform, "can we silence this alert"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Parse(tc.raw)
			if p.Intent != tc.wantIntent {
				t.Errorf("Intent = %v, want %v", p.Intent, tc.wantIntent)
			}
			if p.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", p.Text, tc.wantText)
			}
		})
	}
}

func TestSilenceIntentString(t *testing.T) {
	if got, want := IntentSilence.String(), "silence"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
