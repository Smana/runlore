// SPDX-License-Identifier: Apache-2.0

// Package thread turns a human reply in a chat thread into a knowledge-base
// write. It is transport-agnostic: Slack and Matrix adapters resolve a
// [Context] their own way, hand it to a [Responder], and post back the reply
// string it returns. Nothing here knows about either chat system.
package thread

import (
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

	At time.Time // when the finding was delivered; drives TTL expiry
}
