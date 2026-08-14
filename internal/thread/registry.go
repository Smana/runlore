// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Registry maps a thread root to the investigation it delivered, so a later
// reply can be attributed. It is append-only JSONL on disk, replayed on open —
// the same durability mechanism outcome.Ledger uses, and for the same reason:
// the Slack events endpoint is forwarded to the LEADER, so an in-memory-only
// registry would orphan every live thread on a failover.
//
// It is bounded two ways (a TTL and a max live size) because it is fed by every
// delivery and read by a comparatively rare reply; without bounds it grows for
// the lifetime of the process.
//
// An empty path yields a DISABLED registry: every Put is a no-op and every Get
// misses. Callers never branch on it — a disabled registry simply means thread
// capture is not available, which the adapter reports to the human.
type Registry struct {
	path    string
	ttl     time.Duration
	maxLive int

	mu    sync.Mutex
	byID  map[string]Context
	order []string // insertion order, oldest first; the eviction queue
	now   func() time.Time
}

// NewRegistry opens (replaying) the registry at path. An empty path returns a
// disabled, no-op registry. ttl bounds how long a thread stays answerable; max
// bounds how many live threads are kept.
func NewRegistry(path string, ttl time.Duration, maxLive int) (*Registry, error) {
	r := &Registry{path: path, ttl: ttl, maxLive: maxLive, byID: map[string]Context{}, now: time.Now}
	if path == "" {
		return r, nil
	}
	if err := r.load(); err != nil {
		return nil, fmt.Errorf("thread registry load: %w", err)
	}
	return r, nil
}

// Enabled reports whether the registry persists (a path was configured).
func (r *Registry) Enabled() bool { return r != nil && r.path != "" }

// load replays the JSONL, last-write-wins per root, dropping expired entries. It
// then rewrites the file from the live set when the replay collapsed lines
// (updates, evictions, expiries), which is what keeps the file from growing
// without bound across restarts.
func (r *Registry) load() error {
	f, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing recorded yet
		}
		return err
	}
	lines := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var tc Context
		if err := json.Unmarshal(sc.Bytes(), &tc); err != nil {
			continue // tolerate a torn tail line; the rest of the file is still good
		}
		lines++
		if tc.Root == "" {
			continue
		}
		r.putLocked(tc)
	}
	scanErr := sc.Err()
	_ = f.Close()
	if scanErr != nil {
		return scanErr
	}
	r.expireLocked()
	if lines > len(r.byID) {
		return r.compactLocked()
	}
	return nil
}

// compactLocked rewrites the file from the live set via a temp file + rename, so
// an interrupted compaction leaves the previous file intact.
func (r *Registry) compactLocked() error {
	tmp := r.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: tmp is derived from the operator-configured registry path
	if err != nil {
		return err
	}
	for _, root := range r.order {
		tc, ok := r.byID[root]
		if !ok {
			continue
		}
		b, err := json.Marshal(tc)
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

// putLocked inserts or replaces tc in memory, maintaining the eviction order.
// The caller holds r.mu (or is single-threaded, as load is).
func (r *Registry) putLocked(tc Context) {
	if _, exists := r.byID[tc.Root]; !exists {
		r.order = append(r.order, tc.Root)
	}
	r.byID[tc.Root] = tc
	for r.maxLive > 0 && len(r.byID) > r.maxLive {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.byID, oldest)
	}
}

// expireLocked drops entries older than the TTL.
func (r *Registry) expireLocked() {
	if r.ttl <= 0 {
		return
	}
	cutoff := r.now().Add(-r.ttl)
	kept := r.order[:0]
	for _, root := range r.order {
		tc, ok := r.byID[root]
		if !ok {
			continue
		}
		if !tc.At.IsZero() && tc.At.Before(cutoff) {
			delete(r.byID, root)
			continue
		}
		kept = append(kept, root)
	}
	r.order = kept
}

// appendLocked durably records one entry. fsync'd for the same reason the
// outcome ledger fsyncs: an unclean kill must not lose the record that makes a
// live thread answerable.
func (r *Registry) appendLocked(tc Context) error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := json.Marshal(tc)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// Put records a thread context, replacing any entry with the same Root.
func (r *Registry) Put(tc Context) error {
	if !r.Enabled() || tc.Root == "" {
		return nil
	}
	if tc.At.IsZero() {
		tc.At = r.now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.putLocked(tc)
	return r.appendLocked(tc)
}

// Get returns the context for a thread root. It misses on an unknown root, on a
// disabled registry, and on an entry past the TTL.
func (r *Registry) Get(root string) (Context, bool) {
	if !r.Enabled() || root == "" {
		return Context{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	tc, ok := r.byID[root]
	return tc, ok
}

// Update applies fn to the stored context for root and durably records the
// result. It is how the responder writes back NoteURL and the note counter. A
// miss is not an error: the thread is simply no longer tracked.
func (r *Registry) Update(root string, fn func(*Context)) error {
	if !r.Enabled() || root == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tc, ok := r.byID[root]
	if !ok {
		return nil
	}
	fn(&tc)
	tc.Root = root // fn must not be able to re-key the entry
	r.putLocked(tc)
	return r.appendLocked(tc)
}

// Register records a delivered investigation against the thread root it was
// posted to. It implements notify.ThreadSink, so the Slack notifier can hand
// over the ts it already has without knowing what a registry is. Best-effort by
// contract: a failure here must never affect delivery, so the error is dropped.
func (r *Registry) Register(root, channel string, inv providers.Investigation) {
	if root == "" {
		return
	}
	_ = r.Put(Context{
		Transport:     "slack",
		Root:          root,
		Channel:       channel,
		TriggerKey:    inv.TriggerKey,
		Title:         inv.Title,
		Resource:      inv.Resource.Ref(),
		Verdict:       inv.Verdict,
		CuratedURL:    inv.CuratedURL,
		RecalledEntry: inv.RecalledEntry,
		At:            r.now(),
	})
}
