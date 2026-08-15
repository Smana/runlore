// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// ErrThreadNotTracked is returned by Update when the registry has no entry for
// the requested root. It exists so a caller can tell "this write could not be
// recorded" apart from "recorded" instead of treating both the same way, which
// is what let the per-thread note cap in Responder.Handle go permanently inert
// for a thread the registry had lost (TTL expiry, restart, leader failover,
// eviction at the max-live bound): the counter write-back silently no-op'd
// forever, so it never incremented and the cap never engaged.
var ErrThreadNotTracked = errors.New("thread: root not tracked by the registry")

// ErrThreadNotEstablishable is returned by GetOrCreate when the registry
// cannot record anything at all for root — a disabled registry (no ledger
// path) or an empty root — as distinct from created=false, err=nil, which
// means a CONCURRENT caller already established the entry and this call is
// handing back what they wrote. The two outcomes must never be conflated:
// created=false with a nil error says "someone else did this, use their
// entry"; this error says "no one did, there is nothing to use". A caller
// that receives it must not proceed with the zero-value Context returned
// alongside it as if it were a real, persisted entry.
var ErrThreadNotEstablishable = errors.New("thread: root cannot be established by the registry")

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

	// writeLocks maps a thread root to the guard serialising forge writes for
	// that root — see lockRoot. Guarded by mu, same as byID/order, but never
	// held WHILE a caller is holding the per-root lock itself: mu only
	// protects the brief bookkeeping of acquiring or releasing an entry, so a
	// slow forge round-trip under a root's lock never blocks Get/Put/Update or
	// a different root's writer.
	writeLocks map[string]*rootLock
}

// rootLock is one thread root's write-serialization guard: mu is the actual
// lock a writer holds across its forge round-trip, and refs counts how many
// callers currently hold or are waiting for it. refs is read and written only
// under Registry.mu, so the bookkeeping decision "can this entry be deleted
// now" is never racing the decision "does this entry already exist, reuse
// it" — see lockRoot.
type rootLock struct {
	mu   sync.Mutex
	refs int
}

// lockRoot acquires the write-serialization guard for root and returns a
// release function the caller must call exactly once — typically deferred
// right after acquisition, so a panic or an early return still releases it —
// to unlock and let the entry be reclaimed. root == "" is never serialized
// (Put and Update are already no-ops for it) and returns a no-op release.
//
// This is what closes the residual duplicate-PR race GetOrCreate's doc
// comment describes: two concurrent notes on the same never-before-tracked
// thread both call this before deciding CommentOnPR vs OpenPR. Whichever
// arrives first proceeds immediately; every other caller for the SAME root
// blocks until it releases — not on Registry.mu, which is never held across
// the wait, so Get/Put/Update and a DIFFERENT root's lockRoot are completely
// unaffected. The second caller can then re-read the registry after
// acquiring the lock and see the NoteURL the first caller just wrote back,
// instead of the stale empty value it captured before waiting.
//
// Bounded by concurrency, not by history: an entry exists in writeLocks only
// for a root that has a lock currently held or awaited (refs > 0). The
// moment the last holder releases (refs reaches 0), the entry is deleted —
// under mu, in the same critical section that would otherwise let a
// concurrent new caller observe and reuse it — so a root written to a
// million times sequentially, one write at a time, leaves nothing behind
// once each write completes: at most one entry per root with a write
// IN FLIGHT right now, never one per root ever seen.
func (r *Registry) lockRoot(root string) func() {
	if root == "" {
		return func() {}
	}
	r.mu.Lock()
	if r.writeLocks == nil {
		r.writeLocks = map[string]*rootLock{}
	}
	l, ok := r.writeLocks[root]
	if !ok {
		l = &rootLock{}
		r.writeLocks[root] = l
	}
	l.refs++
	r.mu.Unlock()

	l.mu.Lock()

	released := false
	return func() {
		if released {
			return // defensive: release must run exactly once, but never double-unlock if it somehow doesn't
		}
		released = true
		l.mu.Unlock()
		r.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(r.writeLocks, root)
		}
		r.mu.Unlock()
	}
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
// result. It is how the responder writes back NoteURL and the note counter.
//
// A disabled registry (no ledger path) is a silent no-op, same as before — it
// is not an error case a caller needs to react to, and notify.slack.thread_capture
// already refuses to enable at all without a ledger path, so this branch is
// unreachable in a working deployment. A root the registry does NOT currently
// track is different: it means this write is about to go uncounted, and that
// is exactly the case ErrThreadNotTracked exists to surface rather than hide.
func (r *Registry) Update(root string, fn func(*Context)) error {
	if !r.Enabled() || root == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tc, ok := r.byID[root]
	if !ok {
		return ErrThreadNotTracked
	}
	fn(&tc)
	tc.Root = root // fn must not be able to re-key the entry
	r.putLocked(tc)
	return r.appendLocked(tc)
}

// GetOrCreate returns the tracked context for root, atomically creating and
// persisting one from fallback when the registry has no entry for root yet.
// created reports whether THIS call was the one that established the entry,
// so a caller can log the rehydration once rather than on every subsequent
// hit.
//
// A disabled registry or an empty root can establish nothing at all: that
// case returns ErrThreadNotEstablishable (created=false) rather than the
// same (Context{}, false, nil) a genuine concurrent-loser observes on a
// registry hit. The two must stay distinguishable — created=false with a nil
// error means "a concurrent caller already wrote a real entry, here it is";
// this error means "there is no entry, and none can be made". A caller that
// conflated them would carry a zero-value Context — no Root, no CuratedURL,
// no NoteURL — into a write believing it was the thread's established
// context.
//
// It exists so a caller with a fallback context (Mention.HandleMention, when
// the registry has lost a thread to TTL expiry, a restart, or a leader
// failover) never has to sequence its own Get() and Put(): two of those,
// called from separate goroutines, race — both can miss Get, both then Put
// their own copy of the fallback, and each individual Put is itself atomic
// but the PAIR is not, so two concurrent callers can each walk away believing
// they established "the" entry for the thread. Handle then routes on
// Notes: 0 and NoteURL: "" — state neither caller's copy shares with the
// other's — from two contexts that both look like the thread's one true
// entry, letting both take the OpenPR route and open two standalone PRs for
// one thread. Folding the check and the create into one critical section
// closes that: whichever caller's goroutine reaches the lock first persists
// the entry: every other caller — arriving before or after — observes that
// one persisted entry instead of creating its own.
//
// Closing this does not close every race downstream of it, and this doc
// comment says so on purpose rather than implying otherwise: two concurrent
// callers can still both observe the SAME freshly-created entry with
// NoteURL == "" before either one's forge write returns and updates it — see
// Responder.write, which reads NoteURL to choose CommentOnPR vs OpenPR.
// Closing THAT window would mean holding this method's lock across a network
// round-trip, serialising every note-write this process makes behind one
// HTTP request; that trade is refused deliberately, and the narrower,
// registry-only race is accepted rather than hidden.
func (r *Registry) GetOrCreate(root string, fallback Context) (tc Context, created bool, err error) {
	if !r.Enabled() || root == "" {
		return Context{}, false, ErrThreadNotEstablishable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	if existing, ok := r.byID[root]; ok {
		return existing, false, nil
	}
	fallback.Root = root
	if fallback.At.IsZero() {
		fallback.At = r.now()
	}
	r.putLocked(fallback)
	if err := r.appendLocked(fallback); err != nil {
		return fallback, true, err
	}
	return fallback, true, nil
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
