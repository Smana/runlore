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

	Kind     string    `json:"kind,omitempty"`  // open: "recall" | "fresh"
	Entry    string    `json:"entry,omitempty"` // open+recall: the recalled entry path
	Title    string    `json:"title,omitempty"`
	Resource string    `json:"resource,omitempty"`
	At       time.Time `json:"at"`
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

// Ledger is an append-only outcome log with an in-memory open-index.
type Ledger struct {
	path string
	mu   sync.Mutex
	open map[string]Event // fingerprint → latest unresolved open
}

// New opens (replaying) the ledger at path. An empty path returns a disabled,
// no-op ledger (the feature is off).
func New(path string) (*Ledger, error) {
	l := &Ledger{path: path, open: map[string]Event{}}
	events, err := l.readEvents()
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		switch e.Event {
		case "open":
			l.open[e.Fingerprint] = e
		case "resolve":
			delete(l.open, e.Fingerprint)
		}
	}
	return l, nil
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
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
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
		return err
	}
	l.open[e.Fingerprint] = e
	return nil
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

// OpenCounts rolls Episodes up per catalog entry, counting recall episodes only
// (fresh investigations carry no entry). It is the input to recall decay:
// resolve-rate ≈ (Resolved+k)/(Recalls+k). A disabled/empty ledger yields an
// empty (non-nil) map.
func (l *Ledger) OpenCounts() (map[string]Aggregate, error) {
	eps, err := l.Episodes()
	if err != nil {
		return nil, err
	}
	counts := map[string]Aggregate{}
	for _, e := range eps {
		if e.Kind != "recall" || e.Entry == "" {
			continue
		}
		a := counts[e.Entry]
		a.Recalls++
		if e.Resolved {
			a.Resolved++
			if e.ResolvedAt.After(a.LastConfirmed) {
				a.LastConfirmed = e.ResolvedAt
			}
		}
		counts[e.Entry] = a
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
		return Episode{}, false, err
	}
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
