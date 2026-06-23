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
	Event       string    `json:"event"`           // "open" | "resolve"
	Fingerprint string    `json:"fingerprint"`     // Alertmanager fingerprint (stable firing↔resolved)
	Kind        string    `json:"kind,omitempty"`  // open: "recall" | "fresh"
	Entry       string    `json:"entry,omitempty"` // open+recall: the recalled entry path
	Title       string    `json:"title,omitempty"`
	Resource    string    `json:"resource,omitempty"`
	At          time.Time `json:"at"`
}

// Episode is a matched open→resolve pair (or, from Episodes(), an unresolved open
// when Resolved is false).
type Episode struct {
	Kind, Entry, Title, Resource string
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
		OpenedAt: o.At, ResolvedAt: at, Duration: at.Sub(o.At), Resolved: true,
	}, true, nil
}
