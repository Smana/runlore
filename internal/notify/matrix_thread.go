// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
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

// threadStamp is the thread context as carried on a Matrix event: six
// identifiers and exactly one prose field, every one of them BOUNDED at the
// point the stamp is built and read (see boundStamp, which stampFor and
// contextFromStamp both funnel through).
//
// The "identifiers only — never prose" this comment used to claim was never
// true. Title has been on the stamp since the field existed and is filled from
// inv.Title, which Matrix.Deliver's own doc comment calls model-authored and
// uncapped — so the sentence promising the event stayed small was the thing
// standing in for the bound that was missing. A 64 KiB model title produced a
// 78 KiB stamp beside two bodies that had dutifully bounded themselves to 16 KiB
// each, and the homeserver rejected all 115 KiB of it: the silent-delivery
// failure boundMatrixBodies exists to prevent, reached one field to its right.
//
// Title EARNS its place rather than merely surviving here. contextFromStamp is
// the registry-miss path (thread.Registry TTL expiry, eviction, a leader
// failover onto a replica without the JSONL, a restart), and on that path the
// stamp is the only thing that knows which investigation the thread is about —
// the advantage a Matrix event id has over a Slack thread_ts, which carries
// nothing on its own. Drop Title and every note written after a restart says
// only "Operator note" with no finding named: thread.Chat.renderContext loses
// its "Investigation:" line, thread.NoteBody loses its "Thread:" line, and
// conceptDescription loses the "on the finding %q" clause that makes the note
// reachable from the alert again.
//
// What does NOT belong here is anything larger than an identity. Evidence is the
// worked example and thread.Context.Evidence documents its own exclusion: a
// stamp is forgeable, so evidence read back off one would be attacker-controlled
// text flowing straight into a model prompt.
type threadStamp struct {
	TriggerKey     string `json:"trigger_key,omitempty"`
	DupFingerprint string `json:"dup_fingerprint,omitempty"`
	Title          string `json:"title,omitempty"`
	Resource       string `json:"resource,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	CuratedURL     string `json:"curated_url,omitempty"`
	RecalledEntry  string `json:"recalled_entry,omitempty"`
}

// The stamp's ceilings. They are stated in ENCODED bytes where the whole stamp
// is concerned, because that is the only measure the homeserver shares: the
// Matrix spec caps an event at 65,536 bytes "encoded as Canonical JSON", and
// canonical JSON turns one control byte into a six-byte \u001b escape. A cap
// counted on the string as composed is therefore not a cap on the event — the
// same mistake, one layer down, as bounding the bodies and not the stamp.
const (
	// matrixStampBytes is the encoded stamp's share of one event. Two fields
	// claim it — this stamp and the legacy triggerKeyContentField it feeds — so
	// the arithmetic for the whole event is:
	//
	//	 32,768  two bodies at matrixDeliverBodyBytes each
	//	  8,192  two stamps at matrixStampBytes each
	//	 ------
	//	 40,960  everything Deliver writes
	//	 24,576  left for what it does not: the federation envelope (room_id,
	//	         sender, origin_server_ts, depth, prev_events, auth_events), the
	//	         content hash and one signature per participating server
	//	 ------
	//	 65,536  the spec's hard ceiling
	//
	// A real envelope is one to two KiB, so the holdback is an order of
	// magnitude over what it needs. That asymmetry is deliberate: guessing low
	// is a rejected event and an on-call told nothing, guessing high costs card
	// text nobody was going to read.
	matrixStampBytes = 4 << 10
	// matrixStampTitleBytes bounds the one PROSE field. 200 is the largest
	// budget any reader of thread.Context.Title applies — thread's own
	// maxChatIdentityFieldBytes, which Chat.renderContext re-cuts to anyway,
	// while conceptDescription re-cuts to 120 — so the stamp never hands a
	// reader less than that reader would have cut for itself, and an ordinary
	// finding's title (curator.CapTitle caps a curated one at 120) crosses
	// whole.
	matrixStampTitleBytes = 200
	// matrixStampIDBytes bounds ONE identifier. Generous for every one stamped:
	// a "namespace/name" ref, a forge pull-request URL, a catalog entry path,
	// and a trigger key assembled as "alertname|namespace|kind|name|cluster"
	// from alert labels (curator.IncidentKey), whose Kubernetes-shaped parts
	// come to a couple of hundred bytes in the worst realistic case. Small
	// enough that all six plus the title stay under matrixStampBytes with room
	// for the JSON framing, so the shed below is a backstop against escape
	// expansion rather than something an ordinary card ever reaches.
	matrixStampIDBytes = 512
)

// stampEllipsis marks a shortened prose field. A reader who cannot tell a title
// was cut reads what survived as the whole of it — and unlike a human reading a
// shortened card, the reader here is a model being told what to answer about.
const stampEllipsis = "…"

// boundStamp makes a stamp fit the event, and is the single funnel BOTH
// directions go through: stampFor cannot build an unbounded stamp and
// contextFromStamp cannot decode one. Making it a step a caller has to remember
// is precisely how the defect above happened — the bodies were bounded at
// matrix.go:132 and the stamp assigned unbounded at :154, twenty-two lines
// later.
//
// Prose is TRUNCATED and identifiers are DROPPED, and the asymmetry is the whole
// design:
//
//   - A shortened title is still a true answer to "which investigation is this".
//     Every reader re-cuts it anyway, so the cut takes off only what would have
//     come off downstream; the ellipsis says it happened.
//   - A shortened identifier is not a shortened anything — it is a DIFFERENT
//     identifier. A cut trigger_key names no incident, or on a prefix collision
//     the wrong one, and that is a 👍 recorded against another finding in the
//     outcome ledger, where it weights future trust. A cut curated_url is a
//     write destination that is not the pull request it claims to be. Absence is
//     a state every reader already handles — an empty trigger_key falls through
//     to the legacy field and then to "not one of ours", an empty curated_url
//     makes the thread open its own PR — and a wrong identity is a state nothing
//     handles, because nothing can detect it.
//
// The per-field caps cannot finish the job on their own, because escaping
// happens after them: six identifiers of ANSI escape bytes sit inside their caps
// as composed and encode to six times that. So the encoded stamp is then
// MEASURED and fields are shed until it fits — see stampShedOrder for what goes
// first. Shedding terminates because an empty stamp encodes to two bytes.
func boundStamp(s threadStamp) threadStamp {
	s.Title = stampProse(s.Title, matrixStampTitleBytes)
	for _, id := range []*string{
		&s.TriggerKey, &s.DupFingerprint, &s.Resource,
		&s.Verdict, &s.CuratedURL, &s.RecalledEntry,
	} {
		*id = stampIdentifier(*id, matrixStampIDBytes)
	}
	for _, field := range stampShedOrder(&s) {
		if stampEncodedBytes(s) <= matrixStampBytes {
			break
		}
		*field = ""
	}
	return s
}

// stampShedOrder lists the stamp's fields in the order they are given up when
// the encoded stamp is still over budget after the per-field caps, least
// costly to lose first.
//
// Prose goes first: a context with no title still names the right incident,
// while a context with no trigger_key names nothing at all. Then the fields a
// reader can do without one at a time, and trigger_key last — it is the join to
// the investigation, the key the outcome ledger records against and the key
// thread.Registry is looked up by, so a stamp that has given up everything else
// is still worth sending for that one field alone.
func stampShedOrder(s *threadStamp) []*string {
	return []*string{
		&s.Title, &s.RecalledEntry, &s.Resource, &s.Verdict,
		&s.CuratedURL, &s.DupFingerprint, &s.TriggerKey,
	}
}

// stampProse bounds one prose field, cutting on a rune boundary so no invalid
// UTF-8 reaches the wire, and marking the cut. The ellipsis is reserved INSIDE
// maxBytes, so the result is never longer than the budget it was given.
func stampProse(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return cutBytesToRuneBoundary(s, maxBytes-len(stampEllipsis)) + stampEllipsis
}

// stampIdentifier returns id when it fits maxBytes and "" when it does not —
// see boundStamp for why an identifier is dropped rather than cut down.
func stampIdentifier(id string, maxBytes int) string {
	if len(id) > maxBytes {
		return ""
	}
	return id
}

// stampEncodedBytes is the stamp's size as the HOMESERVER counts it, which is
// not the sum of its field lengths.
//
// SetEscapeHTML(false) is what makes it the homeserver's arithmetic rather than
// Go's: encoding/json escapes "<", ">" and "&" into six-byte \u003c-style
// escapes by default, and Matrix's canonical JSON does not, so measuring with
// the default would shed identity off stamps that fit perfectly well.
func stampEncodedBytes(s threadStamp) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a struct of strings cannot fail to encode
	return buf.Len() - len("\n")
}

// stampFor renders the investigation's thread identifiers for the event
// content, bounded — see boundStamp, which every stamp is built through so that
// no caller can produce one that does not fit the event.
//
// TriggerKey falls back to Fingerprint exactly like Deliver's legacy
// triggerKeyContentField does (cmp.Or): a re-investigation's Request carries a
// Fingerprint but no TriggerKey (internal/investigate/reinvestigate.go), and
// without the same fallback here the stamp's own trigger_key would be empty —
// which contextFromContent decodes and returns FIRST, before ever looking at
// the legacy field, silently breaking notify.matrix.feedback_reactions for
// every re-investigation.
func stampFor(inv providers.Investigation) threadStamp {
	return boundStamp(threadStamp{
		TriggerKey:    cmp.Or(inv.TriggerKey, inv.Fingerprint),
		Title:         inv.Title,
		Resource:      inv.Resource.Ref(),
		Verdict:       string(inv.Verdict),
		CuratedURL:    inv.CuratedURL,
		RecalledEntry: inv.RecalledEntry,
	})
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
//
// The stamp is re-bounded on the way IN, and that is not belt-and-braces on
// stampFor's bound: a room's history holds events THIS process did not send.
// Every investigation delivered before boundStamp existed carries an unbounded
// stamp, and the registry-miss path fetches exactly those — an eviction, a TTL
// expiry or a restart is what sends it looking. What it decodes goes into a
// model prompt (thread.Chat.renderContext) and into a knowledge-base entry body
// (thread.NoteBody's "Thread:" line, which caps nothing), so an upgrade must not
// leave a 64 KiB title reachable through room history.
//
// Bounding NARROWS what is taken from the stamp; it widens nothing. root and
// room are still the caller's own, and every trust control named above is
// unchanged.
func contextFromStamp(s threadStamp, root, room string) thread.Context {
	s = boundStamp(s)
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
//
// The legacy key gets the stamp's own identifier rule applied to it (dropped
// whole when over matrixStampIDBytes, never cut down), for the same reason
// contextFromStamp re-bounds: this field is also read back off events written
// before any bound existed, and it is the one the outcome ledger records a
// rating against. An over-long one now reads as no trigger identity at all,
// which every caller already handles.
func contextFromContent(content map[string]any, root, room string) (thread.Context, bool) {
	legacyKey, _ := content[triggerKeyContentField].(string)
	legacyKey = stampIdentifier(legacyKey, matrixStampIDBytes)
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
