// SPDX-License-Identifier: Apache-2.0

// Package outcome records, in an append-only JSONL ledger, whether an
// investigated incident later resolved and which answer was used for it — the
// "did it actually work?" signal the learning loop reads. The ledger keeps an
// in-memory index of still-open incidents, rebuilt by replaying the file on
// startup so a resolve survives a restart / leader failover.
package outcome

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fingerprint prefixes for incidents RunLore assigns a synthetic id to because the
// triggering source carries no external alert fingerprint (an Alertmanager/PagerDuty
// incident id). They are chosen so a derived id can never collide with a real
// fingerprint, and they double as the "no ground-truth resolve channel" marker that
// keeps such recall opens out of recall-decay (see Event.Resolvable / applyOpenLocked).
const (
	// GitOpsFingerprintPrefix marks a fingerprint derived from a GitOps failure's
	// resource-ref + condition reason.
	GitOpsFingerprintPrefix = "gitops:"
	// ReinvestigateFingerprintPrefix marks a fingerprint derived for a reinvestigate poll.
	ReinvestigateFingerprintPrefix = "reinvestigate:"
)

// DeriveFingerprint returns a stable, deterministic fingerprint for an incident that
// carries no external alert id, formed as prefix + a short hex sha256 of key (e.g. the
// trigger key, or resource-ref+reason). Determinism is the point: the same recurring
// incident derives the SAME fingerprint every time, so its opens roll up into
// Occurrences/Episodes as recurrences — while the prefix keeps it from ever colliding
// with a real Alertmanager/PagerDuty fingerprint.
func DeriveFingerprint(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(sum[:8])
}

// Derived reports whether fp was assigned by DeriveFingerprint (a synthetic id for a
// source with no external fingerprint). Such incidents have no resolve channel — no
// resolved-alert webhook can ever match them — so their recall opens are recorded for
// recurrence but never counted toward recall decay (see Event.Resolvable).
func Derived(fp string) bool {
	return strings.HasPrefix(fp, GitOpsFingerprintPrefix) ||
		strings.HasPrefix(fp, ReinvestigateFingerprintPrefix)
}

// Event is one ledger line: an investigation opened, or an incident resolved.
type Event struct {
	Event          string `json:"event"`                     // "open" | "resolve" | "feedback" | "checkpoint" | "confirm"
	Fingerprint    string `json:"fingerprint"`               // Alertmanager fingerprint (stable firing↔resolved)
	DupFingerprint string `json:"dup_fingerprint,omitempty"` // curator dedup fingerprint (resource+cause); the curated-PR resolution join key

	Kind     string    `json:"kind,omitempty"`  // open: "recall" | "fresh"
	Entry    string    `json:"entry,omitempty"` // open+recall: the recalled entry path
	Title    string    `json:"title,omitempty"`
	Resource string    `json:"resource,omitempty"`
	At       time.Time `json:"at"`

	// StartedAt is, on an open, the wall-clock time the INVESTIGATION BEGAN — whereas At
	// stamps its COMPLETION. The gap between the two is unbounded in practice (queue wait
	// on the single-worker queue, rate-limit backoff, coalesce/debounce, then the run
	// itself), which is exactly why it must be recorded rather than approximated: it is
	// the interval during which a resolve can legitimately land BEFORE the open it belongs
	// to, and so the exact bound on resolve-before-open pairing (see resolvesSince).
	// Zero on a legacy open written before this field existed, and on resolve/feedback/
	// checkpoint lines; omitempty keeps the append-only file compatible with old readers.
	StartedAt time.Time `json:"started_at,omitempty"`

	// Recurrence fields (written by the delivery path). Kept omitempty so the
	// append-only file stays backward/forward compatible with older readers.
	TriggerKey string `json:"trigger_key,omitempty"` // groups recurrences of the same alert; keys the byTrigger index
	CuratedURL string `json:"curated_url,omitempty"` // KB link surfaced as "previous: <link>" on recurrence
	Verdict    string `json:"verdict,omitempty"`     // curator's machine verdict on the investigation

	// User identifies the human behind a feedback event (a Slack user id) — the
	// dedup key that keeps one live vote per (TriggerKey, user), latest wins.
	// Empty on open/resolve lines. On a feedback line Kind carries the rating
	// ("up" | "down").
	User string `json:"user,omitempty"`

	// Resolvable is set on an open when we know whether a ground-truth resolve signal
	// can ever arrive for it: true for sources with a resolve channel (Alertmanager,
	// PagerDuty), false for sources that never emit one (GitOps, reinvestigate, or
	// Alertmanager with send_resolved off). A pointer for a three-state distinction:
	// nil ⇒ the field is absent, i.e. a LEGACY open written before this field existed —
	// those came from Alertmanager/PagerDuty and are treated as resolvable. Only a
	// non-resolvable recall open is excluded from recall decay (see applyOpenLocked).
	Resolvable *bool `json:"resolvable,omitempty"`

	// Checkpoint is set only on a compaction record (Event=="checkpoint"); nil on every
	// open/resolve. It carries the folded aggregate state of the events a compaction
	// dropped. Kept omitempty so normal lines are unaffected, and a distinct event kind
	// so OLD binaries — whose switch has no "checkpoint" case — ignore it gracefully
	// (they lose the pre-horizon aggregates, but do not choke).
	Checkpoint *checkpointData `json:"checkpoint,omitempty"`
}

// resolvable reports whether a ground-truth resolve signal can ever arrive for this
// open. A legacy open (field absent ⇒ nil) came from Alertmanager/PagerDuty, which do
// emit resolves, so it defaults to resolvable.
func (e Event) resolvable() bool { return e.Resolvable == nil || *e.Resolvable }

// Episode is a matched open→resolve pair (or, from Episodes(), an unresolved open
// when Resolved is false).
type Episode struct {
	Kind, Entry, Title, Resource string
	DupFingerprint               string // curator dedup fingerprint; stable join key for the curated-PR resolution check
	OpenedAt, ResolvedAt         time.Time
	Duration                     time.Duration
	Resolved                     bool
}

// pendingOpen is one unresolved open held in a fingerprint's LIFO pairing stack.
// Every open (fresh or recall) joins the stack so a resolve pops the correct one;
// counted records a recall open (Kind=="recall" && Entry!="") whose resolution
// should credit agg[entry].
type pendingOpen struct {
	entry   string
	counted bool
}

// Ledger is an append-only outcome log with an in-memory open-index and a cached
// per-entry recall aggregate (the OpenCounts roll-up). The aggregate is built once
// by replaying the file at New and then maintained INCREMENTALLY under mu on every
// Open/Resolve, so OpenCounts is O(1) and never re-reads the file — it lived on the
// recall hot path, which previously replayed the whole JSONL per incident lookup.
type Ledger struct {
	path string
	mu   sync.Mutex
	open map[string]Event // fingerprint → latest unresolved open

	// agg is the cached OpenCounts result: per recall entry, its recall/resolve
	// counts and last-confirmed time. Equal to a fresh full replay for any event
	// sequence (the same LIFO, order-independent pairing as Episodes()).
	agg map[string]Aggregate
	// pendingOpens/pendingResolves carry the same pairing state Episodes() rebuilds
	// per replay, but kept live so a resolve can find which open (and thus which
	// entry) it credits without re-reading the file.
	pendingOpens    map[string][]pendingOpen // fingerprint → LIFO stack of unresolved opens
	pendingResolves map[string][]time.Time   // fingerprint → buffered early resolves (resolve-before-open), FIFO

	// byTrigger indexes "open" events per TriggerKey so delivery can render
	// "Nth occurrence — previous: <KB link>" without replaying the file. Maintained
	// in lockstep with the durable write, rebuilt on load, like agg.
	byTrigger map[string]triggerAgg

	// votes holds the latest human feedback per (TriggerKey \x00 user), so a
	// duplicate click is idempotent and a changed vote MOVES instead of stacking —
	// without it, one enthusiastic clicker could sink a healthy entry under the
	// OutcomeFloor. Folded on replay like agg; checkpointed on compaction.
	votes map[string]feedbackVote

	// triggerConfirms counts confirmations per TriggerKey — the ContestedTriggers
	// join that lets the curate Contested pass tell a reviewer "N re-investigations
	// reached this same conclusion". Rebuilt on load, checkpointed on compaction.
	triggerConfirms map[string]int

	// droppedResolves counts orphan resolves discarded by the pendingResolves bound
	// (see maxPendingResolvesPerFingerprint) — spurious duplicate/replayed resolve
	// webhooks. Kept so the (otherwise silent) defensive drop is observable.
	droppedResolves int

	// staleResolves counts buffered resolves discarded at PAIRING time because they
	// predate the open's investigation (see resolvesSince) — a resolve from a bygone,
	// uninvestigated episode of the same fingerprint. A distinct signal from
	// droppedResolves (a buffer overflow, not a pairing decision), and kept for the same
	// reason: without it the discard is entirely silent, yet it is the one that decides
	// whether an entry gets a resolve credit. Checkpointed like droppedResolves so
	// compaction does not lose it.
	staleResolves int

	// maxEvents bounds the JSONL before loadLocked compacts it (0 disables). corruptLines
	// records how many lines the last load could not parse (so the skip is observable, not
	// silent). log carries a logger for those warnings; never nil after New.
	maxEvents    int
	corruptLines int
	log          *slog.Logger
}

// DefaultMaxEvents is the generous default compaction bound used by New (and by the
// serve wiring when outcome.max_events is unset). Chosen high enough that a healthy
// ledger never compacts in normal operation; compaction is a growth backstop, not a
// routine trim.
const DefaultMaxEvents = 50000

// checkpointData is the per-load snapshot a compaction writes so a later replay can
// reconstruct the exact in-memory state the dropped (absorbed) events produced, without
// re-reading them. It carries the complete mutable fold state: the per-entry aggregate,
// the per-TriggerKey occurrence index, the open-index + pairing stacks for still-unresolved
// opens, buffered early resolves, and the dropped-resolve counter. loadLocked seeds from
// it, then folds the retained tail on top. Unexported index types (triggerAgg, pendingOpen)
// have exported JSON mirrors here so they serialize.
type checkpointData struct {
	Agg             map[string]Aggregate         `json:"agg,omitempty"`
	ByTrigger       map[string]triggerAggJSON    `json:"by_trigger,omitempty"`
	OpenIndex       map[string]Event             `json:"open_index,omitempty"`
	PendingOpens    map[string][]pendingOpenJSON `json:"pending_opens,omitempty"`
	PendingResolves map[string][]time.Time       `json:"pending_resolves,omitempty"`
	Votes           map[string]feedbackVoteJSON  `json:"votes,omitempty"`
	TriggerConfirms map[string]int               `json:"trigger_confirms,omitempty"`
	DroppedResolves int                          `json:"dropped_resolves,omitempty"`
	StaleResolves   int                          `json:"stale_resolves,omitempty"`
}

type triggerAggJSON struct {
	Count      int       `json:"count"`
	Last       time.Time `json:"last"`
	CuratedURL string    `json:"curated_url,omitempty"`
	Entry      string    `json:"entry,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
}

type feedbackVoteJSON struct {
	Rating string `json:"rating"`
	Entry  string `json:"entry,omitempty"`
}

type pendingOpenJSON struct {
	Entry   string `json:"entry,omitempty"`
	Counted bool   `json:"counted,omitempty"`
}

// triggerAgg is the per-TriggerKey occurrence roll-up backing Occurrences and
// Recurrence.
type triggerAgg struct {
	count      int
	last       time.Time
	curatedURL string // CuratedURL of the newest open
	entry      string // Entry of the newest open ("" for fresh) — feedback attribution target
	verdict    string // Verdict of the newest open — the suppression gate's "conclusive?" input
}

// feedbackVote is the fold state of one (TriggerKey, user) feedback: the rating
// currently held and the entry it credited at vote time — kept so a changed vote
// can un-credit exactly what it credited, even if attribution has since moved to
// a newer open.
type feedbackVote struct {
	rating string // "up" | "down"
	entry  string // agg key credited; "" when the newest open was fresh (nothing credited)
}

// maxPendingResolvesPerFingerprint bounds the resolve-before-open buffer per
// fingerprint. The legitimate window (a transient incident whose resolve webhook
// lands before its open is recorded) needs only a handful of slots; anything beyond
// this generous cap is spurious — duplicate/replayed resolve webhooks, or resolves
// whose open was never recorded — and applyResolveLocked would otherwise buffer it
// forever, growing pendingResolves without limit on a long-lived leader. When full we
// drop the OLDEST entry (FIFO): a long-buffered resolve is the least likely to ever
// pair with a real open, and dropping it leaves the brief-window pairing intact.
const maxPendingResolvesPerFingerprint = 64

// legacyMaxEarlyResolveAge bounds how far a buffered resolve may predate an open that
// carries NO StartedAt — i.e. ONLY an open written before Event.StartedAt existed, replayed
// from a pre-upgrade ledger file. Every open written from now on carries its investigation's
// start time, and resolvesSince gates on that exactly, so this fallback governs nothing but
// history.
//
// It is deliberately generous rather than accurate, because for a legacy open the
// information needed to be accurate was never recorded: the open is stamped at COMPLETION,
// so the true bound is the enqueue→open latency, which the event does not carry. An hour is
// the pragmatic compromise that still discards the pathological case this guards (a resolve
// from a bygone episode, hours or days old, crediting the NEXT open for the fingerprint).
const legacyMaxEarlyResolveAge = time.Hour

// resolvesSince drops the buffered resolves that CANNOT belong to open e, and reports how
// many it dropped. Survivors are returned in arrival order; it reuses the backing array, so
// the caller must replace the slice it passed in.
//
// The rule is exact, not heuristic: a resolve can only legitimately precede the open it
// belongs to if it landed while that investigation was still RUNNING (the open is stamped
// at completion, so a resolve arriving mid-investigation is recorded first). A resolve that
// arrived BEFORE the investigation even began belongs to some earlier episode — the alert
// fired, cleared, and was never investigated (suppressed by dedup or the trigger policy), or
// self-resolved while the request sat in the queue. Pairing with such a resolve credits
// Resolved++ at open time, before the new incident has produced anything: a flapping alert
// would bank a resolve credit on every suppressed cycle for the next recall to cash in,
// inflating an entry's resolve rate on evidence that has nothing to do with it — a
// MANUFACTURED post hoc, not merely an unverified one.
//
// Gating on StartedAt rather than on the resolve's AGE is what makes this exact. The age of
// a legitimate early resolve is bounded by the whole ENQUEUE→OPEN latency — debounce,
// coalesce wait, the wait behind a single sequential worker, rate-limit backoff (a 1h window
// by default), and only then the investigation itself — which no fixed hour-scale constant
// can bound. An age bound therefore drops LEGITIMATE resolves off a backlogged queue and
// permanently deflates a correct entry's resolve rate. The start time is the real boundary,
// and the event carries it.
func resolvesSince(rs []time.Time, e Event) (kept []time.Time, dropped int) {
	// A legacy open (no StartedAt recorded) has no exact boundary to gate on; fall back to
	// the age bound it was written under.
	cutoff := e.StartedAt
	if cutoff.IsZero() {
		cutoff = e.At.Add(-legacyMaxEarlyResolveAge)
	}
	out := rs[:0]
	for _, at := range rs {
		if at.Before(cutoff) {
			dropped++
			continue
		}
		out = append(out, at)
	}
	return out, dropped
}

// New opens (replaying) the ledger at path with the default compaction bound
// (DefaultMaxEvents). An empty path returns a disabled, no-op ledger (the feature is off).
func New(path string) (*Ledger, error) {
	return NewWithMaxEvents(path, DefaultMaxEvents)
}

// NewWithMaxEvents opens (replaying) the ledger at path, compacting the JSONL on load
// when it exceeds maxEvents (0 disables compaction). An empty path returns a disabled,
// no-op ledger.
func NewWithMaxEvents(path string, maxEvents int) (*Ledger, error) {
	l := &Ledger{path: path, maxEvents: maxEvents, log: slog.Default()}
	if err := l.loadLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

// loadLocked (re)builds all in-memory state from a full replay of the file: it resets
// the open-index, the cached aggregate, and the pairing state, then folds every event
// back in. It is the single shared cache-build path called by New (single-threaded)
// and Reload (under mu) — keeping the two in lockstep. A disabled/absent ledger leaves
// the freshly-reset (empty) maps in place.
//
// The read is performed BEFORE any reset: if readEvents returns an error the prior
// cache is left untouched — callers see a stale-but-valid cache rather than an empty one.
func (l *Ledger) loadLocked() error {
	events, corrupt, err := l.readEvents()
	if err != nil {
		return err // prior cache untouched
	}
	l.resetStateLocked()
	l.corruptLines = corrupt
	if corrupt > 0 {
		// A corrupt line is a real signal (truncated write, disk corruption, a stray
		// edit) — surface it rather than dropping it silently.
		l.log.Warn("outcome ledger: skipped corrupt JSONL lines", "count", corrupt, "path", l.path)
	}
	for _, e := range events {
		l.foldLocked(e)
	}
	// Compaction backstop: an append-only ledger replayed in full on every startup /
	// leadership Reload / curate Episodes() grows without bound. When it exceeds the
	// configured cap, fold the oldest events into a single checkpoint record and keep a
	// recent tail — the in-memory state above is already the full, correct state, so a
	// failed rewrite is non-fatal (log and keep serving the uncompacted file).
	if l.maxEvents > 0 && len(events) > l.maxEvents {
		if cerr := l.compactLocked(events); cerr != nil {
			l.log.Warn("outcome ledger: compaction failed; continuing with uncompacted file", "err", cerr, "path", l.path)
		}
	}
	return nil
}

// resetStateLocked clears all derived in-memory state to empty (non-nil) maps.
func (l *Ledger) resetStateLocked() {
	l.open = map[string]Event{}
	l.agg = map[string]Aggregate{}
	l.pendingOpens = map[string][]pendingOpen{}
	l.pendingResolves = map[string][]time.Time{}
	l.byTrigger = map[string]triggerAgg{}
	l.votes = map[string]feedbackVote{}
	l.triggerConfirms = map[string]int{}
	l.droppedResolves = 0
	l.staleResolves = 0
}

// foldLocked folds one replayed event into the derived state. Open/resolve maintain the
// aggregate, open-index, pairing stacks, and occurrence index; feedback folds a human
// vote into the aggregate; a checkpoint seeds the state a prior compaction folded away.
// Any other (unknown/future) kind is ignored — the forward-compat property binaries
// older than a given kind rely on (they ignored "feedback" before it was folded, and
// "checkpoint" before compaction existed).
func (l *Ledger) foldLocked(e Event) {
	switch e.Event {
	case "open":
		l.open[e.Fingerprint] = e
		l.applyOpenLocked(e)
		l.applyTriggerLocked(e)
	case "resolve":
		delete(l.open, e.Fingerprint)
		l.applyResolveLocked(e.Fingerprint, e.At)
	case "feedback":
		l.applyFeedbackLocked(e)
	case "confirm":
		l.applyConfirmLocked(e)
	case "checkpoint":
		l.seedCheckpointLocked(e.Checkpoint)
	}
}

// seedCheckpointLocked restores the derived state a compaction saved. A checkpoint is
// always the first line of a compacted file, so this seeds onto the freshly-reset (empty)
// maps and the retained tail then folds on top. It sets state DIRECTLY (no re-crediting),
// because the checkpoint already accounts for every event it absorbed.
func (l *Ledger) seedCheckpointLocked(cd *checkpointData) {
	if cd == nil {
		return
	}
	// Copy every map/slice out of the decoded event: the ledger's live state must never
	// ALIAS the event held in the replay slice, or a later fold (or the compaction scratch
	// re-seeding from the same event) would mutate both through the shared reference.
	for k, v := range cd.Agg {
		l.agg[k] = v
	}
	for fp, ats := range cd.PendingResolves {
		l.pendingResolves[fp] = append([]time.Time(nil), ats...)
	}
	for fp, ev := range cd.OpenIndex {
		l.open[fp] = ev
	}
	for k, v := range cd.ByTrigger {
		l.byTrigger[k] = triggerAgg{count: v.Count, last: v.Last, curatedURL: v.CuratedURL, entry: v.Entry, verdict: v.Verdict}
	}
	for k, v := range cd.Votes {
		l.votes[k] = feedbackVote{rating: v.Rating, entry: v.Entry}
	}
	for k, v := range cd.TriggerConfirms {
		l.triggerConfirms[k] = v
	}
	for fp, opens := range cd.PendingOpens {
		stack := make([]pendingOpen, 0, len(opens))
		for _, o := range opens {
			stack = append(stack, pendingOpen{entry: o.Entry, counted: o.Counted})
		}
		l.pendingOpens[fp] = stack
	}
	l.droppedResolves += cd.DroppedResolves
	l.staleResolves += cd.StaleResolves
}

// compactLocked rewrites the ledger file as [checkpoint][recent tail]: it folds the
// oldest events (everything but the most recent maxEvents-1) into a checkpoint that
// captures their exact aggregate contribution, and retains the tail as raw lines. The
// caller's live state is untouched (already the full, correct state); this only shrinks
// the file. Episodes() replay is preserved for the retained tail — episodes older than
// this horizon are folded into the checkpoint (still counted in the aggregate) but are no
// longer individually replayable, which the Phase-2 Queue/Recurrence passes tolerate
// (they only need recent history).
func (l *Ledger) compactLocked(events []Event) error {
	keep := l.maxEvents - 1 // reserve one slot for the checkpoint line
	if keep < 0 {
		keep = 0
	}
	cut := len(events) - keep
	if cut <= 0 {
		return nil // nothing old enough to absorb
	}
	// Fold the absorbed prefix in a throwaway ledger to snapshot the state it produces,
	// without disturbing the caller's live (full) state.
	scratch := &Ledger{log: l.log}
	scratch.resetStateLocked()
	for _, e := range events[:cut] {
		scratch.foldLocked(e)
	}
	return l.rewriteFileLocked(scratch.snapshotCheckpointLocked(), events[cut:])
}

// snapshotCheckpointLocked captures the receiver's derived state into a checkpointData.
// Safe to reference the live maps directly: the receiver is a throwaway scratch ledger.
func (l *Ledger) snapshotCheckpointLocked() *checkpointData {
	cd := &checkpointData{
		Agg:             l.agg,
		OpenIndex:       l.open,
		PendingResolves: l.pendingResolves,
		DroppedResolves: l.droppedResolves,
		StaleResolves:   l.staleResolves,
	}
	if len(l.byTrigger) > 0 {
		cd.ByTrigger = make(map[string]triggerAggJSON, len(l.byTrigger))
		for k, v := range l.byTrigger {
			cd.ByTrigger[k] = triggerAggJSON{Count: v.count, Last: v.last, CuratedURL: v.curatedURL, Entry: v.entry, Verdict: v.verdict}
		}
	}
	if len(l.votes) > 0 {
		cd.Votes = make(map[string]feedbackVoteJSON, len(l.votes))
		for k, v := range l.votes {
			cd.Votes[k] = feedbackVoteJSON{Rating: v.rating, Entry: v.entry}
		}
	}
	if len(l.triggerConfirms) > 0 {
		cd.TriggerConfirms = make(map[string]int, len(l.triggerConfirms))
		for k, v := range l.triggerConfirms {
			cd.TriggerConfirms[k] = v
		}
	}
	if len(l.pendingOpens) > 0 {
		cd.PendingOpens = make(map[string][]pendingOpenJSON, len(l.pendingOpens))
		for fp, opens := range l.pendingOpens {
			js := make([]pendingOpenJSON, 0, len(opens))
			for _, o := range opens {
				js = append(js, pendingOpenJSON{Entry: o.entry, Counted: o.counted})
			}
			cd.PendingOpens[fp] = js
		}
	}
	return cd
}

// rewriteFileLocked atomically replaces the ledger file with the checkpoint followed by
// the retained tail events, via a temp file + fsync + rename (so an interrupted compaction
// can never leave a half-written ledger).
func (l *Ledger) rewriteFileLocked(cd *checkpointData, tail []Event) error {
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".outcomes-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()
	w := bufio.NewWriter(tmp)
	writeLine := func(e Event) error {
		b, mErr := json.Marshal(e)
		if mErr != nil {
			return mErr
		}
		if _, wErr := w.Write(append(b, '\n')); wErr != nil {
			return wErr
		}
		return nil
	}
	if err := writeLine(Event{Event: "checkpoint", At: time.Now(), Checkpoint: cd}); err != nil {
		_ = tmp.Close()
		return err
	}
	for _, e := range tail {
		if err := writeLine(e); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // durability before the rename
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return err
	}
	// fsync the directory so the rename itself survives a crash.
	if d, derr := os.Open(dir); derr == nil { //nolint:gosec // G304: dir is the parent of the operator-configured ledger path
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Reload re-replays the ledger file under the write lock and rebuilds the cached
// aggregate, open-index, and pairing state from scratch. The cache is otherwise
// maintained incrementally and so only reflects writes made through THIS process: in a
// multi-replica HA deployment sharing one ledger file, a replica that loses then
// re-acquires leadership would keep serving its pre-handover cache, missing every
// open/resolve another replica appended while it was a follower — stale aggregates and
// thus wrong recall-decay. Call Reload when this process (re-)acquires leadership so it
// re-syncs with those external writes. It takes the write lock (no torn read against a
// concurrent OpenCounts). A disabled/nil ledger is a no-op.
func (l *Ledger) Reload() error {
	if !l.enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.loadLocked()
}

// applyOpenLocked folds one open event into the cached aggregate, mirroring
// Episodes(): a counted recall open increments Recalls; if an early resolve is
// already buffered for this fingerprint, the open is paired immediately and also
// counts as resolved. Must be called with mu held (or during single-threaded New).
func (l *Ledger) applyOpenLocked(e Event) {
	// A recall open counts toward decay ONLY when it is resolvable — i.e. a resolve
	// signal for it can actually arrive. A non-resolvable recall (GitOps, reinvestigate,
	// or Alertmanager with send_resolved off) neither builds nor erodes trust: counting
	// it toward Recalls with no possible Resolved would decay a CORRECT entry's
	// resolve-rate forever, on evidence the source can never provide. So decay is only
	// learned where a ground-truth resolve signal exists. Episodes()/Occurrences() still
	// include EVERY open regardless of resolvability, so recurrence counting is unaffected.
	counted := e.Kind == "recall" && e.Entry != "" && e.resolvable()
	if counted {
		a := l.agg[e.Entry]
		a.Recalls++
		l.agg[e.Entry] = a
	}
	// Order-independent pairing: a resolve that arrived before this open is buffered;
	// pair with the earliest such resolve (FIFO), matching Episodes(). Resolves that
	// predate this open's investigation cannot belong to it and are discarded first —
	// see resolvesSince.
	if rs := l.pendingResolves[e.Fingerprint]; len(rs) > 0 {
		kept, stale := resolvesSince(rs, e)
		if stale > 0 {
			// Count + log: the discard decides whether an entry gets a resolve credit, so
			// it must never be silent (cf. droppedResolves).
			l.staleResolves += stale
			l.log.Debug("outcome ledger: discarded buffered resolves predating the investigation",
				"count", stale, "fingerprint", e.Fingerprint, "started_at", e.StartedAt, "opened_at", e.At)
		}
		if len(kept) > 0 {
			at := kept[0]
			l.pendingResolves[e.Fingerprint] = kept[1:]
			if counted {
				l.creditResolveLocked(e.Entry, at)
			}
			return
		}
		// Every buffered resolve was stale: this open is genuinely unresolved so far.
		delete(l.pendingResolves, e.Fingerprint)
	}
	l.pendingOpens[e.Fingerprint] = append(l.pendingOpens[e.Fingerprint], pendingOpen{entry: e.Entry, counted: counted})
}

// applyResolveLocked folds one resolve event into the cached aggregate, mirroring
// Episodes(): pop the most-recent unresolved open (LIFO) for the fingerprint and,
// if it was a counted recall, credit its resolution; with no pending open, buffer
// the resolve for a later open. Must be called with mu held (or during New).
func (l *Ledger) applyResolveLocked(fp string, at time.Time) {
	stack := l.pendingOpens[fp]
	if len(stack) == 0 {
		// No pending open yet — buffer this resolve for a later open (resolve-before-open).
		// Defensive bound: orphan resolves that never pair (duplicate/replayed resolve
		// webhooks, or resolves whose open was never recorded) would otherwise accumulate
		// here forever on a long-lived leader. Cap the per-fingerprint buffer, dropping the
		// oldest excess — these are spurious; the legitimate brief window needs only a few,
		// so its pairing is untouched.
		rs := l.pendingResolves[fp]
		if len(rs) >= maxPendingResolvesPerFingerprint {
			l.droppedResolves++
			rs = rs[1:] // drop oldest (FIFO) to make room for the newer resolve
		}
		l.pendingResolves[fp] = append(rs, at)
		return
	}
	top := stack[len(stack)-1]
	l.pendingOpens[fp] = stack[:len(stack)-1]
	if top.counted {
		l.creditResolveLocked(top.entry, at)
	}
}

// creditResolveLocked records a resolved recall for entry: Resolved++ and bump
// LastConfirmed to the latest resolve time. Must be called with mu held (or New).
func (l *Ledger) creditResolveLocked(entry string, at time.Time) {
	a := l.agg[entry]
	a.Resolved++
	if at.After(a.LastConfirmed) {
		a.LastConfirmed = at
	}
	l.agg[entry] = a
}

// readEvents replays the ledger file in order, skipping (and counting) corrupt lines.
// The second return is the number of unparseable lines skipped — surfaced by the caller
// so the skip is observable, never silent. It returns a nil slice when the ledger is
// disabled (path=="") or the file is absent.
func (l *Ledger) readEvents() ([]Event, int, error) {
	if l.path == "" {
		return nil, 0, nil
	}
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var events []Event
	var corrupt int
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			corrupt++ // skip a corrupt line rather than fail, but count it
			continue
		}
		events = append(events, e)
	}
	return events, corrupt, sc.Err()
}

func (l *Ledger) enabled() bool { return l != nil && l.path != "" }

// Enabled reports whether the ledger will actually persist events (a non-empty
// ledger_path was configured); nil-safe. Exported for wiring sites: a disabled
// ledger's methods silently no-op, so handing it to a consumer that assumes its
// writes land would misrepresent persistence. Cheaper than Status(), which
// re-reads the whole file.
func (l *Ledger) Enabled() bool { return l.enabled() }

// Status is a cheap snapshot of the ledger's on-disk reality, used to tell apart
// "feature off" (Configured=false) from "configured but the file the curate pod
// can see is absent/empty" (Configured=true, Present=false or Events==0) — the
// silent-no-op the `lore curate` startup warning surfaces. It re-reads the file
// (no cached open-index) so it reflects what a fresh process actually sees.
type Status struct {
	Path       string // the configured ledger path ("" when disabled)
	Configured bool   // a non-empty ledger_path was set
	Present    bool   // the file exists (true even when empty)
	Events     int    // number of replayable events (0 for absent/empty)
}

// Status reports whether the ledger is configured and whether its file is
// actually present/non-empty where this process runs. A disabled ledger
// (path=="") reports Configured=false.
func (l *Ledger) Status() Status {
	s := Status{}
	if !l.enabled() {
		return s
	}
	s.Path, s.Configured = l.path, true
	if _, err := os.Stat(l.path); err == nil {
		s.Present = true
	}
	if events, _, err := l.readEvents(); err == nil {
		s.Events = len(events)
	}
	return s
}

func (l *Ledger) appendLocked(e Event) error {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	// fsync so an unclean crash/SIGKILL can't lose the tail open/resolve event — the
	// ledger is the durable truth for recall-decay attribution (the audit log fsyncs too).
	return f.Sync()
}

// Open records that an investigation completed for an incident (fingerprint).
func (l *Ledger) Open(e Event) error {
	if !l.enabled() {
		return nil
	}
	e.Event = "open"
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(e); err != nil {
		return err // append failed: leave both index and cache untouched
	}
	l.open[e.Fingerprint] = e
	l.applyOpenLocked(e)    // keep the OpenCounts cache in lockstep with the durable write
	l.applyTriggerLocked(e) // and the per-TriggerKey occurrence index
	return nil
}

// applyTriggerLocked folds one open into the per-TriggerKey occurrence index.
func (l *Ledger) applyTriggerLocked(e Event) {
	if e.TriggerKey == "" {
		return
	}
	a := l.byTrigger[e.TriggerKey]
	a.count++
	if !e.At.Before(a.last) {
		a.last = e.At
		a.curatedURL = e.CuratedURL
		// Feedback attribution follows the newest open: a fresh open (Entry "")
		// deliberately CLEARS it — a vote on a fresh investigation must not credit
		// an older recall's entry. The verdict tracks the newest open the same way.
		a.entry = e.Entry
		a.verdict = e.Verdict
	}
	l.byTrigger[e.TriggerKey] = a
}

// Occurrences reports how many investigations have been recorded for a
// TriggerKey, when the most recent one happened, and its KB link — the
// recurrence facts the notifier renders. Zero values for a disabled ledger,
// an empty key, or a never-seen key. A narrowing of Recurrence for the
// delivery path, which has no use for the verdict or the 👎-vote count.
func (l *Ledger) Occurrences(triggerKey string) (int, time.Time, string) {
	r := l.Recurrence(triggerKey)
	return r.Count, r.Last, r.CuratedURL
}

// TriggerRecurrence is the per-TriggerKey snapshot the pre-investigation
// suppression gate reads: how many investigations this trigger has had, when the
// last one was and what it concluded, its KB link, and how many humans currently
// contest that conclusion.
type TriggerRecurrence struct {
	Count        int
	Last         time.Time
	Verdict      string // newest open's verdict ("" for pre-verdict events)
	CuratedURL   string
	FeedbackDown int // LIVE 👎 votes for this trigger, after per-user dedup
}

// Contested reports whether the trigger carries standing 👎 feedback — the ONE
// definition of "a human contests this diagnosis" shared by every suppression
// layer that must yield to it (the recurrence gate and the coalescer's cooldown,
// #288). Layers consulting different notions of contested-ness is exactly the
// divergence that issue was about; add nuance here, not at the call sites.
func (r TriggerRecurrence) Contested() bool { return r.FeedbackDown > 0 }

// Recurrence returns the trigger's recurrence snapshot. FeedbackDown counts the
// votes map's current "down" entries for the key — O(live votes), which stays
// small (one entry per trigger×user) and off the recall hot path (read once per
// incoming investigation). Zero value for a disabled ledger or unseen key.
func (l *Ledger) Recurrence(triggerKey string) TriggerRecurrence {
	if !l.enabled() || triggerKey == "" {
		return TriggerRecurrence{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.byTrigger[triggerKey]
	tr := TriggerRecurrence{Count: a.count, Last: a.last, Verdict: a.verdict, CuratedURL: a.curatedURL}
	prefix := triggerKey + "\x00"
	for k, v := range l.votes {
		if v.rating == "down" && strings.HasPrefix(k, prefix) {
			tr.FeedbackDown++
		}
	}
	return tr
}

// ContestedTrigger is one trigger whose delivered conclusion has standing 👎
// votes AND a KB artifact to act on — the unit of work for the curate pass that
// warns the human reviewing the pending KB PR. Downs counts LIVE "down" votes
// after per-user dedup (a vote later moved to 👍 no longer counts); CuratedURL
// and Last come from the trigger's newest open, matching Recurrence.
type ContestedTrigger struct {
	TriggerKey string
	CuratedURL string    // KB link of the newest open (never "")
	Downs      int       // standing 👎 votes, per-user latest-wins
	Last       time.Time // when the trigger's newest investigation happened
	Confirms   int       // machine confirmations recorded for this trigger since (recovery evidence)
}

// ContestedTriggers returns every trigger with at least one standing 👎 vote and
// a non-empty CuratedURL, sorted by TriggerKey so the pass output is
// deterministic. It is the inverse view of Recurrence's per-key FeedbackDown:
// one iteration over the live votes map — O(live votes), which stays small (one
// entry per trigger×user) and runs on the scheduled curate cadence, not a hot
// path — joined against byTrigger for the newest open's KB link and time. A
// contested trigger with NO KB link is deliberately excluded: there is no PR for
// a reviewer to be warned on (the vote itself still stands in the ledger for
// trust decay and the recurrence cooldown). Nil for a disabled (or nil) ledger.
func (l *Ledger) ContestedTriggers() []ContestedTrigger {
	if !l.enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	downs := map[string]int{}
	for k, v := range l.votes {
		if v.rating != "down" {
			continue
		}
		trigger, _, ok := strings.Cut(k, "\x00")
		if !ok {
			continue // defensive: Feedback never writes a separator-less key
		}
		downs[trigger]++
	}
	out := make([]ContestedTrigger, 0, len(downs))
	for trigger, n := range downs {
		a := l.byTrigger[trigger]
		if a.curatedURL == "" {
			continue // no KB artifact to warn a reviewer on
		}
		out = append(out, ContestedTrigger{TriggerKey: trigger, CuratedURL: a.curatedURL, Downs: n, Last: a.last, Confirms: l.triggerConfirms[trigger]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TriggerKey < out[j].TriggerKey })
	return out
}

// Episodes replays the full ledger and turns every open into an Episode, pairing
// each resolve with the most-recent (LIFO) unresolved open for the same
// fingerprint — so recurrence is preserved (N opens + 1 resolve ⇒ N episodes, 1
// resolved). Pairing is order-independent: a resolve that arrives BEFORE its open
// (a transient incident that cleared mid-investigation, so the resolve webhook
// landed before the open was recorded) is buffered and paired with the next open
// for that fingerprint. Episodes are returned in open order; all kinds are
// included. A disabled/empty ledger yields nil.
func (l *Ledger) Episodes() ([]Episode, error) {
	if !l.enabled() {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	events, _, err := l.readEvents()
	if err != nil {
		return nil, err
	}
	var out []Episode
	pendingOpens := map[string][]int{}          // fingerprint → stack of indices into out (LIFO)
	pendingResolves := map[string][]time.Time{} // fingerprint → buffered resolve times (resolve-before-open)
	resolveAt := func(i int, at time.Time) {
		out[i].ResolvedAt = at
		d := at.Sub(out[i].OpenedAt)
		if d < 0 {
			d = 0 // guard: a resolve recorded before its open is not negative duration
		}
		out[i].Duration = d
		out[i].Resolved = true
	}
	for _, e := range events {
		switch e.Event {
		case "open":
			out = append(out, Episode{
				Kind: e.Kind, Entry: e.Entry, Title: e.Title, Resource: e.Resource,
				DupFingerprint: e.DupFingerprint,
				OpenedAt:       e.At,
			})
			i := len(out) - 1
			// Same pairing rule as applyOpenLocked, kept in lockstep: discard the buffered
			// resolves that predate this open's investigation (see resolvesSince), then pair
			// with the earliest survivor. The drop count is deliberately ignored here — this
			// is a read-only replay off the file, and folding it into the ledger's counter
			// would re-count the same discards on every Episodes() call.
			if rs := pendingResolves[e.Fingerprint]; len(rs) > 0 {
				kept, _ := resolvesSince(rs, e)
				if len(kept) > 0 {
					resolveAt(i, kept[0]) // pair with the earliest buffered (early) resolve
					pendingResolves[e.Fingerprint] = kept[1:]
					continue
				}
				delete(pendingResolves, e.Fingerprint)
			}
			pendingOpens[e.Fingerprint] = append(pendingOpens[e.Fingerprint], i)
		case "resolve":
			stack := pendingOpens[e.Fingerprint]
			if len(stack) == 0 {
				// No pending open yet — buffer this resolve for a later open.
				pendingResolves[e.Fingerprint] = append(pendingResolves[e.Fingerprint], e.At)
				continue
			}
			i := stack[len(stack)-1]
			pendingOpens[e.Fingerprint] = stack[:len(stack)-1]
			resolveAt(i, e.At)
		}
	}
	return out, nil
}

// Aggregate is a per-entry roll-up of recall episodes: how often the entry was
// recalled, how often the incident then resolved, and when it last resolved —
// plus the human 👍/👎 votes attributed to the entry (one live vote per
// TriggerKey+user, latest wins; see applyFeedbackLocked).
type Aggregate struct {
	Recalls       int
	Resolved      int
	FeedbackUp    int // human "the diagnosis was right" votes — success observations for decay
	FeedbackDown  int // human "the diagnosis was wrong" votes — failure observations for decay
	LastConfirmed time.Time
	// Confirms counts machine confirmations: fresh investigations that independently
	// reached this entry's conclusion (same DupFingerprint) — the recovery evidence a
	// standing 👎 forces into existence. Weighted at half a human observation in
	// Factor (confirmWeight); see Ledger.Confirm.
	Confirms int
}

// Factor is the entry's outcome-decay factor: the posterior mean of a symmetric
// Beta(k/2, k/2) prior over the success rate, folding resolves and human votes
// into one trust signal:
//
//	factor = (Resolved + FeedbackUp + k/2) / (Recalls + FeedbackUp + FeedbackDown + k)
//
// It is THE single definition of decay — recall's fire gate and the curate
// retirement pass both consume it, so they can never drift apart. See
// investigate's gate docs for the full statistical rationale.
func (a Aggregate) Factor(k float64) float64 {
	return (float64(a.Resolved+a.FeedbackUp) + k/2) / (float64(a.Recalls+a.FeedbackUp+a.FeedbackDown) + k)
}

// OpenCounts rolls recall episodes up per catalog entry (fresh investigations
// carry no entry). It is the input to recall decay: resolve-rate ≈
// (Resolved+k)/(Recalls+k), and runs on the recall hot path once per incident
// lookup. It returns the cached aggregate — built once at New and maintained
// incrementally on every Open/Resolve — so it is O(entries) and never re-reads the
// file; the value equals a fresh full replay of the ledger for any event sequence
// below the maxPendingResolvesPerFingerprint cap — above the cap (pathological
// orphan-resolve load), dropped excess resolves mean the cache and an unbounded
// Episodes() replay may diverge.
// A disabled/empty ledger yields an empty (non-nil) map. The returned map is a
// fresh copy the caller may freely mutate.
//
// Behaviour note: because the read moved to New/Reload, OpenCounts no longer performs
// file I/O and so can no longer return a read error — any error reading the ledger
// surfaces at construction (New) or re-sync (Reload) time, not per call. The error
// return is retained for signature stability and is always nil here.
func (l *Ledger) OpenCounts() (map[string]Aggregate, error) {
	if l == nil {
		return map[string]Aggregate{}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	counts := make(map[string]Aggregate, len(l.agg))
	for k, v := range l.agg {
		counts[k] = v
	}
	return counts, nil
}

// Feedback appends a human 👍/👎 verdict on a delivered investigation and folds it
// into the trust aggregate. rating is "up" or "down" (anything else is an error,
// never silently recorded); user is the stable reviewer id (a Slack user id) the
// per-trigger dedup keys on. Attribution is entirely TriggerKey-based — feedback
// lines carry no alert fingerprint and open/resolve pairing never sees them;
// binaries older than the fold ignored the kind entirely.
func (l *Ledger) Feedback(triggerKey, rating, user string, at time.Time) error {
	if rating != "up" && rating != "down" {
		return fmt.Errorf("feedback rating %q: want up or down", rating)
	}
	if !l.enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{Event: "feedback", TriggerKey: triggerKey, Kind: rating, User: user, At: at}
	if err := l.appendLocked(e); err != nil {
		return err // append failed: leave the fold untouched (durable-first, like Open/Resolve)
	}
	l.applyFeedbackLocked(e)
	return nil
}

// applyFeedbackLocked folds one feedback event into the per-entry aggregate.
// Attribution goes through the byTrigger index: the vote credits the entry of the
// NEWEST open for its TriggerKey (a fresh investigation attributes nothing — the
// vote is still on disk for analytics, there is just no catalog entry to weigh).
// Dedup: one live vote per (TriggerKey, user), latest wins — a repeated identical
// vote is idempotent, a changed one first un-credits what it previously credited.
// Unlike resolve-based decay, feedback counts regardless of resolvability: a human
// judgment on the diagnosis is exactly the ground truth non-resolvable sources
// (GitOps failures, reinvestigate) can never get from a resolve signal.
func (l *Ledger) applyFeedbackLocked(e Event) {
	if e.TriggerKey == "" || (e.Kind != "up" && e.Kind != "down") {
		return // unattributable, or a malformed replayed line: never folded
	}
	key := e.TriggerKey + "\x00" + e.User
	entry := l.byTrigger[e.TriggerKey].entry
	if prev, ok := l.votes[key]; ok {
		if prev.rating == e.Kind && prev.entry == entry {
			return // duplicate click — idempotent
		}
		l.creditFeedbackLocked(prev.entry, prev.rating, -1)
	}
	l.votes[key] = feedbackVote{rating: e.Kind, entry: entry}
	l.creditFeedbackLocked(entry, e.Kind, +1)
}

// Confirm appends a machine confirmation: a FRESH investigation independently
// reached the same deterministic identity (DupFingerprint) as an existing catalog
// entry — exactly the re-derivation a standing 👎 forces. It is folded as recovery
// evidence into the entry's aggregate (at confirmWeight, see Aggregate.Factor) and
// into the per-trigger confirmation count the Contested curate pass surfaces.
// entry must be non-empty (an unattributable confirm is a caller bug); triggerKey
// may be empty (human `lore investigate` has none — entry credit still applies).
// Recalls must never reach this: only the curator's fingerprint-dedup branch calls
// it, and Curate returns before that branch for recalled findings.
func (l *Ledger) Confirm(entry, triggerKey, dupFP string, at time.Time) error {
	if entry == "" {
		return fmt.Errorf("confirm: empty entry path")
	}
	if !l.enabled() {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := Event{Event: "confirm", Entry: entry, TriggerKey: triggerKey, DupFingerprint: dupFP, At: at}
	if err := l.appendLocked(e); err != nil {
		return err // durable-first: leave the fold untouched, like Open/Feedback
	}
	l.applyConfirmLocked(e)
	return nil
}

// applyConfirmLocked folds one confirm event into the per-entry aggregate and the
// per-trigger index. Must be called with mu held (or during single-threaded load).
func (l *Ledger) applyConfirmLocked(e Event) {
	if e.Entry == "" {
		return // malformed replayed line: never folded
	}
	a := l.agg[e.Entry]
	a.Confirms++
	l.agg[e.Entry] = a
	if e.TriggerKey != "" {
		l.triggerConfirms[e.TriggerKey]++
	}
}

// creditFeedbackLocked adjusts entry's feedback counter for rating by delta; a
// no-op for the empty entry (fresh-investigation votes credit nothing).
func (l *Ledger) creditFeedbackLocked(entry, rating string, delta int) {
	if entry == "" {
		return
	}
	a := l.agg[entry]
	if rating == "up" {
		a.FeedbackUp += delta
	} else {
		a.FeedbackDown += delta
	}
	l.agg[entry] = a
}

// Resolve records that an incident's alert cleared. When it matches an open
// investigation it returns the Episode (with duration + kind) and ok=true.
func (l *Ledger) Resolve(fp string, at time.Time) (Episode, bool, error) {
	if !l.enabled() {
		return Episode{}, false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(Event{Event: "resolve", Fingerprint: fp, At: at}); err != nil {
		return Episode{}, false, err // append failed: leave both index and cache untouched
	}
	l.applyResolveLocked(fp, at) // keep the OpenCounts cache in lockstep with the durable write
	o, ok := l.open[fp]
	if !ok {
		return Episode{}, false, nil
	}
	delete(l.open, fp)
	return Episode{
		Kind: o.Kind, Entry: o.Entry, Title: o.Title, Resource: o.Resource,
		DupFingerprint: o.DupFingerprint,
		OpenedAt:       o.At, ResolvedAt: at, Duration: at.Sub(o.At), Resolved: true,
	}, true, nil
}
