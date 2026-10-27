// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

const (
	// seenWindow is how long a webhook-id is remembered: it covers every id a fresh
	// (within five minutes) timestamp can carry.
	seenWindow = 10 * time.Minute
	// maxEvents is how many event types a Record keeps.
	maxEvents = 20
	// compactFactor and compactFloor decide when the file is rewritten: once it holds
	// compactFactor times the live lines, and at least compactFloor lines.
	compactFactor = 4
	compactFloor  = 64
)

// Record is what RunLore knows about one incident handed to the factory, keyed by TriggerKey.
type Record struct {
	TriggerKey    string    `json:"trigger_key"`
	Alert         string    `json:"alert,omitempty"`
	Namespace     string    `json:"namespace,omitempty"`
	Workload      string    `json:"workload,omitempty"`
	Task          string    `json:"task,omitempty"`
	Room          string    `json:"room,omitempty"`
	Issue         int       `json:"issue,omitempty"`
	SlackRoot     string    `json:"slack_root,omitempty"`
	SlackChannel  string    `json:"slack_channel,omitempty"`
	FixRoom       string    `json:"fix_room,omitempty"`
	PR            int       `json:"pr,omitempty"`
	MergedAt      time.Time `json:"merged_at,omitzero"`
	FiringAtMerge bool      `json:"firing_at_merge,omitempty"`
	SoakUntil     time.Time `json:"soak_until,omitzero"`
	Verdict       string    `json:"verdict,omitempty"`
	Outcome       string    `json:"outcome,omitempty"`
	Events        []string  `json:"events,omitempty"` // the last 20 event types seen
}

// line is one ledger line: a whole record, or a webhook-id seen at a time.
type line struct {
	Record *Record    `json:"record,omitempty"`
	Seen   string     `json:"seen,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// Ledger is the append-only JSONL file of Records and seen webhook-ids, replayed on open and on
// Reload (each leadership). An empty path makes every method a no-op returning zero values.
type Ledger struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	records map[string]Record
	seen    map[string]time.Time
	lines   int
}

// OpenLedger opens (creating if needed) the ledger at path and replays it.
func OpenLedger(path string, now func() time.Time) (*Ledger, error) {
	l := &Ledger{path: path, now: now}
	if path == "" {
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("factory ledger: %w", err)
	}
	if err := l.Reload(); err != nil {
		return nil, err
	}
	return l, nil
}

// Reload replays the file, replacing the in-memory state: a replica that becomes leader must see
// what another replica appended. A torn line is skipped with a warning and the file is rewritten
// clean, so the next append cannot fuse with the torn bytes.
func (l *Ledger) Reload() error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = map[string]Record{}
	l.seen = map[string]time.Time{}
	l.lines = 0
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("factory ledger: %w", err)
	}
	defer func() { _ = f.Close() }()
	torn := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	cutoff := l.now().Add(-seenWindow)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ln line
		if err := json.Unmarshal(raw, &ln); err != nil || (ln.Record == nil && ln.Seen == "") {
			slog.Warn("factory ledger: skipping an unreadable line", "path", l.path)
			torn = true
			continue
		}
		l.lines++
		switch {
		case ln.Record != nil && ln.Record.TriggerKey != "":
			l.records[ln.Record.TriggerKey] = *ln.Record
		case ln.Seen != "" && ln.At != nil && ln.At.After(cutoff):
			l.seen[ln.Seen] = *ln.At
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("factory ledger: read: %w", err)
	}
	if torn {
		return l.compactLocked()
	}
	return nil
}

// Upsert applies fn to the record of key (a new one when absent) and appends the result.
func (l *Ledger) Upsert(key string, fn func(*Record)) error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.records[key]
	r.Events = slices.Clone(r.Events)
	fn(&r)
	r.TriggerKey = key
	if len(r.Events) > maxEvents {
		r.Events = r.Events[len(r.Events)-maxEvents:]
	}
	if err := l.appendLocked(line{Record: &r}); err != nil {
		return err
	}
	l.records[key] = r
	return l.maybeCompactLocked()
}

// Get returns the record of key.
func (l *Ledger) Get(key string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.records[key]
	return r, ok
}

// ByRoom returns the record whose task room or fix room is room.
func (l *Ledger) ByRoom(room string) (Record, bool) {
	if room == "" {
		return Record{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.Room == room || r.FixRoom == room {
			return r, true
		}
	}
	return Record{}, false
}

// Due returns the records merged, whose soak ended at or before at, with no verdict and no
// terminal outcome.
func (l *Ledger) Due(at time.Time) []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Record
	for _, r := range l.records {
		if !r.MergedAt.IsZero() && !r.SoakUntil.IsZero() && !r.SoakUntil.After(at) && r.Verdict == "" && r.Outcome == "" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Record) int { return a.SoakUntil.Compare(b.SoakUntil) })
	return out
}

// Seen reports whether id was recorded within the last 10 minutes of at; when it was not, it
// records it (persisted) and returns false.
func (l *Ledger) Seen(id string, at time.Time) (bool, error) {
	if l.path == "" {
		return false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.seen[id]; ok && at.Sub(t) < seenWindow {
		return true, nil
	}
	if err := l.appendLocked(line{Seen: id, At: &at}); err != nil {
		return false, err
	}
	l.seen[id] = at
	for k, t := range l.seen {
		if at.Sub(t) >= seenWindow {
			delete(l.seen, k)
		}
	}
	return false, l.maybeCompactLocked()
}

// Register records the Slack root of key's incident thread. It is notify.ThreadSink: best-effort,
// slack only, ignoring an empty key.
func (l *Ledger) Register(transport, root, channel string, inv providers.Investigation) {
	if l == nil || transport != "slack" || inv.TriggerKey == "" {
		return
	}
	if err := l.Upsert(inv.TriggerKey, func(r *Record) { r.SlackRoot, r.SlackChannel = root, channel }); err != nil {
		slog.Warn("factory ledger: recording the Slack root failed", "err", err)
	}
}

// appendLocked appends one line and fsyncs it.
func (l *Ledger) appendLocked(ln line) error {
	b, err := json.Marshal(ln)
	if err != nil {
		return fmt.Errorf("factory ledger: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: operator-configured ledger path
	if err != nil {
		return fmt.Errorf("factory ledger: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("factory ledger: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("factory ledger: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("factory ledger: %w", err)
	}
	l.lines++
	return nil
}

// maybeCompactLocked rewrites the file once it holds compactFactor times the live lines.
func (l *Ledger) maybeCompactLocked() error {
	live := len(l.records) + len(l.seen)
	if l.lines < compactFloor || l.lines <= compactFactor*live {
		return nil
	}
	return l.compactLocked()
}

// compactLocked writes the live lines to a temp file, fsyncs it, renames it over the ledger and
// fsyncs the directory.
func (l *Ledger) compactLocked() error {
	dir := filepath.Dir(l.path)
	tmp, err := os.CreateTemp(dir, ".factory-*.tmp")
	if err != nil {
		return fmt.Errorf("factory ledger: compact: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	w := bufio.NewWriter(tmp)
	n := 0
	write := func(ln line) error {
		b, err := json.Marshal(ln)
		if err != nil {
			return err
		}
		n++
		_, err = w.Write(append(b, '\n'))
		return err
	}
	for _, r := range l.records {
		if err := write(line{Record: &r}); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("factory ledger: compact: %w", err)
		}
	}
	for id, at := range l.seen {
		if err := write(line{Seen: id, At: &at}); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("factory ledger: compact: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("factory ledger: compact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("factory ledger: compact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("factory ledger: compact: %w", err)
	}
	if err := os.Rename(name, l.path); err != nil {
		return fmt.Errorf("factory ledger: compact: %w", err)
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: parent of the operator-configured ledger path
		_ = d.Sync()
		_ = d.Close()
	}
	l.lines = n
	return nil
}
