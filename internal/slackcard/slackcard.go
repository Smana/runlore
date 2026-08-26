// SPDX-License-Identifier: Apache-2.0

// Package slackcard holds the Block Kit vocabulary shared by the card's BUILDER
// (internal/notify) and its REWRITER (internal/server): the action ids a click
// arrives under, the block_id encoding that carries the TriggerKey, the two
// rendering helpers whose omission is invisible on screen, and the rewrite itself.
//
// It exists because those two packages sit on opposite sides of a layering rule —
// the HTTP layer knows nothing about renderers — and the rule was being paid for
// in duplication and in bugs. The action ids lived twice as bare literals; the
// block_id prefix lived twice behind a test that parsed the other package's
// source to compare them; and the silence marker shipped a server-timezone
// timestamp and an unescaped username because Date and EscapeMrkdwn were on the
// wrong side of the line and could not be reached.
//
// A leaf package with no RunLore dependencies keeps the rule intact — internal/server
// still does not import internal/notify — while letting one definition serve both
// sides, checked by the compiler instead of by a source-parsing guard.
package slackcard

import (
	"fmt"
	"maps"
	"strings"
	"time"
)

// Slack interaction action_ids. Both the renderer that stamps them onto a card
// and the handler that dispatches on them read these, so a rename cannot land on
// one side only.
const (
	ApproveActionID      = "runlore_approve"
	RejectActionID       = "runlore_reject"
	FeedbackUpActionID   = "runlore_feedback_up"
	FeedbackDownActionID = "runlore_feedback_down"
	SilenceActionID      = "runlore_silence"
)

// SilenceBlockIDPrefix namespaces the actions block whose block_id carries the
// TriggerKey for the silence control, and BlockIDMax is Slack's cap on that field.
//
// The key rides in block_id rather than in the control's option values because
// Slack caps an option value at 75 characters (a button value gets 2000), and a
// GitOps TriggerKey is `namespace/name:Reason` — routinely 60-70 characters, and
// unbounded in principle since Kubernetes names run to 253.
const (
	SilenceBlockIDPrefix = "sil:"
	BlockIDMax           = 255
)

// mrkdwnEscaper implements Slack's documented mrkdwn escaping: exactly three
// characters act as control characters and must be replaced with HTML entities
// (& first). strings.Replacer substitutes in a single left-to-right pass, so the
// ampersands introduced by &lt;/&gt; are never re-escaped.
var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// EscapeMrkdwn neutralises untrusted text (model output, evidence quoting cluster
// logs or alert annotations) before it is interpolated into Slack mrkdwn, so a
// hostile log line like <https://evil.example|innocent text> renders as literal
// text instead of a clickable phishing link.
func EscapeMrkdwn(s string) string { return mrkdwnEscaper.Replace(s) }

// Date renders t as a Slack date token that displays in the reader's local
// timezone, with the RFC3339 UTC form as the no-JS fallback. Blocks only: the
// token uses raw <>, so it must never enter the escaped fallback text.
func Date(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format(time.RFC3339))
}

// UserMention renders a Slack user id as a mention. This is the ONLY form Slack
// linkifies inside a Block Kit mrkdwn element: a leading "@name" renders there as
// literal text, because link_names is a chat.postMessage option with no effect on
// blocks. The id is never escaped — the angle brackets ARE the syntax — which is
// safe because it is a Slack-generated identifier arriving inside an
// HMAC-verified payload, not operator- or model-supplied text.
func UserMention(userID string) string { return "<@" + userID + ">" }

// ContextBlock builds the small-grey-type footer block a marker renders in.
func ContextBlock(text string) map[string]any {
	return map[string]any{
		"type":     "context",
		"elements": []any{map[string]any{"type": "mrkdwn", "text": text}},
	}
}

// Silenced rewrites a card after a silence: the control carrying actionID is
// removed and marker is appended as a context block. Other controls are KEPT — a
// 👎 re-arms a silenced trigger immediately, and the acknowledgement names that
// as the escape hatch, so stripping it would leave the card promising a way out
// it no longer offers.
//
// It reports false when it cannot produce a card it is sure of, and that matters
// more than the signature suggests: the caller posts the result with
// replace_original: true, which OVERWRITES the Slack message. A rebuild that
// guessed would replace the investigation with a lone marker line — the finding,
// its evidence and its next steps all gone, unrecoverably, in exchange for a note
// about them. Refusing costs a marker; guessing costs the finding.
//
// Three things make it refuse:
//
//   - Nothing was removed. The control is not on the card, so this click is
//     answering a card that was ALREADY rewritten — a second engineer clicking a
//     stale card, or a click that raced the first rewrite. Appending anyway would
//     stack a second marker under the first and report two different silence
//     windows for one finding.
//   - Nothing survived. Every block was dropped, so there is no card left to post.
//   - There were no blocks to begin with, which lands on the same check.
//
// Only blocks whose type is literally "actions" are rewritten. A context block
// also carries an "elements" array, so keying off the field alone would walk the
// footer as though it held controls.
func Silenced(blocks []map[string]any, actionID, marker string) ([]map[string]any, bool) {
	out := make([]map[string]any, 0, len(blocks)+1)
	removed := false
	for _, b := range blocks {
		elems, ok := b["elements"].([]any)
		if b["type"] != "actions" || !ok {
			out = append(out, b)
			continue
		}
		kept := make([]any, 0, len(elems))
		removedHere := false
		for _, e := range elems {
			if m, isMap := e.(map[string]any); isMap && m["action_id"] == actionID {
				removedHere = true
				continue
			}
			kept = append(kept, e)
		}
		removed = removed || removedHere
		// Slack rejects an actions block with an empty elements array, so a block
		// that held the silence control ALONE is dropped rather than emptied.
		if len(kept) == 0 {
			continue
		}
		nb := maps.Clone(b)
		nb["elements"] = kept
		// The block_id is stamped IFF the silence control sits on THIS block (it
		// carries the TriggerKey the control's click is routed by), so it leaves
		// with the control. Scoped to removedHere, not removed: a card with a
		// second actions block must keep its own block_id. The surviving 👍/👎
		// buttons carry the key in their own value field and do not read it.
		if removedHere {
			delete(nb, "block_id")
		}
		out = append(out, nb)
	}
	if !removed || len(out) == 0 {
		return nil, false
	}
	return append(out, ContextBlock(marker)), true
}
