// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"cmp"
	"encoding/json"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// threadContentField is the custom event-content field Deliver stamps on
// investigation messages so a threaded reply can be attributed to the
// investigation it answers — the same mechanism triggerKeyContentField already
// uses for 👍/👎, widened to carry the whole thread context.
//
// It is ADDITIVE: triggerKeyContentField keeps its exact meaning and value, so
// the reaction listener and every event sent before this field existed behave
// unchanged. Namespaced per the Matrix convention for custom keys.
const threadContentField = "io.runlore.thread"

// threadStamp is the thread context as carried on a Matrix event. It holds
// identifiers only — never prose — so the event stays small and nothing
// sensitive is duplicated into room history that is not already in the message.
type threadStamp struct {
	TriggerKey     string `json:"trigger_key,omitempty"`
	DupFingerprint string `json:"dup_fingerprint,omitempty"`
	Title          string `json:"title,omitempty"`
	Resource       string `json:"resource,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	CuratedURL     string `json:"curated_url,omitempty"`
	RecalledEntry  string `json:"recalled_entry,omitempty"`
}

// stampFor renders the investigation's thread identifiers for the event
// content. TriggerKey falls back to Fingerprint exactly like Deliver's legacy
// triggerKeyContentField does (cmp.Or): a re-investigation's Request carries a
// Fingerprint but no TriggerKey (internal/investigate/reinvestigate.go), and
// without the same fallback here the stamp's own trigger_key would be empty —
// which contextFromContent decodes and returns FIRST, before ever looking at
// the legacy field, silently breaking notify.matrix.feedback_reactions for
// every re-investigation.
func stampFor(inv providers.Investigation) threadStamp {
	return threadStamp{
		TriggerKey:    cmp.Or(inv.TriggerKey, inv.Fingerprint),
		Title:         inv.Title,
		Resource:      inv.Resource.Ref(),
		Verdict:       string(inv.Verdict),
		CuratedURL:    inv.CuratedURL,
		RecalledEntry: inv.RecalledEntry,
	}
}

// contextFromStamp rebuilds a thread.Context from a stamped event. root and
// room come from the event itself, not from the stamp, so a forged stamp
// cannot move a note into a different ROOM or thread.
//
// It does NOT follow that a forged stamp cannot redirect where a note is
// WRITTEN, and this function is the wrong place to look for that guarantee:
// curated_url is the write destination, and it comes straight out of the stamp
// below. Two controls one layer up are what actually hold. First,
// MatrixFeedback.contextFor discards the whole context unless the stamped
// event's sender is the bot itself, so nobody else's message is ever read as a
// stamp — that is the primary control. Second, thread.Responder.ForgeRepo
// refuses to take a routing decision from a URL that does not name the one
// repository RunLore writes to, so even a stamp that got through could not
// point a note at another repository's pull-request numbering — not on another
// host, and not on the same host either, which is what a host-only anchor left
// wide open.
func contextFromStamp(s threadStamp, root, room string) thread.Context {
	return thread.Context{
		Transport:      "matrix",
		Root:           root,
		Channel:        room,
		TriggerKey:     s.TriggerKey,
		DupFingerprint: s.DupFingerprint,
		Title:          s.Title,
		Resource:       s.Resource,
		Verdict:        providers.Verdict(s.Verdict),
		CuratedURL:     s.CuratedURL,
		RecalledEntry:  s.RecalledEntry,
	}
}

// contextFromContent extracts a thread.Context from one fetched event's
// content map — root and room are always the fetch's own parameters, never
// anything read from content, so a forged field cannot redirect where a note
// is written (the caller still owns the sender trust-check; this function only
// decodes).
//
// io.runlore.thread wins when present: content[threadContentField] is an
// `any` decoded off the wire, so it is re-marshalled through encoding/json
// into threadStamp rather than hand-cast field by field — a hand-cast would
// silently yield zero values on any type mismatch instead of surfacing it.
// Failing that, the legacy io.runlore.trigger_key field is used on its own.
// Neither field present (or the thread field present but undecodable) yields
// false: not one of RunLore's stamped messages.
//
// A decoded stamp whose OWN trigger_key is empty still falls back to the
// legacy field when present — defense in depth alongside stampFor's identical
// cmp.Or fallback: stampFor (the only builder today) never leaves trigger_key
// empty while a trigger identity exists, but this keeps the read side correct
// independently of that, for a stamp built any other way.
func contextFromContent(content map[string]any, root, room string) (thread.Context, bool) {
	legacyKey, _ := content[triggerKeyContentField].(string)
	if raw, ok := content[threadContentField]; ok {
		if b, err := json.Marshal(raw); err == nil {
			var stamp threadStamp
			if err := json.Unmarshal(b, &stamp); err == nil {
				tc := contextFromStamp(stamp, root, room)
				if tc.TriggerKey == "" && legacyKey != "" {
					tc.TriggerKey = legacyKey
				}
				return tc, true
			}
		}
	}
	if legacyKey != "" {
		return thread.Context{Transport: "matrix", Root: root, Channel: room, TriggerKey: legacyKey}, true
	}
	return thread.Context{}, false
}
