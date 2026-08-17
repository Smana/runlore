// SPDX-License-Identifier: Apache-2.0

// Package thread turns a human reply in a chat thread into a knowledge-base
// write. It is transport-agnostic: Slack and Matrix adapters resolve a
// [Context] their own way, hand it to a [Responder], and post back the reply
// string it returns. Nothing here knows about either chat system.
package thread

import (
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Context is the answer to "which investigation is this thread about" — the
// join between an opaque transport handle (a Slack thread_ts, a Matrix event
// id) and the finding whose knowledge base a reply should be written into.
type Context struct {
	Transport string // "slack" | "matrix" — logs and metrics only
	Root      string // opaque transport handle for the thread root
	Channel   string // Slack channel id; Matrix room id

	TriggerKey     string
	DupFingerprint string
	Title          string
	Resource       string // rendered "namespace/name"; "" when the finding named none
	Verdict        providers.Verdict

	// CuratedURL is the KB PR the curator opened for this finding; "" when it
	// opened none (a recall, a skipped verdict, or a coalesced duplicate).
	CuratedURL string
	// RecalledEntry is the catalog entry path a recalled answer came from; "" on
	// a fresh investigation.
	RecalledEntry string
	// NoteURL is the standalone PR THIS thread opened for an operator note. It
	// exists so the second note in a thread comments on the first note's PR
	// instead of opening another one — OpenPR is not idempotent (its branch name
	// carries a unix timestamp), so without this every note is a new PR.
	NoteURL string
	// Notes counts knowledge writes made from this thread, for the per-thread cap.
	Notes int

	// Evidence is what the investigation FOUND, carried so a reply can be
	// answered from the reasoning that already happened. It is why
	// `@runlore reinvestigate:` is a non-goal: "did you check the
	// NetworkPolicies?" is answerable from the ruled-out list without
	// re-running anything.
	//
	// It lives here, in the registry's own record, and NOWHERE ELSE —
	// notably not on the Matrix event stamp (see stampFor / contextFromStamp
	// in internal/notify/matrix_thread.go). A Matrix event is capped at
	// 64 KiB, and stamped content is FORGEABLE, which is exactly why
	// contextFromStamp takes root and room from the event rather than the
	// stamp; evidence read back off a stamp would be attacker-controlled text
	// flowing straight into a model prompt. So a context rebuilt from an event
	// — the registry-miss path in GetOrCreate, after a TTL expiry, an
	// eviction or a restart — carries no evidence at all. That is the correct
	// and already-documented degradation: identity-only, and every reader here
	// must render fine with an empty payload.
	//
	// omitzero, not omitempty: omitempty is a no-op for a struct value, so an
	// evidence-free context would persist a useless "Evidence":{} on every one
	// of the registry's append-only lines.
	Evidence Evidence `json:",omitzero"`

	At time.Time // when the finding was delivered; drives TTL expiry
}

// Evidence is the reasoning an investigation produced, bounded for storage:
// what it concluded, what it rejected, what it could not settle and what it
// could not see. Every list and every item is capped at the point of CAPTURE
// (see evidenceFrom) rather than at render time, so the ceiling below is a
// property of what the registry stores.
type Evidence struct {
	RootCauses []string `json:",omitempty"` // leading root-cause summaries, in the investigation's own order
	RuledOut   []string `json:",omitempty"` // hypotheses rejected, with the disproving evidence
	Unresolved []string `json:",omitempty"` // what the investigation could not determine
	DataGaps   []string `json:",omitempty"` // signals that could not be obtained
}

// The evidence caps. They exist because the registry is not a cache of one
// context: it holds up to DefaultRegistryMax of them and appends every write
// to JSONL, so an unbounded evidence blob is an unbounded file.
const (
	// MaxEvidenceRootCauses keeps the LEADING root causes: evidenceFrom takes a
	// prefix of providers.Investigation.RootCauses and does not reorder it.
	//
	// Nothing guarantees that order is sorted by confidence. The submit_findings
	// tool asks the model for "ranked root causes" and the model usually obliges,
	// but internal/investigate/tools.go stores the array exactly as it arrives,
	// and capRecallConfidence (internal/investigate/confirm.go) later clamps
	// individual confidences downward, which can leave a leading hypothesis below
	// a later one. So this is a prefix of what the investigation REPORTED, which
	// is the same order every other reader treats as authoritative:
	// internal/notify/slack.go's top cause, internal/curator/draft.go's numbered
	// "## Cause" list, internal/curator/fingerprint.go's cause. Sorting here
	// would tell the chat model a different story than the on-call was shown.
	MaxEvidenceRootCauses = 3
	// MaxEvidenceListItems bounds each of the other three lists.
	MaxEvidenceListItems = 5
	// MaxEvidenceItemBytes bounds one item's text. Each of these is one
	// summary line by contract (see providers.Investigation's field comments),
	// so this is a generous line rather than a clipped paragraph.
	MaxEvidenceItemBytes = 100
	// MaxEvidenceBytes is the resulting worst case for one context, in text
	// bytes: 18 items at the per-item cap. The +2 is truncate's overshoot — it
	// cuts at MaxEvidenceItemBytes-1 and appends a 3-byte "…".
	//
	// 1836 bytes, so DefaultRegistryMax live contexts are under 4 MiB of
	// registry JSONL — a figure readable off this source rather than derived
	// from it. JSON field names, quoting and escaping sit on top of it.
	MaxEvidenceBytes = (MaxEvidenceRootCauses + 3*MaxEvidenceListItems) * (MaxEvidenceItemBytes + 2)
)

// evidenceFrom lifts an investigation's reasoning into a stored, bounded
// Evidence. Blank items are dropped rather than kept: an investigation with
// nothing to say must produce the ZERO Evidence — which the persisted record
// then omits entirely — not a payload of empty strings.
func evidenceFrom(inv providers.Investigation) Evidence {
	ev := Evidence{
		RuledOut:   evidenceList(inv.RuledOut, MaxEvidenceListItems),
		Unresolved: evidenceList(inv.Unresolved, MaxEvidenceListItems),
		DataGaps:   evidenceList(inv.DataGaps, MaxEvidenceListItems),
	}
	for _, rc := range inv.RootCauses {
		if len(ev.RootCauses) == MaxEvidenceRootCauses {
			break
		}
		if s := evidenceItem(rc.Summary); s != "" {
			ev.RootCauses = append(ev.RootCauses, s)
		}
	}
	return ev
}

// evidenceList keeps the first limit non-blank items of in, each bounded. It
// returns nil — not an empty slice — when nothing survives, so the field's
// omitempty holds.
func evidenceList(in []string, limit int) []string {
	var out []string
	for _, item := range in {
		if len(out) == limit {
			break
		}
		if s := evidenceItem(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// evidenceItem trims one item and bounds it to MaxEvidenceItemBytes on a rune
// boundary.
//
// It reuses truncate, note.go's truncator for machine-generated fields, rather
// than capNoteText: capNoteText reserves its "(truncated — N input bytes
// dropped)" marker INSIDE the caller's budget, and that marker is around 45
// bytes — at this cap it would eat nearly half of every truncated item to tell
// a model something the trailing "…" already tells it. That marker exists so a
// HUMAN can see their own verbatim words were cut (see NoteBody); these items
// are a model's own generated summaries, which is precisely the silent-
// shortening case truncate's doc comment describes.
func evidenceItem(s string) string {
	return truncate(strings.TrimSpace(s), MaxEvidenceItemBytes)
}
