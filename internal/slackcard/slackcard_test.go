// SPDX-License-Identifier: Apache-2.0

package slackcard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func actionsBlock(blockID string, actionIDs ...string) map[string]any {
	els := make([]any, 0, len(actionIDs))
	for _, id := range actionIDs {
		els = append(els, map[string]any{"type": "button", "action_id": id})
	}
	b := map[string]any{"type": "actions", "elements": els}
	if blockID != "" {
		b["block_id"] = blockID
	}
	return b
}

func section(text string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

// TestSilencedRefusesRatherThanBlanksTheCard pins the hard invariant: the result
// is posted with replace_original, which OVERWRITES the message, so every input
// the rewrite cannot account for must refuse rather than guess.
func TestSilencedRefusesRatherThanBlanksTheCard(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks []map[string]any
		want   SilenceOutcome
	}{
		{"no blocks in the payload at all", nil, SilenceNoUsableCard},
		{"an empty block list", []map[string]any{}, SilenceNoUsableCard},
		{
			"a lone actions block, so removing the control leaves nothing to post",
			[]map[string]any{actionsBlock("sil:k", SilenceActionID)},
			SilenceNoUsableCard,
		},
		{
			// The double-marker case: the control is already gone, so this click is
			// answering a card that was ALREADY rewritten.
			"a card whose silence control has already been removed",
			[]map[string]any{section("*Why:* something broke"), actionsBlock("", FeedbackUpActionID)},
			SilenceAlreadyRewritten,
		},
		{
			"a card that never carried a silence control",
			[]map[string]any{section("*Why:* something broke")},
			SilenceAlreadyRewritten,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, outcome := Silenced(tc.blocks, SilenceActionID, "marker")
			if outcome == SilenceRewritten {
				t.Fatalf("rebuild claimed success on %s: %v", tc.name, got)
			}
			// The two refusals are opposite facts about what the channel can see, so
			// the caller must be able to tell them apart to say the right thing.
			if outcome != tc.want {
				t.Errorf("outcome = %v, want %v — the caller cannot tell the clicker the truth about their card", outcome, tc.want)
			}
		})
	}
}

// TestSilencedDoesNotStackASecondMarker is the consequence the refusal above
// exists to prevent, stated as the scenario rather than the input: two engineers
// with the same card open, the second click arriving after the first rewrite
// landed. Appending anyway would report two different windows for one finding.
func TestSilencedDoesNotStackASecondMarker(t *testing.T) {
	first, outcome := Silenced([]map[string]any{
		section("*Why:* something broke"),
		actionsBlock("sil:k", FeedbackUpActionID, SilenceActionID),
	}, SilenceActionID, "🔕 Silenced by <@U1> until tomorrow · 4h")
	if outcome != SilenceRewritten {
		t.Fatalf("the first rewrite refused (%v), so there is nothing to race", outcome)
	}
	if _, outcome := Silenced(first, SilenceActionID, "🔕 Silenced by <@U2> until later · 24h"); outcome != SilenceAlreadyRewritten {
		t.Errorf("second click outcome = %v, want SilenceAlreadyRewritten — a stacked marker shows two windows for one finding", outcome)
	}
}

// TestSilencedDropsTheBlockIDWithTheControl pins the iff-rule the renderer holds:
// block_id is stamped only on the block carrying the silence control, because it
// carries the TriggerKey that control's click is routed by. It must leave with it.
func TestSilencedDropsTheBlockIDWithTheControl(t *testing.T) {
	out, outcome := Silenced([]map[string]any{
		section("*Why:* something broke"),
		actionsBlock("sil:ns/app:CrashLoop", FeedbackUpActionID, SilenceActionID),
	}, SilenceActionID, "marker")
	if outcome != SilenceRewritten {
		t.Fatalf("the rewrite refused (%v) a card it should have rebuilt", outcome)
	}
	dump, _ := json.Marshal(out)
	if strings.Contains(string(dump), SilenceBlockIDPrefix) {
		t.Errorf("the block_id outlived the control it belonged to: %s", dump)
	}
	if !strings.Contains(string(dump), FeedbackUpActionID) {
		t.Errorf("👍 was dropped — a 👎 is how a colleague lifts the silence early: %s", dump)
	}
	if !strings.Contains(string(dump), "something broke") {
		t.Errorf("the rewrite dropped the finding it was marking: %s", dump)
	}
}

// TestSilencedKeepsASecondActionsBlocksOwnID guards the scope of the rule above:
// only the block the control was ON loses its id.
func TestSilencedKeepsASecondActionsBlocksOwnID(t *testing.T) {
	out, outcome := Silenced([]map[string]any{
		actionsBlock("sil:k", SilenceActionID, FeedbackUpActionID),
		actionsBlock("other:block", FeedbackDownActionID),
	}, SilenceActionID, "marker")
	if outcome != SilenceRewritten {
		t.Fatalf("the rewrite refused (%v) a card it should have rebuilt", outcome)
	}
	dump, _ := json.Marshal(out)
	if !strings.Contains(string(dump), "other:block") {
		t.Errorf("an unrelated actions block lost its own block_id: %s", dump)
	}
}

// TestSilencedDoesNotMutateItsInput matters because the caller posts the ORIGINAL
// card when the rewrite refuses; a walk that edited blocks in place would corrupt
// the fallback it is falling back to.
func TestSilencedDoesNotMutateItsInput(t *testing.T) {
	in := []map[string]any{
		section("*Why:* something broke"),
		actionsBlock("sil:k", FeedbackUpActionID, SilenceActionID),
	}
	before, _ := json.Marshal(in)
	if _, outcome := Silenced(in, SilenceActionID, "marker"); outcome != SilenceRewritten {
		t.Fatalf("the rewrite refused (%v) a card it should have rebuilt", outcome)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Errorf("Silenced mutated the caller's blocks:\n before %s\n after  %s", before, after)
	}
}

func TestUserMentionIsTheFormSlackLinkifies(t *testing.T) {
	if got := UserMention("U9"); got != "<@U9>" {
		t.Errorf("UserMention = %q, want <@U9> — the only form linkified inside a mrkdwn element", got)
	}
}

func TestDateIsZeroSafe(t *testing.T) {
	if got := Date(time.Time{}); got != "" {
		t.Errorf("Date(zero) = %q, want empty rather than a 1970 token", got)
	}
	got := Date(time.Date(2026, 8, 26, 14, 40, 0, 0, time.UTC))
	if !strings.HasPrefix(got, "<!date^") || !strings.Contains(got, "2026-08-26T14:40:00Z") {
		t.Errorf("Date = %q, want a date token carrying the RFC3339 fallback", got)
	}
}

func TestEscapeMrkdwnDoesNotDoubleEscapeAmpersands(t *testing.T) {
	if got := EscapeMrkdwn("<https://evil.example|click> & co"); got != "&lt;https://evil.example|click&gt; &amp; co" {
		t.Errorf("EscapeMrkdwn = %q", got)
	}
}
