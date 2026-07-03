// Package outcome records, in an append-only JSONL ledger, whether an
// investigated incident later resolved and which answer was used for it — the
// "did it actually work?" signal the learning loop reads. The ledger keeps an
// in-memory index of still-open incidents, rebuilt by replaying the file on
// startup so a resolve survives a restart / leader failover.
package outcome

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"
)

// Event is one ledger line: an investigation opened, or an incident resolved.
type Event struct {
	Event          string `json:"event"`                     // "open" | "resolve"
	Fingerprint    string `json:"fingerprint"`               // Alertmanager fingerprint (stable firing↔resolved)
	DupFingerprint string `json:"dup_fingerprint,omitempty"` // curator dedup fingerprint (resource+cause); the curated-PR resolution join key

	Kind     string    `json:"kind,omitempty"`  // open: "recall" | "fresh"; feedback: "up" | "down"
	Entry    string    `json:"entry,omitempty"` // open+recall: the recalled entry path
	Title    string    `json:"title,omitempty"`
	Resource string    `json:"resource,omitempty"`
	At       time.Time `json:"at"`

	// Recurrence/feedback fields (written by the delivery path). Kept omitempty so the
	// append-only file stays backward/forward compatible with older readers.
	TriggerKey string `json:"trigger_key,omitempty"` // groups recurrences of the same alert; keys the byTrigger index
	CuratedURL string `json:"curated_url,omitempty"` // KB link surfaced as "previous: <link>" on recurrence
	Verdict    string `json:"verdict,omitempty"`     // curator's machine verdict on the investigation
}

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

// New opens (replaying) the ledger at path. An empty path returns a disabled,
// no-op ledger (the feature is off).
func New(path string) (*Ledger, error) {
	l := &Ledger{path: path}
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
	events, err := l.readEvents()
	if err != nil {
		return err // prior cache untouched
	}
	l.open = map[string]Event{}
	l.agg = map[string]Aggregate{}
	l.pendingOpens = map[string][]pendingOpen{}
	l.pendingResolves = map[string][]time.Time{}
	l.byTrigger = map[string]triggerAgg{}
	l.droppedResolves = 0
	for _, e := range events {
		switch e.Event {
		case "open":
			l.open[e.Fingerprint] = e
			l.applyOpenLocked(e)
			l.applyTriggerLocked(e)
		case "resolve":
			delete(l.open, e.Fingerprint)
			l.applyResolveLocked(e.Fingerprint, e.At)
		}
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
	counted := e.Kind == "recall" && e.Entry != ""
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

// readEvents replays the ledger file in order, skipping corrupt lines. It returns
// a nil slice when the ledger is disabled (path=="") or the file is absent.
func (l *Ledger) readEvents() ([]Event, error) {
	if l.path == "" {
		return nil, nil
	}
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var events []Event
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // skip a corrupt line rather than fail
		}
		events = append(events, e)
	}
	return events, sc.Err()
}

func (l *Ledger) enabled() bool { return l != nil && l.path != "" }

// Enabled reports whether the ledger will actually persist events (a non-empty
// ledger_path was configured); nil-safe. Exported for wiring sites: a disabled
// ledger's methods silently no-op, so handing it to a consumer that acks writes
// (e.g. the server's FeedbackRecorder) would lie about persistence. Cheaper than
// Status(), which re-reads the whole file.
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
	if events, err := l.readEvents(); err == nil {
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

// Feedback appends a human 👍/👎 verdict on a delivered investigation. It is a
// pure append: replay ignores unknown event kinds, so feedback never disturbs
// open/resolve pairing or the recall aggregate.
func (l *Ledger) Feedback(triggerKey, fingerprint, rating string, at time.Time) error {
	if !l.enabled() {
		return nil
	}
	if rating != "up" && rating != "down" {
		return fmt.Errorf("feedback rating %q: want up or down", rating)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(Event{Event: "feedback", TriggerKey: triggerKey, Fingerprint: fingerprint, Kind: rating, At: at})
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
	events, err := l.readEvents()
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
