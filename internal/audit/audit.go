// Package audit provides an append-only, tamper-evident record of every action
// the agent attempts — the accountability backbone for the autonomy ladder.
//
// Records are written as JSON lines, each carrying the SHA-256 hash of the
// previous record (a hash chain). Any insertion, deletion, or edit of a past
// record breaks the chain and is detectable via Verify. The chain is seeded on
// open from the last record already on disk, so it survives process restarts.
//
// An autonomous cluster-mutator cannot rely on best-effort stdout logging: this
// is the durable, ordered, complete record of what it did and why.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Decision is the outcome of an action attempt.
type Decision string

// Action-attempt outcomes.
const (
	DecisionExecuted Decision = "executed" // the op was applied to the cluster
	DecisionDryRun   Decision = "dry-run"  // auto dry-run: would have executed
	DecisionSkipped  Decision = "skipped"  // a safety gate withheld it (paused, low confidence, rate-limited…)
	DecisionDenied   Decision = "denied"   // outside the policy envelope
	DecisionFailed   Decision = "failed"   // execution was attempted and errored
)

// Record is one audited action attempt. Hash and PrevHash are filled by Log.
type Record struct {
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor"`            // "auto" | "approve:<user>" | "suggest"
	Op       string    `json:"op"`               // suspend | resume | reconcile | ""
	Target   string    `json:"target"`           // kind/namespace/name
	Decision Decision  `json:"decision"`         // executed | dry-run | skipped | denied | failed
	Reason   string    `json:"reason,omitempty"` // why skipped/denied/failed
	PrevHash string    `json:"prev_hash"`
	Hash     string    `json:"hash"`
}

// Auditor records action attempts. Implementations must be safe for concurrent use.
type Auditor interface {
	Log(r Record) error
}

// Logger is a file-backed, hash-chained Auditor.
type Logger struct {
	mu       sync.Mutex
	w        io.Writer
	closer   io.Closer
	syncFn   func() error // fsync the backing file after each write; nil for NewWriter
	lastHash string
	now      func() time.Time
}

// Open opens (creating if needed) an append-only audit log at path and seeds the
// hash chain from the last record already present.
func Open(path string) (*Logger, error) {
	last, err := lastHash(path)
	if err != nil {
		return nil, fmt.Errorf("audit: read existing chain: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: path is the operator-configured audit log
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &Logger{w: f, closer: f, syncFn: f.Sync, lastHash: last, now: time.Now}, nil
}

// OpenVerified opens the audit log like Open, but first re-walks the existing
// chain and refuses to open a chain whose integrity is broken (insertion, edit,
// or mid-chain deletion). An empty or absent file is a valid (zero-record) chain.
// Callers that must fail closed against an untrustworthy chain (executing action
// modes) should use this instead of Open; the mode-based fail/warn policy lives at
// the call site where the config is available.
//
// It reads the file EXACTLY ONCE: the same scan that verifies the chain also
// yields the trusted seed hash, and that fd is reused as the append handle (seeked
// to end). This closes a TOCTOU window the previous implementation had — it
// verified, closed the fd, then re-read the tail via a second os.Open to seed the
// chaining hash, letting an attacker overwrite the tail in between so the Logger
// chained from a tampered base. Now the seed comes from the verified bytes and the
// handle is never reopened, so there is no verify→append gap.
func OpenVerified(path string) (*Logger, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: path is the operator-configured audit log
	if err != nil {
		return nil, fmt.Errorf("audit: open %s for verify: %w", path, err)
	}
	// Verify and capture the trusted last hash from the SAME read pass.
	last, verr := verifyChain(f)
	if verr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit: existing chain failed verification: %w", verr)
	}
	// Reuse the verified fd as the append handle: seek past the verified tail so the
	// next write lands at end-of-file. Seeding lastHash from the verified scan (not a
	// second os.Open) means an append always chains from bytes we just vouched for.
	// Safety: a one-time seek-to-end is sufficient because Logger is single-writer
	// (mutex-guarded, single process/Logger owner); a second writer or a stray Seek
	// on this fd would silently produce a mid-file write and corrupt the chain.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit: seek to append: %w", err)
	}
	return &Logger{w: f, closer: f, syncFn: f.Sync, lastHash: last, now: time.Now}, nil
}

// NewWriter builds a Logger over an arbitrary writer (tests).
func NewWriter(w io.Writer) *Logger {
	return &Logger{w: w, now: time.Now}
}

// Close closes the underlying file, if any.
func (l *Logger) Close() error {
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}

// Log appends a record, chaining it to the previous one. Safe for concurrent use.
func (l *Logger) Log(r Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Time.IsZero() {
		r.Time = l.now().UTC()
	}
	r.PrevHash = l.lastHash
	r.Hash = "" // never hash over a prior hash value
	r.Hash = hashRecord(r)
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: marshal: %w", err)
	}
	if _, err := l.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("audit: write: %w", err)
	}
	// fsync so the tail of the tamper-evident chain survives an unclean crash.
	// nil for NewWriter (an arbitrary writer can't be synced).
	if l.syncFn != nil {
		if err := l.syncFn(); err != nil {
			return fmt.Errorf("audit: sync: %w", err)
		}
	}
	l.lastHash = r.Hash
	return nil
}

// hashRecord computes the chain hash over the record's content + PrevHash. The
// Hash field is excluded (cleared by the caller before hashing).
func hashRecord(r Record) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// lastHash returns the Hash of the last record in the file, or "" if the file is
// absent or empty.
func lastHash(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the operator-configured audit log
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var last string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			var r Record
			if err := json.Unmarshal([]byte(line), &r); err == nil && r.Hash != "" {
				last = r.Hash
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return last, nil
}

// Verify re-walks a chain and reports the first broken link, if any.
func Verify(r io.Reader) error {
	_, err := verifyChain(r)
	return err
}

// verifyChain re-walks a chain in a single pass, returning the last record's hash
// (the trusted seed for a subsequent append) alongside the first broken-link
// error, if any. Both the public Verify and OpenVerified share it; OpenVerified
// uses the returned hash to seed its Logger from the very bytes it just verified,
// so the file is read exactly once and there is no verify→append re-read window.
func verifyChain(r io.Reader) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	prev := ""
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		n++
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return "", fmt.Errorf("audit: record %d: %w", n, err)
		}
		if rec.PrevHash != prev {
			return "", fmt.Errorf("audit: record %d: prev_hash mismatch (chain broken)", n)
		}
		if hashRecord(rec) != rec.Hash {
			return "", fmt.Errorf("audit: record %d: hash mismatch (record tampered)", n)
		}
		prev = rec.Hash
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return prev, nil
}

// Nop is an Auditor that drops records (local/no-audit fallback).
type Nop struct{}

// Log implements Auditor.
func (Nop) Log(Record) error { return nil }
