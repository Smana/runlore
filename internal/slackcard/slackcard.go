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

// SilenceOutcome says WHY a rewrite did or did not happen. The two ways of
// refusing look identical to the caller as a bare false, but they are opposite
// facts about what the channel can now see, and telling the person who clicked
// the wrong one is worse than telling them nothing: one means the card carries no
// marker at all, the other means it carries someone else's.
type SilenceOutcome int

const (
	// SilenceNoUsableCard means there was nothing to rebuild from, or removing the
	// control left nothing that could be posted. The card carries no marker.
	//
	// Deliberately FIRST, so it is the zero value. An outcome nobody set should be
	// the one that discloses conservatively, not the one that suppresses every
	// disclosure — a `var o SilenceOutcome` that read as "rewritten" would silently
	// tell the person who clicked that their card is fine.
	SilenceNoUsableCard SilenceOutcome = iota
	// SilenceRewritten means the control was removed and the marker appended.
	SilenceRewritten
	// SilenceAlreadyRewritten means the control was not on the card, so this click is
	// answering a view that had already been rewritten — an earlier click, or one
	// that raced this one. The card is marked; it just does not say what THIS
	// click recorded, and the ledger's latest-wins fold means the two now differ.
	//
	// A card that never carried the control at all lands here too. That shape
	// cannot arise from a real click on a silence control, so the reading "already
	// rewritten" is the one worth optimising the message for.
	SilenceAlreadyRewritten
)

// String renders the outcome for logs and test failures. Without it a failed
// assertion reads "outcome = 1, want 2" on a type whose entire purpose is that
// its two refusals are NOT interchangeable.
func (o SilenceOutcome) String() string {
	switch o {
	case SilenceRewritten:
		return "rewritten"
	case SilenceAlreadyRewritten:
		return "already-rewritten"
	case SilenceNoUsableCard:
		return "no-usable-card"
	default:
		return fmt.Sprintf("unknown(%d)", int(o))
	}
}

// Silenced rewrites a card after a silence: the control carrying actionID is
// removed and marker is appended as a context block. Other controls are KEPT — a
// 👎 re-arms a silenced trigger immediately, and the acknowledgement names that
// as the escape hatch, so stripping it would leave the card promising a way out
// it no longer offers.
//
// It returns an outcome other than SilenceRewritten when it cannot produce a card
// it is sure of, and that matters more than the signature suggests: the caller
// posts the result with replace_original: true, which OVERWRITES the Slack
// message. A rebuild that guessed would replace the investigation with a lone
// marker line — the finding, its evidence and its next steps all gone,
// unrecoverably, in exchange for a note about them. Refusing costs a marker;
// guessing costs the finding.
//
// Two things make it refuse, and which one is reported decides what the person
// who clicked is told — see SilenceOutcome:
//
//   - Nothing survived. Every block was dropped, or there were none to begin with,
//     so there is no card left to post — SilenceNoUsableCard.
//   - Nothing was removed. The control is not on the card, so this click is
//     answering a view that was ALREADY rewritten — a second engineer on a stale
//     card, or a click that raced the first rewrite. Appending anyway would stack
//     a second marker under the first and show two windows for one finding —
//     SilenceAlreadyRewritten.
//
// Only blocks whose type is literally "actions" are rewritten. A context block
// also carries an "elements" array, so keying off the field alone would walk the
// footer as though it held controls.
func Silenced(blocks []map[string]any, actionID, marker string) ([]map[string]any, SilenceOutcome) {
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
	// Order matters: an empty result is "no card", even when the control was among
	// what got dropped. Stale is the other shape — a card that SURVIVED but had no
	// silence control to remove, because an earlier rewrite already took it.
	if len(out) == 0 {
		return nil, SilenceNoUsableCard
	}
	if !removed {
		return nil, SilenceAlreadyRewritten
	}
	return append(out, ContextBlock(marker)), SilenceRewritten
}
