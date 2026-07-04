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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
	Event          string `json:"event"`                     // "open" | "resolve"
	Fingerprint    string `json:"fingerprint"`               // Alertmanager fingerprint (stable firing↔resolved)
	DupFingerprint string `json:"dup_fingerprint,omitempty"` // curator dedup fingerprint (resource+cause); the curated-PR resolution join key

	Kind     string    `json:"kind,omitempty"`  // open: "recall" | "fresh"
	Entry    string    `json:"entry,omitempty"` // open+recall: the recalled entry path
	Title    string    `json:"title,omitempty"`
	Resource string    `json:"resource,omitempty"`
	At       time.Time `json:"at"`

	// Recurrence fields (written by the delivery path). Kept omitempty so the
	// append-only file stays backward/forward compatible with older readers.
	TriggerKey string `json:"trigger_key,omitempty"` // groups recurrences of the same alert; keys the byTrigger index
	CuratedURL string `json:"curated_url,omitempty"` // KB link surfaced as "previous: <link>" on recurrence
	Verdict    string `json:"verdict,omitempty"`     // curator's machine verdict on the investigation

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

	// droppedResolves counts orphan resolves discarded by the pendingResolves bound
	// (see maxPendingResolvesPerFingerprint) — spurious duplicate/replayed resolve
	// webhooks. Kept so the (otherwise silent) defensive drop is observable.
	droppedResolves int

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
	DroppedResolves int                          `json:"dropped_resolves,omitempty"`
}

type triggerAggJSON struct {
	Count      int       `json:"count"`
	Last       time.Time `json:"last"`
	CuratedURL string    `json:"curated_url,omitempty"`
}

type pendingOpenJSON struct {
	Entry   string `json:"entry,omitempty"`
	Counted bool   `json:"counted,omitempty"`
}

// triggerAgg is the per-TriggerKey occurrence roll-up backing Occurrences.
type triggerAgg struct {
	count      int
	last       time.Time
	curatedURL string // CuratedURL of the newest open
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
	l.droppedResolves = 0
}

// foldLocked folds one replayed event into the derived state. Open/resolve maintain the
// aggregate, open-index, pairing stacks, and occurrence index; a checkpoint seeds the
// state a prior compaction folded away. Any other (unknown/future) kind is ignored — the
// property old binaries rely on for the feedback kind, and now for "checkpoint" too.
func (l *Ledger) foldLocked(e Event) {
	switch e.Event {
	case "open":
		l.open[e.Fingerprint] = e
		l.applyOpenLocked(e)
		l.applyTriggerLocked(e)
	case "resolve":
		delete(l.open, e.Fingerprint)
		l.applyResolveLocked(e.Fingerprint, e.At)
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
		l.byTrigger[k] = triggerAgg{count: v.Count, last: v.Last, curatedURL: v.CuratedURL}
	}
	for fp, opens := range cd.PendingOpens {
		stack := make([]pendingOpen, 0, len(opens))
		for _, o := range opens {
			stack = append(stack, pendingOpen{entry: o.Entry, counted: o.Counted})
		}
		l.pendingOpens[fp] = stack
	}
	l.droppedResolves += cd.DroppedResolves
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
	}
	if len(l.byTrigger) > 0 {
		cd.ByTrigger = make(map[string]triggerAggJSON, len(l.byTrigger))
		for k, v := range l.byTrigger {
			cd.ByTrigger[k] = triggerAggJSON{Count: v.count, Last: v.last, CuratedURL: v.curatedURL}
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
	// pair with the earliest such resolve (FIFO), matching Episodes().
	if rs := l.pendingResolves[e.Fingerprint]; len(rs) > 0 {
		at := rs[0]
		l.pendingResolves[e.Fingerprint] = rs[1:]
		if counted {
			l.creditResolveLocked(e.Entry, at)
		}
		return
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
	}
	l.byTrigger[e.TriggerKey] = a
}

// Occurrences reports how many investigations have been recorded for a
// TriggerKey, when the most recent one happened, and its KB link — the
// recurrence facts the notifier renders. Zero values for a disabled ledger,
// an empty key, or a never-seen key.
func (l *Ledger) Occurrences(triggerKey string) (int, time.Time, string) {
	if !l.enabled() || triggerKey == "" {
		return 0, time.Time{}, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.byTrigger[triggerKey]
	return a.count, a.last, a.curatedURL
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
			if rs := pendingResolves[e.Fingerprint]; len(rs) > 0 {
				resolveAt(i, rs[0]) // pair with the earliest buffered (early) resolve
				pendingResolves[e.Fingerprint] = rs[1:]
				continue
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
// recalled, how often the incident then resolved, and when it last resolved.
type Aggregate struct {
	Recalls       int
	Resolved      int
	LastConfirmed time.Time
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
