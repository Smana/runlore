# Thread Interaction PR1 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A human replying `@runlore note: <text>` in a Slack investigation thread gets that text written into the knowledge base — as a comment on the finding's open KB PR, or as a new `Concept` entry PR when there is none.

**Architecture:** A transport-agnostic core (`internal/thread`) parses the message, decides where the knowledge lands, and writes it through the existing `providers.CurationForge`. Slack is one adapter: a new signature-verified `POST /slack/events` endpoint feeds the core, and a persisted registry answers "which investigation is this thread about" (Slack's `app_mention` payload carries only the thread id). No model is involved anywhere in this PR.

**Tech Stack:** Go 1.26, stdlib only. Existing internals: `internal/providers` (contracts), `internal/server` (HTTP + Slack signature verification + leader forwarding), `internal/notify` (Slack delivery), `internal/ratelimit` (sliding window), `internal/kbvalidate` (KB merge gate), `internal/okf` (OKF rendering).

Source spec: `dev/superpowers/specs/2026-08-14-thread-interaction-design.md`.

## Global Constraints

- **Quality gate, run before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`. `gofmt -l .` must print nothing; golangci-lint must report `0 issues`.
- **TDD.** Failing test first, then the minimal implementation. Prefer table-driven tests. Tests verify behaviour, not mocks.
- **Every file starts with** `// SPDX-License-Identifier: Apache-2.0`.
- **Every exported symbol carries a doc comment** (enforced by `revive`).
- **Errors** wrap with `%w`; compare with `errors.Is` / `errors.As` (enforced by `errorlint`).
- **`context.Context` is the first parameter** of any function that does I/O.
- Module path `github.com/Smana/runlore`.
- Nothing in this PR may block or fail investigation delivery. Every new call site is best-effort relative to its host path.
- No new third-party dependencies.
- Defaults for the two capture flags are `false`.

---

## File Structure

**Create — `internal/thread/` (the transport-agnostic core):**

| File | Responsibility |
|---|---|
| `thread.go` | `Context` — the transport-agnostic "which investigation is this thread" value. |
| `registry.go` | Persisted, bounded, TTL'd `Root → Context` store. Implements `notify.ThreadSink`. |
| `grammar.go` | Mention stripping and prefix parsing. Pure functions. |
| `note.go` | Provenance-header rendering and `Concept` `KBEntry` construction. Pure functions. |
| `responder.go` | `Handle` — grammar → routing → forge write → reply text. |
| `mention.go` | `Mention` — registry lookup + responder + reply. Satisfies `server.ThreadHandler`. |

Plus `_test.go` alongside each.

**Modify:**

| File | Change |
|---|---|
| `internal/providers/providers.go` | Add `ThreadNotifier` capability interface. |
| `internal/notify/registry.go` | Add `Threads` to `Deps`. |
| `internal/notify/slack.go` | `ThreadSink` interface; `SlackBot.Threads` field; register the root after the summary post; `SlackBot.ReplyInThread`; `Multi.ThreadReplier`. |
| `internal/config/config.go` | `SlackNotify.ThreadCapture` + `Validate` rule. |
| `internal/server/server.go` | `ThreadHandler` interface, `Actions.Threads`, `POST /slack/events`. |
| `internal/app/notify.go` | `BuildThreadRegistry`, `ThreadCaptureDeliverable`. |
| `internal/app/serve.go` | Wiring + startup log. |
| `docs/` | Slack setup (scope + Request URL) and the thread grammar. |

---

### Task 1: `thread.Context` and the persisted registry

**Files:**
- Create: `internal/thread/thread.go`
- Create: `internal/thread/registry.go`
- Test: `internal/thread/registry_test.go`

**Interfaces:**
- Consumes: `providers.Investigation`, `providers.Verdict`, `providers.Workload.Ref()`.
- Produces:
  - `type Context struct` with fields `Transport, Root, Channel, TriggerKey, DupFingerprint, Title, Resource string; Verdict providers.Verdict; CuratedURL, RecalledEntry, NoteURL string; Notes int; At time.Time`
  - `func NewRegistry(path string, ttl time.Duration, max int) (*Registry, error)`
  - `func (r *Registry) Enabled() bool`
  - `func (r *Registry) Put(tc Context) error`
  - `func (r *Registry) Get(root string) (Context, bool)`
  - `func (r *Registry) Update(root string, fn func(*Context)) error`
  - `func (r *Registry) Register(root, channel string, inv providers.Investigation)`

- [ ] **Step 1: Write the failing test**

Create `internal/thread/registry_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestRegistryPutGet(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tc := Context{Transport: "slack", Root: "111.222", Channel: "C1", TriggerKey: "tk", Title: "OOMKilled"}
	if err := r.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := r.Get("111.222")
	if !ok {
		t.Fatal("Get: want hit, got miss")
	}
	if got.TriggerKey != "tk" || got.Title != "OOMKilled" {
		t.Fatalf("Get = %+v, want TriggerKey=tk Title=OOMKilled", got)
	}
	if _, ok := r.Get("nope"); ok {
		t.Fatal("Get(nope): want miss, got hit")
	}
}

func TestRegistryDisabledWhenPathEmpty(t *testing.T) {
	r, err := NewRegistry("", time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if r.Enabled() {
		t.Fatal("empty path must yield a disabled registry")
	}
	if err := r.Put(Context{Root: "1"}); err != nil {
		t.Fatalf("Put on disabled registry must be a no-op, got %v", err)
	}
	if _, ok := r.Get("1"); ok {
		t.Fatal("disabled registry must never return a hit")
	}
}

func TestRegistryReplayAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.jsonl")
	r1, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r1.Put(Context{Root: "111.222", TriggerKey: "tk", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := r1.Update("111.222", func(c *Context) { c.Notes = 3 }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	r2, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := r2.Get("111.222")
	if !ok {
		t.Fatal("replay lost the entry")
	}
	if got.Notes != 3 {
		t.Fatalf("Notes = %d, want 3 (last write wins on replay)", got.Notes)
	}
	if got.CuratedURL != "https://github.com/o/r/pull/42" {
		t.Fatalf("CuratedURL = %q, want it preserved through Update", got.CuratedURL)
	}
}

func TestRegistryTTLExpiry(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	now := time.Now()
	r.now = func() time.Time { return now }
	if err := r.Put(Context{Root: "old"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	r.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, ok := r.Get("old"); ok {
		t.Fatal("an entry past the TTL must not be returned")
	}
}

func TestRegistryBoundEvictsOldest(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 2)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for _, root := range []string{"a", "b", "c"} {
		if err := r.Put(Context{Root: root}); err != nil {
			t.Fatalf("Put(%s): %v", root, err)
		}
	}
	if _, ok := r.Get("a"); ok {
		t.Fatal("oldest entry must be evicted at the bound")
	}
	if _, ok := r.Get("c"); !ok {
		t.Fatal("newest entry must survive")
	}
}

func TestRegistryRegisterFromInvestigation(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	inv := providers.Investigation{
		Title:         "ImageGalleryUnavailable",
		TriggerKey:    "tk-1",
		Verdict:       providers.VerdictActionRequired,
		CuratedURL:    "https://github.com/o/r/pull/42",
		RecalledEntry: "incidents/foo.md",
		Resource:      providers.Workload{Kind: "Deployment", Name: "gallery", Namespace: "apps"},
	}
	r.Register("111.222", "C1", inv)

	got, ok := r.Get("111.222")
	if !ok {
		t.Fatal("Register did not store the thread")
	}
	if got.Transport != "slack" {
		t.Fatalf("Transport = %q, want slack", got.Transport)
	}
	if got.Resource != "apps/gallery" {
		t.Fatalf("Resource = %q, want apps/gallery", got.Resource)
	}
	if got.TriggerKey != "tk-1" || got.CuratedURL != "https://github.com/o/r/pull/42" || got.RecalledEntry != "incidents/foo.md" {
		t.Fatalf("Register lost fields: %+v", got)
	}
}

func TestRegistryRegisterIgnoresEmptyRoot(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.Register("", "C1", providers.Investigation{Title: "x"})
	if _, ok := r.Get(""); ok {
		t.Fatal("an empty root must never be stored")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestRegistry -v`
Expected: FAIL — the package does not exist (`no Go files in .../internal/thread`).

- [ ] **Step 3: Write `thread.go`**

Create `internal/thread/thread.go`:

```go
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
```

- [ ] **Step 4: Write `registry.go`**

Create `internal/thread/registry.go`:

```go
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
	path string
	ttl  time.Duration
	max  int

	mu    sync.Mutex
	byID  map[string]Context
	order []string // insertion order, oldest first; the eviction queue
	now   func() time.Time
}

// NewRegistry opens (replaying) the registry at path. An empty path returns a
// disabled, no-op registry. ttl bounds how long a thread stays answerable; max
// bounds how many live threads are kept.
func NewRegistry(path string, ttl time.Duration, max int) (*Registry, error) {
	r := &Registry{path: path, ttl: ttl, max: max, byID: map[string]Context{}, now: time.Now}
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
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
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
	for r.max > 0 && len(r.byID) > r.max {
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
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/thread/ -run TestRegistry -v`
Expected: PASS — all seven tests.

- [ ] **Step 6: Run the quality gate**

Run: `go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/thread/`
Expected: no output from `gofmt -l .`; `0 issues` from golangci-lint.

- [ ] **Step 7: Commit**

```bash
git add internal/thread/thread.go internal/thread/registry.go internal/thread/registry_test.go
git commit -m "feat(thread): persisted thread-context registry

Maps a chat thread root to the investigation delivered into it, so a later
reply can be attributed. Append-only JSONL replayed on open — the same
mechanism the outcome ledger uses, and for the same reason: the Slack events
endpoint is leader-forwarded, so in-memory-only state would orphan every live
thread on a failover. Bounded by TTL and size; an empty path disables it."
```

---

### Task 2: Grammar — mention stripping and prefix parsing

**Files:**
- Create: `internal/thread/grammar.go`
- Test: `internal/thread/grammar_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Intent int` with constants `IntentNote`, `IntentFreeform`, `IntentReinvestigate`
  - `type Parsed struct { Intent Intent; Text string }`
  - `func Parse(raw string) Parsed`

- [ ] **Step 1: Write the failing test**

Create `internal/thread/grammar_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import "testing"

func TestParse(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantIntent Intent
		wantText   string
	}{
		{"note prefix", "<@U0BOT> note: the real cause was a spot reclaim", IntentNote, "the real cause was a spot reclaim"},
		{"note prefix uppercase", "<@U0BOT> NOTE: spot reclaim", IntentNote, "spot reclaim"},
		{"note prefix no space after colon", "<@U0BOT> note:spot reclaim", IntentNote, "spot reclaim"},
		{"mention with display name", "<@U0BOT|runlore> note: x", IntentNote, "x"},
		{"no mention at all", "note: x", IntentNote, "x"},
		{"multiple leading mentions", "<@U0BOT> <@U0HUMAN> note: x", IntentNote, "x"},
		{"freeform", "<@U0BOT> did you check the NetworkPolicies?", IntentFreeform, "did you check the NetworkPolicies?"},
		{"reinvestigate reserved", "<@U0BOT> reinvestigate: look at the CNI", IntentReinvestigate, "look at the CNI"},
		{"reinvestigate bare", "<@U0BOT> reinvestigate:", IntentReinvestigate, ""},
		{"empty after mention", "<@U0BOT>", IntentFreeform, ""},
		{"whitespace only", "<@U0BOT>    ", IntentFreeform, ""},
		{"newlines preserved inside the note", "<@U0BOT> note: line one\nline two", IntentNote, "line one\nline two"},
		{"colon in freeform is not a prefix", "<@U0BOT> why: did it fail", IntentFreeform, "why: did it fail"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.raw)
			if got.Intent != tc.wantIntent {
				t.Errorf("Intent = %v, want %v", got.Intent, tc.wantIntent)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text = %q, want %q", got.Text, tc.wantText)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestParse -v`
Expected: FAIL — `undefined: Parse`, `undefined: Intent`.

- [ ] **Step 3: Write the implementation**

Create `internal/thread/grammar.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import "strings"

// Intent is what a human addressed to RunLore in a thread asked for.
type Intent int

const (
	// IntentFreeform is anything without a recognised prefix — a question, or a
	// statement RunLore is left to interpret.
	IntentFreeform Intent = iota
	// IntentNote is an explicit "note:" — capture this verbatim into the KB.
	IntentNote
	// IntentReinvestigate is the RESERVED "reinvestigate:" prefix. It is parsed so
	// the grammar is stable, but deliberately not implemented: re-running costs a
	// full investigation and the capability already exists behind the
	// `reinvestigate` forge label. Reserving it now makes adding it later a
	// handler case rather than a grammar migration.
	IntentReinvestigate
)

// String renders the intent for logs and metrics.
func (i Intent) String() string {
	switch i {
	case IntentNote:
		return "note"
	case IntentReinvestigate:
		return "reinvestigate"
	default:
		return "freeform"
	}
}

// Parsed is one addressed message, split into what was asked and the text.
type Parsed struct {
	Intent Intent
	Text   string
}

// prefixes maps a recognised command prefix to its intent. Matched
// case-insensitively against the text remaining after leading mentions.
var prefixes = []struct {
	prefix string
	intent Intent
}{
	{"note:", IntentNote},
	{"reinvestigate:", IntentReinvestigate},
}

// Parse strips leading chat mentions and classifies the remainder.
//
// Mentions are stripped generically (any leading `<@…>` token) rather than
// matched against the bot's own id: the message reached us because the
// transport already decided we were addressed, so re-deriving that here would
// duplicate the decision and need a `auth.test` round-trip to learn our own id.
func Parse(raw string) Parsed {
	s := stripLeadingMentions(raw)
	lower := strings.ToLower(s)
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p.prefix) {
			return Parsed{Intent: p.intent, Text: strings.TrimSpace(s[len(p.prefix):])}
		}
	}
	return Parsed{Intent: IntentFreeform, Text: s}
}

// stripLeadingMentions removes every leading `<@…>` token (with the surrounding
// whitespace) and trims the result.
func stripLeadingMentions(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "<@") {
		end := strings.IndexByte(s, '>')
		if end < 0 {
			break
		}
		s = strings.TrimSpace(s[end+1:])
	}
	return strings.TrimSpace(s)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/thread/ -run TestParse -v`
Expected: PASS — all 13 subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/thread/grammar.go internal/thread/grammar_test.go
git commit -m "feat(thread): parse the addressed-message grammar

Strips leading chat mentions and recognises the note: prefix. reinvestigate:
is parsed but reserved — stabilising the grammar now means implementing it
later is a handler case, not a migration."
```

---

### Task 3: Note rendering and the `Concept` entry

**Files:**
- Create: `internal/thread/note.go`
- Test: `internal/thread/note_test.go`

**Interfaces:**
- Consumes: `Context` (Task 1).
- Produces:
  - `func NoteBody(tc Context, author, text string, at time.Time) string`
  - `func ConceptEntry(tc Context, author, text string, at time.Time) providers.KBEntry`

- [ ] **Step 1: Write the failing test**

Create `internal/thread/note_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/kbvalidate"
)

var noteAt = time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC)

func TestNoteBodyCarriesProvenanceAndVerbatimText(t *testing.T) {
	tc := Context{Transport: "slack", Title: "ImageGalleryUnavailable", TriggerKey: "tk-1"}
	got := NoteBody(tc, "alice", "the real cause was a spot reclaim", noteAt)

	for _, want := range []string{"alice", "slack", "2026-08-14", "the real cause was a spot reclaim"} {
		if !strings.Contains(got, want) {
			t.Errorf("NoteBody missing %q:\n%s", want, got)
		}
	}
}

func TestNoteBodyNeutralisesImageMarkdown(t *testing.T) {
	got := NoteBody(Context{}, "alice", "look ![x](https://evil.example/track.png) here", noteAt)
	if strings.Contains(got, "![") {
		t.Fatalf("image markdown must be neutralised:\n%s", got)
	}
	if !strings.Contains(got, "https://evil.example/track.png") {
		t.Fatal("the URL must survive as text — neutralised, not censored")
	}
}

func TestConceptEntryPassesTheMergeGate(t *testing.T) {
	tests := []struct {
		name string
		tc   Context
	}{
		{"full context", Context{Title: "ImageGalleryUnavailable", Resource: "apps/gallery", TriggerKey: "tk-1", RecalledEntry: "incidents/foo.md"}},
		{"no resource", Context{Title: "ImageGalleryUnavailable", TriggerKey: "tk-1"}},
		{"no title", Context{TriggerKey: "tk-1"}},
		{"empty context", Context{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ConceptEntry(tt.tc, "alice", "the real cause was a spot reclaim", noteAt)
			if e.Type != "Concept" {
				t.Fatalf("Type = %q, want Concept", e.Type)
			}
			issues := kbvalidate.ValidateStructural(catalog.Entry{
				Type: e.Type, Title: e.Title, Description: e.Description,
				Resource: e.Resource, Tags: e.Tags, Body: e.Body,
			})
			for _, is := range issues {
				if is.Severity == kbvalidate.SeverityError {
					t.Errorf("entry fails the merge gate: %s: %s", is.Field, is.Message)
				}
			}
		})
	}
}

func TestConceptEntryLinksTheRecalledEntry(t *testing.T) {
	tc := Context{Title: "ImageGalleryUnavailable", RecalledEntry: "incidents/foo.md"}
	e := ConceptEntry(tc, "alice", "this resolution is stale", noteAt)
	if !strings.Contains(e.Body, "incidents/foo.md") {
		t.Fatalf("body must link the entry the note corrects:\n%s", e.Body)
	}
}

func TestConceptEntryCarriesTriggerKeyNotFingerprint(t *testing.T) {
	// The dedup fingerprint identifies a CURATED FINDING. An operator note is not
	// a finding, so stamping it would make the note collide with the real entry in
	// curator dedup and in ByFingerprint lookups.
	e := ConceptEntry(Context{TriggerKey: "tk-1", DupFingerprint: "fp-1"}, "alice", "x", noteAt)
	if e.Fingerprint != "" {
		t.Fatalf("Fingerprint = %q, want empty on an operator note", e.Fingerprint)
	}
	if e.Confidence != 0 {
		t.Fatalf("Confidence = %v, want 0 — a note carries no model confidence", e.Confidence)
	}
}

func TestConceptEntryTitleIsBounded(t *testing.T) {
	e := ConceptEntry(Context{Title: strings.Repeat("x", 400)}, "alice", "y", noteAt)
	issues := kbvalidate.ValidateStructural(catalog.Entry{
		Type: e.Type, Title: e.Title, Description: e.Description, Body: e.Body,
	})
	for _, is := range issues {
		if is.Severity == kbvalidate.SeverityError {
			t.Errorf("long source title must not break the gate: %s: %s", is.Field, is.Message)
		}
	}
}
```

The test imports `catalog` and `kbvalidate` but **not** `providers` — no test here names a `providers` type. Do not add a blank identifier to keep an unused import alive; drop the import.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run 'TestNote|TestConcept' -v`
Expected: FAIL — `undefined: NoteBody`, `undefined: ConceptEntry`.

- [ ] **Step 3: Write the implementation**

(`kbvalidate.Issue` is `{Severity Severity; Field, Message string}` with `SeverityError`/`SeverityWarning` — verified; the Step 1 test matches it.)

Create `internal/thread/note.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"fmt"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// maxNoteTitle bounds a generated entry title well inside the validator's own
// limit, which counts BYTES — an accented title hits it at roughly half as many
// characters.
const maxNoteTitle = 90

// NoteBody renders a human's thread reply as a KB-bound note: a provenance
// header naming who said it, where and when, followed by their words verbatim.
//
// Verbatim is the point. The human's exact wording is the evidence a reviewer
// weighs; anything that rewrites it makes the note less trustworthy than the
// Slack message it came from.
func NoteBody(tc Context, author, text string, at time.Time) string {
	var b strings.Builder
	b.WriteString("### 📝 Operator note\n\n")
	b.WriteString(fmt.Sprintf("From **@%s** via %s on %s.\n",
		author, transportName(tc.Transport), at.UTC().Format(time.RFC3339)))
	if tc.Title != "" {
		b.WriteString(fmt.Sprintf("Thread: %s\n", tc.Title))
	}
	b.WriteString("\n")
	b.WriteString(neutralizeImages(text))
	b.WriteString("\n")
	return b.String()
}

// ConceptEntry builds the standalone KB entry for a note that has no open PR to
// land on — a recall (which never curates), a skipped verdict, or a coalesced
// finding.
//
// The type is Concept, not Incident, deliberately: kbvalidate requires the
// Symptom/Cause/Resolution body sections and a `resource` for Incident only. A
// bare operator note has neither, so typing it Concept clears the merge gate
// honestly instead of fabricating evidence sections nobody wrote.
func ConceptEntry(tc Context, author, text string, at time.Time) providers.KBEntry {
	title := "Operator note"
	if tc.Title != "" {
		title = "Operator note: " + tc.Title
	}
	title = truncate(title, maxNoteTitle)

	var body strings.Builder
	body.WriteString(NoteBody(tc, author, text, at))
	if tc.RecalledEntry != "" || tc.TriggerKey != "" || tc.Resource != "" {
		body.WriteString("\n### Context\n\n")
		if tc.RecalledEntry != "" {
			body.WriteString(fmt.Sprintf("- Corrects or extends: `%s`\n", tc.RecalledEntry))
		}
		if tc.Resource != "" {
			body.WriteString(fmt.Sprintf("- Resource: `%s`\n", tc.Resource))
		}
		if tc.TriggerKey != "" {
			body.WriteString(fmt.Sprintf("- Trigger key: `%s`\n", tc.TriggerKey))
		}
		if tc.Verdict != "" {
			body.WriteString(fmt.Sprintf("- Verdict at delivery: `%s`\n", tc.Verdict))
		}
	}

	return providers.KBEntry{
		Type:        "Concept",
		Title:       title,
		Description: truncate(fmt.Sprintf("Operator knowledge captured from a %s thread by @%s.", transportName(tc.Transport), author), maxNoteTitle*2),
		Resource:    tc.Resource,
		Tags:        []string{"operator-note", transportName(tc.Transport)},
		Body:        body.String(),
		// Fingerprint, Confidence and Provenance are deliberately unset: the dedup
		// fingerprint identifies a CURATED FINDING, and stamping a note with one
		// would collide it with the real entry in curator dedup and ByFingerprint.
	}
}

// transportName normalises an empty transport so rendering never emits "via ".
func transportName(t string) string {
	if t == "" {
		return "chat"
	}
	return t
}

// neutralizeImages defuses markdown image syntax so a note cannot embed a remote
// image (a tracking pixel, or a request from the reviewer's browser) into a PR
// body. The URL survives as ordinary text — a reviewer must still be able to see
// what was linked.
func neutralizeImages(s string) string {
	return strings.ReplaceAll(s, "![", "!&#91;")
}

// truncate shortens s to at most n BYTES, on a rune boundary, appending an
// ellipsis when it cuts. Bytes, because that is what the validator's limit
// counts.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 1
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// isRuneStart reports whether b begins a UTF-8 rune (i.e. is not a continuation
// byte).
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/thread/ -run 'TestNote|TestConcept' -v`
Expected: PASS — all six tests, including every subtest of `TestConceptEntryPassesTheMergeGate`.

- [ ] **Step 5: Run the quality gate**

Run: `go build ./... && go vet ./... && go test ./internal/thread/ && gofmt -l . && golangci-lint run ./internal/thread/`
Expected: tests PASS; `gofmt -l .` silent; `0 issues`.

- [ ] **Step 6: Commit**

```bash
git add internal/thread/note.go internal/thread/note_test.go
git commit -m "feat(thread): render operator notes and standalone Concept entries

A note keeps the human's wording verbatim under a provenance header. When a
thread has no open KB PR to comment on, the note becomes a Concept entry —
kbvalidate requires Symptom/Cause/Resolution and a resource for Incident only,
so Concept clears the merge gate without fabricating evidence sections.

The entry carries no dedup fingerprint: that identifies a curated finding, and
stamping a note with one would collide it with the real entry."
```

---

### Task 4: The responder — routing and rate limits

**Files:**
- Create: `internal/thread/responder.go`
- Test: `internal/thread/responder_test.go`

**Interfaces:**
- Consumes: `Context`, `Registry` (Task 1); `Parse`, `Intent*` (Task 2); `NoteBody`, `ConceptEntry` (Task 3).
- Produces:
  - `type Forge interface { CommentOnPR(ctx context.Context, number int, body string) error; OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error) }`
  - `type Responder struct { Forge Forge; Registry *Registry; MaxNotesPerThread int; OpenPRs *ratelimit.Window; Now func() time.Time; Log *slog.Logger }`
  - `func (r *Responder) Handle(ctx context.Context, tc Context, author, raw string) (string, error)`
  - `func PRNumber(rawURL string) (int, bool)`

- [ ] **Step 1: Write the failing test**

Create `internal/thread/responder_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"errors"
	"log/slog"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
)

type fakeForge struct {
	comments []struct {
		number int
		body   string
	}
	opened   []providers.KBEntry
	openURL  string
	openErr  error
	commErr  error
}

func (f *fakeForge) CommentOnPR(_ context.Context, number int, body string) error {
	if f.commErr != nil {
		return f.commErr
	}
	f.comments = append(f.comments, struct {
		number int
		body   string
	}{number, body})
	return nil
}

func (f *fakeForge) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	if f.openErr != nil {
		return providers.Ref{}, f.openErr
	}
	f.opened = append(f.opened, e)
	url := f.openURL
	if url == "" {
		url = "https://github.com/o/r/pull/99"
	}
	return providers.Ref{URL: url}, nil
}

func newTestResponder(t *testing.T, f *fakeForge) *Responder {
	t.Helper()
	reg, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return &Responder{
		Forge:             f,
		Registry:          reg,
		MaxNotesPerThread: 3,
		OpenPRs:           ratelimit.New(10, time.Hour),
		Now:               func() time.Time { return noteAt },
		Log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestPRNumber(t *testing.T) {
	tests := []struct {
		url  string
		want int
		ok   bool
	}{
		{"https://github.com/o/r/pull/42", 42, true},
		{"https://gitlab.com/o/r/-/merge_requests/7", 7, true},
		{"https://gitlab.example.com/grp/sub/proj/-/merge_requests/1234", 1234, true},
		{"https://github.com/o/r/pull/42#issuecomment-1", 42, true},
		{"https://github.com/o/r/issues/42", 0, false},
		{"", 0, false},
		{"not a url", 0, false},
		{"https://github.com/o/r/pull/notanumber", 0, false},
	}
	for _, tt := range tests {
		got, ok := PRNumber(tt.url)
		if got != tt.want || ok != tt.ok {
			t.Errorf("PRNumber(%q) = (%d, %v), want (%d, %v)", tt.url, got, ok, tt.want, tt.ok)
		}
	}
}

func TestHandleNoteCommentsOnTheOpenPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: spot reclaim, not OOM")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if f.comments[0].number != 42 {
		t.Errorf("commented on PR %d, want 42", f.comments[0].number)
	}
	if !strings.Contains(f.comments[0].body, "spot reclaim, not OOM") {
		t.Errorf("comment lost the note text: %s", f.comments[0].body)
	}
	if len(f.opened) != 0 {
		t.Errorf("must not open a PR when one is already linked; opened %d", len(f.opened))
	}
	if !strings.Contains(reply, "42") {
		t.Errorf("reply must point at the PR it wrote to: %q", reply)
	}
}

func TestHandleNoteOpensStandalonePRWhenNoneLinked(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM", RecalledEntry: "incidents/foo.md"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note: stale since Karpenter")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1", len(f.opened))
	}
	if f.opened[0].Type != "Concept" {
		t.Errorf("Type = %q, want Concept", f.opened[0].Type)
	}
	if !strings.Contains(reply, "99") {
		t.Errorf("reply must name the PR it opened: %q", reply)
	}
	stored, ok := r.Registry.Get("111.222")
	if !ok {
		t.Fatal("registry lost the thread")
	}
	if stored.NoteURL == "" {
		t.Error("NoteURL must be written back so the next note comments instead of opening again")
	}
}

func TestHandleSecondNoteCommentsOnTheFirstNotesPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", Title: "OOM"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: first"); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	refreshed, _ := r.Registry.Get("111.222")
	if _, err := r.Handle(context.Background(), refreshed, "bob", "note: second"); err != nil {
		t.Fatalf("second Handle: %v", err)
	}

	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want exactly 1 — a thread opens at most one standalone PR", len(f.opened))
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1 (the second note)", len(f.comments))
	}
	if f.comments[0].number != 99 {
		t.Errorf("second note went to PR %d, want 99 (the first note's PR)", f.comments[0].number)
	}
}

func TestHandlePerThreadCap(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	var lastReply string
	for i := 0; i < 5; i++ {
		cur, _ := r.Registry.Get("111.222")
		var err error
		lastReply, err = r.Handle(context.Background(), cur, "alice", "note: spam")
		if err != nil {
			t.Fatalf("Handle %d: %v", i, err)
		}
	}
	if len(f.comments) != 3 {
		t.Fatalf("comments = %d, want 3 (MaxNotesPerThread)", len(f.comments))
	}
	if !strings.Contains(strings.ToLower(lastReply), "limit") {
		t.Errorf("the capped reply must say so: %q", lastReply)
	}
}

func TestHandleOpenPRRateLimit(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	r.OpenPRs = ratelimit.New(1, time.Hour)

	for _, root := range []string{"a", "b"} {
		tc := Context{Root: root}
		if err := r.Registry.Put(tc); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	if len(f.opened) != 1 {
		t.Fatalf("opened = %d, want 1 — the global OpenPR budget caps the second", len(f.opened))
	}
}

func TestHandleFreeformIsCapturedWhenNoModelIsWired(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> the cause was a spot reclaim")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 1 {
		t.Fatalf("freeform must still be captured; comments = %d", len(f.comments))
	}
	if !strings.Contains(strings.ToLower(reply), "note:") {
		t.Errorf("the reply should teach the explicit prefix: %q", reply)
	}
}

func TestHandleReinvestigateIsReservedNotImplemented(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> reinvestigate: check the CNI")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatal("reinvestigate must write nothing to the KB")
	}
	if !strings.Contains(strings.ToLower(reply), "not supported") {
		t.Errorf("the reply must say it is unsupported: %q", reply)
	}
}

func TestHandleEmptyTextAsksForContent(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}

	reply, err := r.Handle(context.Background(), tc, "alice", "<@U0BOT> note:   ")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.comments) != 0 {
		t.Fatal("an empty note must write nothing")
	}
	if reply == "" {
		t.Fatal("an empty note must still get a reply")
	}
}

func TestHandleForgeFailureIsReportedNotSwallowed(t *testing.T) {
	f := &fakeForge{commErr: errors.New("403 forbidden")}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	reply, err := r.Handle(context.Background(), tc, "alice", "note: x")
	if err == nil {
		t.Fatal("Handle must return the forge error")
	}
	if !strings.Contains(reply, "403 forbidden") {
		t.Errorf("the human must see why their note was not saved: %q", reply)
	}
	cur, _ := r.Registry.Get("111.222")
	if cur.Notes != 0 {
		t.Errorf("a failed write must not consume the per-thread budget; Notes = %d", cur.Notes)
	}
}

func TestHandleUnparseableCuratedURLFallsBackToOpeningAPR(t *testing.T) {
	f := &fakeForge{}
	r := newTestResponder(t, f)
	tc := Context{Root: "111.222", CuratedURL: "https://github.com/o/r/issues/42"}
	if err := r.Registry.Put(tc); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := r.Handle(context.Background(), tc, "alice", "note: x"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(f.opened) != 1 {
		t.Fatalf("a URL with no parseable PR number must fall back to OpenPR; opened = %d", len(f.opened))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run 'TestPRNumber|TestHandle' -v`
Expected: FAIL — `undefined: Responder`, `undefined: PRNumber`.

- [ ] **Step 3: Write the implementation**

Create `internal/thread/responder.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/ratelimit"
)

// Forge is the write surface a thread note needs. It is a subset of
// providers.CurationForge, which satisfies it — narrowed here so the responder
// declares exactly the two calls it makes and can be faked in one struct.
type Forge interface {
	CommentOnPR(ctx context.Context, number int, body string) error
	OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error)
}

// DefaultMaxNotesPerThread bounds how many knowledge writes one thread can make.
const DefaultMaxNotesPerThread = 20

// Responder turns an addressed thread message into a knowledge-base write and
// returns the text to post back. It is transport-agnostic: every chat-system
// concern lives in the adapter that calls it.
type Responder struct {
	Forge    Forge
	Registry *Registry
	// MaxNotesPerThread caps knowledge writes per thread; <= 0 means
	// DefaultMaxNotesPerThread.
	MaxNotesPerThread int
	// OpenPRs caps how often a standalone note PR may be opened, globally. A
	// chatty channel must not become a forge incident. nil means unlimited.
	OpenPRs *ratelimit.Window
	Now     func() time.Time
	Log     *slog.Logger
}

// prNumberRe matches the numeric id in a GitHub pull URL or a GitLab merge-request
// URL. Anchored on the path segment so an issue URL never matches.
var prNumberRe = regexp.MustCompile(`/(?:pull|merge_requests)/(\d+)`)

// PRNumber extracts the pull-request / merge-request number from a forge URL.
// providers.Ref carries only a URL, so the number the comment API needs is
// recovered here rather than widening the Ref contract.
func PRNumber(rawURL string) (int, bool) {
	m := prNumberRe.FindStringSubmatch(rawURL)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (r *Responder) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Responder) maxNotes() int {
	if r.MaxNotesPerThread <= 0 {
		return DefaultMaxNotesPerThread
	}
	return r.MaxNotesPerThread
}

// Handle parses raw, writes the knowledge where it belongs, and returns the
// reply to post in the thread. The reply is returned even alongside an error —
// the human must always learn what happened to their words.
func (r *Responder) Handle(ctx context.Context, tc Context, author, raw string) (string, error) {
	p := Parse(raw)

	switch p.Intent {
	case IntentReinvestigate:
		return "Re-running an investigation from a thread is not supported yet. " +
			"Add the `reinvestigate` label to the KB issue to re-run, or use `note:` to record what you know.", nil
	case IntentNote, IntentFreeform:
	}

	if p.Text == "" {
		return "Tell me what to record — for example: `note: the real cause was a spot-node reclaim`.", nil
	}

	if tc.Notes >= r.maxNotes() {
		return fmt.Sprintf("This thread has hit its note limit (%d). Add anything further directly on the pull request.", r.maxNotes()), nil
	}

	at := r.now()
	reply, err := r.write(ctx, tc, author, p.Text, at)
	if err != nil {
		return reply, err
	}

	// The budget is consumed only by a write that landed: a forge outage must not
	// burn the thread's allowance.
	if uerr := r.Registry.Update(tc.Root, func(c *Context) { c.Notes++ }); uerr != nil {
		r.Log.Warn("thread: note counter write-back failed", "root", tc.Root, "err", uerr)
	}
	if p.Intent == IntentFreeform {
		reply += "\n_Tip: prefix with `note:` to record something explicitly._"
	}
	return reply, nil
}

// write routes the note to the open KB PR, to the PR this thread already opened,
// or to a new standalone Concept PR — in that order.
func (r *Responder) write(ctx context.Context, tc Context, author, text string, at time.Time) (string, error) {
	// The route is derived from the thread context alone. It is never influenced
	// by the message text.
	for _, url := range []string{tc.CuratedURL, tc.NoteURL} {
		n, ok := PRNumber(url)
		if !ok {
			continue
		}
		if err := r.Forge.CommentOnPR(ctx, n, NoteBody(tc, author, text, at)); err != nil {
			return fmt.Sprintf("⚠️ I could not save that to the knowledge base: %v", err),
				fmt.Errorf("comment on PR %d: %w", n, err)
		}
		r.Log.Info("thread: note recorded on KB PR", "pr", n, "root", tc.Root, "author", author)
		return fmt.Sprintf("📝 Noted on the knowledge-base PR #%d — %s", n, url), nil
	}

	if r.OpenPRs != nil && !r.OpenPRs.Allow() {
		return "⚠️ I have opened too many knowledge-base PRs recently and paused. Try again shortly.", nil
	}
	ref, err := r.Forge.OpenPR(ctx, ConceptEntry(tc, author, text, at))
	if err != nil {
		return fmt.Sprintf("⚠️ I could not save that to the knowledge base: %v", err),
			fmt.Errorf("open note PR: %w", err)
	}
	if uerr := r.Registry.Update(tc.Root, func(c *Context) { c.NoteURL = ref.URL }); uerr != nil {
		r.Log.Warn("thread: note PR write-back failed; a later note in this thread may open a second PR",
			"root", tc.Root, "url", ref.URL, "err", uerr)
	}
	r.Log.Info("thread: note opened a standalone KB PR", "url", ref.URL, "root", tc.Root, "author", author)
	if n, ok := PRNumber(ref.URL); ok {
		return fmt.Sprintf("📝 Opened knowledge-base PR #%d with your note — %s", n, ref.URL), nil
	}
	return "📝 Opened a knowledge-base PR with your note — " + ref.URL, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/thread/ -run 'TestPRNumber|TestHandle' -v`
Expected: PASS — all 11 tests.

- [ ] **Step 5: Run the full package suite and the quality gate**

Run: `go test ./internal/thread/ -v && go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/thread/`
Expected: every test PASS; `gofmt -l .` silent; `0 issues`.

- [ ] **Step 6: Commit**

```bash
git add internal/thread/responder.go internal/thread/responder_test.go
git commit -m "feat(thread): route an operator note to the right KB artifact

Comment on the finding's open KB PR; failing that, on the PR this thread
already opened; failing that, open a standalone Concept PR. The NoteURL
write-back is what stops five notes becoming five PRs — OpenPR is not
idempotent, its branch name carries a unix timestamp.

The route is derived from the thread context alone and is never influenced by
the message text. A failed write does not consume the per-thread budget."
```

---

### Task 5: `Mention` — registry lookup and reply

**Files:**
- Create: `internal/thread/mention.go`
- Test: `internal/thread/mention_test.go`

**Interfaces:**
- Consumes: `Registry` (Task 1), `Responder` (Task 4).
- Produces:
  - `type Replier interface { ReplyInThread(ctx context.Context, root, channel, text string) error }`
  - `type Mention struct { Responder *Responder; Registry *Registry; Replier Replier; Log *slog.Logger }`
  - `func (m *Mention) HandleMention(ctx context.Context, channel, root, author, text string)`

- [ ] **Step 1: Write the failing test**

Create `internal/thread/mention_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

type fakeReplier struct {
	replies []string
	err     error
}

func (f *fakeReplier) ReplyInThread(_ context.Context, _, _, text string) error {
	f.replies = append(f.replies, text)
	return f.err
}

func newTestMention(t *testing.T, f *fakeForge, rep *fakeReplier) *Mention {
	t.Helper()
	r := newTestResponder(t, f)
	return &Mention{
		Responder: r,
		Registry:  r.Registry,
		Replier:   rep,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestMentionKnownThreadWritesAndReplies(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", Channel: "C1", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "<@U0BOT> note: spot reclaim")

	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(f.comments))
	}
	if len(rep.replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(rep.replies))
	}
	if !strings.Contains(rep.replies[0], "42") {
		t.Errorf("reply must name the PR: %q", rep.replies[0])
	}
}

func TestMentionUnknownThreadRepliesAndWritesNothing(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{}
	m := newTestMention(t, f, rep)

	m.HandleMention(context.Background(), "C1", "999.888", "alice", "note: x")

	if len(f.comments) != 0 || len(f.opened) != 0 {
		t.Fatal("an unknown thread must write nothing to the KB")
	}
	if len(rep.replies) != 1 {
		t.Fatalf("an unknown thread must still get a reply, got %d", len(rep.replies))
	}
	if !strings.Contains(strings.ToLower(rep.replies[0]), "don't have") &&
		!strings.Contains(strings.ToLower(rep.replies[0]), "do not have") {
		t.Errorf("the reply must name the limitation: %q", rep.replies[0])
	}
}

func TestMentionRepliesEvenWhenTheWriteFails(t *testing.T) {
	f, rep := &fakeForge{commErr: errors.New("403 forbidden")}, &fakeReplier{}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x")

	if len(rep.replies) != 1 {
		t.Fatalf("replies = %d, want 1 — a failed write must still be reported", len(rep.replies))
	}
	if !strings.Contains(rep.replies[0], "403 forbidden") {
		t.Errorf("the reply must carry the reason: %q", rep.replies[0])
	}
}

func TestMentionSurvivesAReplyFailure(t *testing.T) {
	f, rep := &fakeForge{}, &fakeReplier{err: errors.New("channel_not_found")}
	m := newTestMention(t, f, rep)
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x")

	if len(f.comments) != 1 {
		t.Fatal("the KB write must not be rolled back when the reply fails to post")
	}
}

func TestMentionWithNoReplierStillWrites(t *testing.T) {
	f := &fakeForge{}
	m := newTestMention(t, f, nil)
	m.Replier = nil
	if err := m.Registry.Put(Context{Root: "111.222", CuratedURL: "https://github.com/o/r/pull/42"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	m.HandleMention(context.Background(), "C1", "111.222", "alice", "note: x")

	if len(f.comments) != 1 {
		t.Fatal("a missing replier must not lose the note")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/thread/ -run TestMention -v`
Expected: FAIL — `undefined: Mention`.

- [ ] **Step 3: Write the implementation**

Create `internal/thread/mention.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"log/slog"
)

// Replier posts a reply into an existing thread. Implemented by the chat
// notifiers that can carry a conversation back (see providers.ThreadNotifier).
type Replier interface {
	ReplyInThread(ctx context.Context, root, channel, text string) error
}

// Mention is the transport-facing entry point: it resolves the thread, runs the
// responder, and posts the answer. It exists so a transport handler stays a
// parser — everything downstream of "a human addressed us in thread X" is here.
type Mention struct {
	Responder *Responder
	Registry  *Registry
	Replier   Replier
	Log       *slog.Logger
}

// HandleMention processes one addressed message. It never returns an error: it
// runs detached from the request that delivered it, so every failure is a log
// line plus, wherever possible, a reply the human can see.
func (m *Mention) HandleMention(ctx context.Context, channel, root, author, text string) {
	tc, ok := m.Registry.Get(root)
	if !ok {
		m.Log.Info("thread: mention in an unrecognised thread", "root", root, "channel", channel, "author", author)
		m.reply(ctx, root, channel,
			"I don't have context for this thread — I can only record knowledge in a thread I started, "+
				"and only for a limited time after the finding was posted.")
		return
	}
	// The channel is taken from the live event rather than the stored context: a
	// message can only be replied to where it was actually sent.
	tc.Channel = channel

	reply, err := m.Responder.Handle(ctx, tc, author, text)
	if err != nil {
		m.Log.Warn("thread: knowledge write failed", "root", root, "author", author, "err", err)
	}
	m.reply(ctx, root, channel, reply)
}

// reply posts best-effort. The knowledge write has already succeeded by this
// point and is never rolled back because the acknowledgement could not be
// delivered.
func (m *Mention) reply(ctx context.Context, root, channel, text string) {
	if m.Replier == nil || text == "" {
		return
	}
	if err := m.Replier.ReplyInThread(ctx, root, channel, text); err != nil {
		m.Log.Warn("thread: reply failed (best-effort)", "root", root, "err", err)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/thread/ -run TestMention -v`
Expected: PASS — all five tests.

- [ ] **Step 5: Commit**

```bash
git add internal/thread/mention.go internal/thread/mention_test.go
git commit -m "feat(thread): resolve the thread, respond, reply

Mention is the transport-facing entry point, so a transport handler stays a
parser. It never returns an error — it runs detached from the request that
delivered it, so every failure is a log line plus a reply the human can see.
A failed reply never rolls back a KB write that already landed."
```

---

### Task 6: Slack notifier — register the thread root and reply into it

**Files:**
- Modify: `internal/providers/providers.go` (add `ThreadNotifier` after the `ProgressNotifier` declaration)
- Modify: `internal/notify/registry.go:15-18` (`Deps`)
- Modify: `internal/notify/slack.go:141-178` (`SlackBot`, `Deliver`), `:52-68` (`init`), `:834-882` (`Multi`)
- Test: `internal/notify/slack_test.go`

**Interfaces:**
- Consumes: nothing from `internal/thread` (the sink is an interface, so `notify` does not import `thread`).
- Produces:
  - `providers.ThreadNotifier` — `interface { Notifier; ReplyInThread(ctx context.Context, root, channel, text string) error }`
  - `notify.ThreadSink` — `interface { Register(root, channel string, inv providers.Investigation) }`
  - `notify.Deps.Threads ThreadSink`
  - `func (s *SlackBot) ReplyInThread(ctx context.Context, root, channel, text string) error`
  - `func (m *Multi) ThreadReplier() providers.ThreadNotifier`

- [ ] **Step 1: Write the failing test**

Append to `internal/notify/slack_test.go`:

```go
type recordingThreadSink struct {
	root, channel string
	inv           providers.Investigation
	calls         int
}

func (r *recordingThreadSink) Register(root, channel string, inv providers.Investigation) {
	r.root, r.channel, r.inv, r.calls = root, channel, inv, r.calls+1
}

func TestSlackBotRegistersTheThreadRoot(t *testing.T) {
	var posts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		posts = append(posts, m)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
	}))
	defer srv.Close()

	sink := &recordingThreadSink{}
	b := NewSlackBot("xoxb-test", "C1")
	b.baseURL = srv.URL
	b.Threads = sink

	inv := providers.Investigation{Title: "OOMKilled", TriggerKey: "tk-1", CuratedURL: "https://github.com/o/r/pull/42"}
	if err := b.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	if sink.calls != 1 {
		t.Fatalf("Register calls = %d, want 1 (the summary root only, never the detail reply)", sink.calls)
	}
	if sink.root != "111.222" {
		t.Errorf("root = %q, want 111.222", sink.root)
	}
	if sink.channel != "C1" {
		t.Errorf("channel = %q, want C1", sink.channel)
	}
	if sink.inv.TriggerKey != "tk-1" {
		t.Errorf("investigation not passed through: %+v", sink.inv)
	}
	if len(posts) == 0 {
		t.Fatal("nothing was posted")
	}
}

func TestSlackBotDeliverSucceedsWithNoThreadSink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"ts":"111.222"}`))
	}))
	defer srv.Close()

	b := NewSlackBot("xoxb-test", "C1")
	b.baseURL = srv.URL
	// Threads deliberately left nil.
	if err := b.Deliver(context.Background(), providers.Investigation{Title: "OOMKilled"}); err != nil {
		t.Fatalf("Deliver with a nil thread sink: %v", err)
	}
}

func TestSlackBotReplyInThread(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"333.444"}`))
	}))
	defer srv.Close()

	b := NewSlackBot("xoxb-test", "C-default")
	b.baseURL = srv.URL
	if err := b.ReplyInThread(context.Background(), "111.222", "C-live", "📝 Noted"); err != nil {
		t.Fatalf("ReplyInThread: %v", err)
	}

	if got["thread_ts"] != "111.222" {
		t.Errorf("thread_ts = %v, want 111.222", got["thread_ts"])
	}
	if got["channel"] != "C-live" {
		t.Errorf("channel = %v, want C-live (the live event's channel, not the configured default)", got["channel"])
	}
	if got["text"] != "📝 Noted" {
		t.Errorf("text = %v, want the reply text", got["text"])
	}
}

func TestMultiThreadReplier(t *testing.T) {
	bot := NewSlackBot("xoxb-test", "C1")
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewSlack("https://hooks.slack.com/x"), bot)
	if got := m.ThreadReplier(); got != providers.ThreadNotifier(bot) {
		t.Fatalf("ThreadReplier = %v, want the bot notifier", got)
	}

	none := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), NewSlack("https://hooks.slack.com/x"))
	if got := none.ThreadReplier(); got != nil {
		t.Fatalf("ThreadReplier = %v, want nil when no notifier can reply", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/notify/ -run 'TestSlackBotRegisters|TestSlackBotReply|TestMultiThreadReplier|TestSlackBotDeliverSucceedsWithNoThreadSink' -v`
Expected: FAIL — `b.Threads undefined`, `b.ReplyInThread undefined`, `m.ThreadReplier undefined`.

(`slack_test.go` already imports everything these tests need — `context`, `encoding/json`, `io`, `log/slog`, `net/http/httptest`, `providers`. No import changes.)

- [ ] **Step 3: Add `providers.ThreadNotifier`**

In `internal/providers/providers.go`, immediately after the `ProgressNotifier` interface declaration, add:

```go
// ThreadNotifier is the optional capability of carrying a conversation back into
// a delivered notification's thread. A notifier that cannot — an incoming
// webhook, the generic HTTP sink — simply does not implement it, and thread
// interaction is unavailable on that transport. Same contract as an unset data
// source disabling its tool: no capability is ever faked.
type ThreadNotifier interface {
	Notifier
	// ReplyInThread posts text into the thread rooted at root, in channel. Both
	// handles are transport-specific opaque strings (Slack: thread_ts and a
	// channel id).
	ReplyInThread(ctx context.Context, root, channel, text string) error
}
```

- [ ] **Step 4: Add `ThreadSink` and wire it through `Deps`**

In `internal/notify/registry.go`, replace the `Deps` struct with:

```go
type Deps struct {
	Cfg *config.Config
	Log *slog.Logger
	// Threads receives the thread root of each delivered investigation, so a
	// later reply can be attributed. nil (the default) disables thread capture.
	Threads ThreadSink
}
```

In `internal/notify/slack.go`, add after the `SlackDeliveryTarget` function:

```go
// ThreadSink records the thread root a delivered investigation was posted to.
// Implemented by *thread.Registry; declared here as an interface so the notifier
// never imports the thread package and stays ignorant of how the mapping is
// stored. Nil-safe by contract at every call site: registration is best-effort
// and must never affect delivery.
type ThreadSink interface {
	Register(root, channel string, inv providers.Investigation)
}
```

- [ ] **Step 5: Add the `Threads` field, register on delivery, and add `ReplyInThread`**

In `internal/notify/slack.go`, add a field to `SlackBot` (after `FeedbackButtons`):

```go
	// Threads, when set (notify.slack.thread_capture), receives the summary
	// message's ts so a later reply in that thread can be attributed to this
	// investigation. Never set from the detail reply — the root is the handle.
	Threads ThreadSink
```

In the `init()` Slack descriptor, set it on the bot path:

```go
			case slackTargetBot:
				b := NewSlackBot(os.Getenv(sl.BotTokenEnv), sl.Channel)
				b.FeedbackButtons = sl.FeedbackButtons
				if sl.ThreadCapture {
					b.Threads = d.Threads
				}
				return b, nil
```

In `SlackBot.Deliver`, register immediately after the summary post returns its `ts` — before the detail thread post, so a failed detail post cannot cost the registration:

```go
	ts, err := s.post(ctx, map[string]any{"text": fallbackText(inv), "blocks": summary})
	if err != nil {
		return err
	}
	// Record the thread root so a reply here can be attributed. Best-effort and
	// nil-safe: capture is an opt-in extra, delivery is the contract.
	if s.Threads != nil && ts != "" {
		s.Threads.Register(ts, s.channel, inv)
	}
	detail := detailBlocks(inv)
```

Add the reply method next to `DeliverProgress`:

```go
// ReplyInThread posts text as a reply in the thread rooted at root
// (providers.ThreadNotifier). It targets the channel it is given rather than the
// configured default: a reply can only go where the message it answers was sent.
func (s *SlackBot) ReplyInThread(ctx context.Context, root, channel, text string) error {
	msg := map[string]any{"text": text, "thread_ts": root}
	if channel != "" {
		msg["channel"] = channel
	}
	_, err := s.post(ctx, msg)
	return err
}
```

Extend the `SlackBot` interface assertions:

```go
var (
	_ providers.Notifier         = (*SlackBot)(nil)
	_ providers.ProgressNotifier = (*SlackBot)(nil)
	_ providers.ThreadNotifier   = (*SlackBot)(nil)
)
```

**Note on `post`:** it sets `msg["channel"] = s.channel` unconditionally. Change that line to respect a channel already present, so `ReplyInThread` can override it:

```go
func (s *SlackBot) post(ctx context.Context, msg map[string]any) (string, error) {
	if _, ok := msg["channel"]; !ok {
		msg["channel"] = s.channel
	}
```

- [ ] **Step 6: Add `Multi.ThreadReplier`**

In `internal/notify/slack.go`, after `Multi.Len`:

```go
// ThreadReplier returns the first configured notifier that can carry a reply
// back into a thread, or nil when none can. The wiring needs one concrete
// replier and Multi is where the built notifiers live; this is the same
// capability discovery Deliver does for ProgressNotifier, exposed.
func (m *Multi) ThreadReplier() providers.ThreadNotifier {
	for _, n := range m.notifiers {
		if tn, ok := n.(providers.ThreadNotifier); ok {
			return tn
		}
	}
	return nil
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `go test ./internal/notify/ -v`
Expected: PASS — the four new tests plus every pre-existing notify test. `TestSlackBotDeliversDetailInThread` (or equivalently named existing threading test) must still pass — the `post` change is behaviour-preserving because every other caller leaves `channel` unset.

- [ ] **Step 8: Commit**

```bash
git add internal/providers/providers.go internal/notify/registry.go internal/notify/slack.go internal/notify/slack_test.go
git commit -m "feat(notify): capture the Slack thread root and reply into it

ThreadSink is an interface so the notifier never imports the thread package —
it hands over the ts it already has and stays ignorant of how the mapping is
stored. Registration happens right after the summary post, before the detail
reply, so a failed detail post cannot cost it; it is nil-safe and best-effort,
because capture is an opt-in extra and delivery is the contract.

ThreadNotifier is an optional capability alongside ProgressNotifier: a webhook
notifier simply does not implement it and thread interaction is unavailable
there. No capability is faked."
```

---

### Task 7: Config — `thread_capture` and its validation

**Files:**
- Modify: `internal/config/config.go` (`SlackNotify` struct near `:805`; `Validate` near `:1446`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.SlackNotify.ThreadCapture bool` (yaml `thread_capture`).

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestValidateThreadCapture(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.Notify.Slack = SlackNotify{
			BotTokenEnv:      "SLACK_BOT_TOKEN",
			Channel:          "C1",
			SigningSecretEnv: "SLACK_SIGNING_SECRET",
			ThreadCapture:    true,
		}
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(*Config) {}, ""},
		{"off needs nothing", func(c *Config) {
			c.Notify.Slack = SlackNotify{ThreadCapture: false}
		}, ""},
		{"missing signing secret", func(c *Config) {
			c.Notify.Slack.SigningSecretEnv = ""
		}, "signing_secret_env"},
		{"webhook-only delivery", func(c *Config) {
			c.Notify.Slack.BotTokenEnv = ""
			c.Notify.Slack.Channel = ""
			c.Notify.Slack.WebhookURLEnv = "SLACK_WEBHOOK_URL"
		}, "bot_token_env"},
		{"bot token without a channel", func(c *Config) {
			c.Notify.Slack.Channel = ""
		}, "channel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate() = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestValidateThreadCapture -v`
Expected: FAIL — `unknown field ThreadCapture in struct literal`.

- [ ] **Step 3: Add the field**

In `internal/config/config.go`, in the `SlackNotify` struct immediately after `FeedbackButtons`:

```go
	// ThreadCapture (opt-in) exposes POST /slack/events and lets a human write
	// knowledge back from an investigation thread: `@runlore note: <text>` lands
	// on the finding's KB PR, or opens one when it has none.
	//
	// Requires the BOT-token delivery path: an incoming webhook returns no message
	// ts, so there is no thread root to attribute a reply to and no way to reply.
	// Requires signing_secret_env for the same reason feedback_buttons does —
	// events arrive on an exposed endpoint and must be signature-verified.
	ThreadCapture bool `yaml:"thread_capture"`
```

- [ ] **Step 4: Add the validation rule**

In `internal/config/config.go`, immediately after the existing `feedback_buttons` check (around `:1446`):

```go
	if sl := c.Notify.Slack; sl.ThreadCapture {
		if sl.SigningSecretEnv == "" {
			return fmt.Errorf("notify.slack.thread_capture requires notify.slack.signing_secret_env: mentions arrive on the exposed POST /slack/events endpoint and must be signature-verified")
		}
		if sl.BotTokenEnv == "" {
			return fmt.Errorf("notify.slack.thread_capture requires notify.slack.bot_token_env: an incoming webhook returns no message ts, so there is no thread to attribute a reply to")
		}
		if sl.Channel == "" {
			return fmt.Errorf("notify.slack.thread_capture requires notify.slack.channel (it is required alongside bot_token_env)")
		}
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/config/ -run TestValidateThreadCapture -v`
Expected: PASS — all five subtests.

- [ ] **Step 6: Run the full config suite**

Run: `go test ./internal/config/`
Expected: PASS. If a golden/round-trip test enumerates `SlackNotify` fields, update its fixture to include `thread_capture`.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): notify.slack.thread_capture

Opt-in, default off, matching feedback_buttons. Fails loud on the two ways it
cannot work: no signing secret (events arrive on an exposed endpoint), and
webhook-only delivery (an incoming webhook returns no ts, so there is no thread
root to attribute a reply to)."
```

---

### Task 8: `POST /slack/events`

**Files:**
- Modify: `internal/server/server.go` (`Server` struct `:31-46`, `Actions` `:63+`, `New` `:87-107`, new handler after `handleSlackInteraction`)
- Test: Create `internal/server/events_test.go`

**Interfaces:**
- Consumes: `Server.verifySlack` (existing), `fwd.middleware` (existing).
- Produces:
  - `type ThreadHandler interface { HandleMention(ctx context.Context, channel, root, author, text string) }`
  - `Actions.Threads ThreadHandler`

- [ ] **Step 1: Write the failing test**

Create `internal/server/events_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturingThreadHandler struct {
	mu    sync.Mutex
	calls []struct{ channel, root, author, text string }
	done  chan struct{}
}

func newCapturingThreadHandler() *capturingThreadHandler {
	return &capturingThreadHandler{done: make(chan struct{}, 16)}
}

func (c *capturingThreadHandler) HandleMention(_ context.Context, channel, root, author, text string) {
	c.mu.Lock()
	c.calls = append(c.calls, struct{ channel, root, author, text string }{channel, root, author, text})
	c.mu.Unlock()
	c.done <- struct{}{}
}

func (c *capturingThreadHandler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

const testSigningSecret = "s3cr3t"

func signedEventRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	_, _ = mac.Write([]byte("v0:" + ts + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func newEventServer(t *testing.T, h ThreadHandler) *Server {
	t.Helper()
	return New(nil, Actions{SlackSecret: testSigningSecret, Threads: h}, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func waitForMention(t *testing.T, h *capturingThreadHandler) {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the mention to be dispatched")
	}
}

func TestEventsAppMentionDispatches(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	body := `{"type":"event_callback","event_id":"Ev1","event":{"type":"app_mention","user":"U1","text":"<@U0BOT> note: x","channel":"C1","ts":"333.444","thread_ts":"111.222"}}`
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	waitForMention(t, h)
	if h.calls[0].root != "111.222" {
		t.Errorf("root = %q, want 111.222 (the thread root, not the reply ts)", h.calls[0].root)
	}
	if h.calls[0].channel != "C1" || h.calls[0].author != "U1" {
		t.Errorf("dispatch = %+v, want channel C1 author U1", h.calls[0])
	}
	if h.calls[0].text != "<@U0BOT> note: x" {
		t.Errorf("text = %q, want the raw text (the responder strips the mention)", h.calls[0].text)
	}
}

func TestEventsURLVerification(t *testing.T) {
	s := newEventServer(t, newCapturingThreadHandler())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, `{"type":"url_verification","challenge":"abc123"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "abc123" {
		t.Fatalf("body = %q, want the challenge echoed back", got)
	}
}

func TestEventsRejectsBadSignature(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	req := signedEventRequest(t, `{"type":"event_callback","event_id":"Ev1","event":{"type":"app_mention","thread_ts":"111.222"}}`)
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if h.count() != 0 {
		t.Fatal("an unsigned event must never be dispatched")
	}
}

func TestEventsDisabledWhenNoHandlerWired(t *testing.T) {
	s := New(nil, Actions{SlackSecret: testSigningSecret}, nil, nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, signedEventRequest(t, `{"type":"url_verification","challenge":"abc"}`))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when thread capture is off", rec.Code)
	}
}

func TestEventsRetryIsDeduped(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	body := `{"type":"event_callback","event_id":"Ev-dup","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, signedEventRequest(t, body))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200 (a retry must still be acked)", i, rec.Code)
		}
	}
	waitForMention(t, h)
	time.Sleep(100 * time.Millisecond)
	if h.count() != 1 {
		t.Fatalf("dispatches = %d, want 1 — Slack retries must not file the note repeatedly", h.count())
	}
}

func TestEventsIgnoresBotAndNonThreadAndNonMention(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"bot author", `{"type":"event_callback","event_id":"E1","event":{"type":"app_mention","bot_id":"B1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`},
		{"not in a thread", `{"type":"event_callback","event_id":"E2","event":{"type":"app_mention","user":"U1","text":"note: x","channel":"C1","ts":"111.222"}}`},
		{"not an app_mention", `{"type":"event_callback","event_id":"E3","event":{"type":"message","user":"U1","text":"note: x","channel":"C1","thread_ts":"111.222"}}`},
		{"no user and no bot", `{"type":"event_callback","event_id":"E4","event":{"type":"app_mention","text":"note: x","channel":"C1","thread_ts":"111.222"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCapturingThreadHandler()
			s := newEventServer(t, h)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, signedEventRequest(t, tt.body))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — an ignored event is still acked so Slack stops retrying", rec.Code)
			}
			time.Sleep(50 * time.Millisecond)
			if h.count() != 0 {
				t.Fatalf("dispatches = %d, want 0", h.count())
			}
		})
	}
}

func TestEventsStaleTimestampRejected(t *testing.T) {
	h := newCapturingThreadHandler()
	s := newEventServer(t, h)

	body := `{"type":"event_callback","event_id":"E1","event":{"type":"app_mention","user":"U1","thread_ts":"111.222"}}`
	old := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(testSigningSecret))
	_, _ = mac.Write([]byte("v0:" + old + ":" + body))
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", old)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 — a replayed request must be rejected", rec.Code)
	}
	if h.count() != 0 {
		t.Fatal("a replayed event must never be dispatched")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/server/ -run TestEvents -v`
Expected: FAIL — `undefined: ThreadHandler`, `unknown field Threads in struct literal of type Actions`.

- [ ] **Step 3: Add the interface, the field, and the route**

(`server.New` is `func New(ready func() bool, acts Actions, built []source.Built, pipe *source.Pipeline, metricsHandler http.Handler, fwd *Forward, log *slog.Logger) *Server` — verified; `newEventServer` in Step 1 matches it, and `fwd == nil` makes `work` the identity middleware.)

In `internal/server/server.go`, add after the `FeedbackRecorder` interface:

```go
// ThreadHandler processes a human message addressed to RunLore inside a thread.
// Implemented by *thread.Mention. It returns nothing: the endpoint acks Slack
// within its 3s deadline and the handler runs detached, replying in the thread
// itself.
type ThreadHandler interface {
	HandleMention(ctx context.Context, channel, root, author, text string)
}
```

Add to `Actions`:

```go
	Threads ThreadHandler // opt-in thread capture (notify.slack.thread_capture)
```

Add to the `Server` struct:

```go
	threads    ThreadHandler // nil unless notify.slack.thread_capture is on
	seenEvents *seenSet      // Slack event_id dedup for delivery retries
	eventSlots chan struct{} // bounds concurrent detached mention handlers
```

In `New`, set them alongside the other `acts` fields:

```go
		threads:    acts.Threads,
		seenEvents: newSeenSet(1024),
		eventSlots: make(chan struct{}, 4),
```

And register the route next to the interactions route:

```go
	mux.Handle("POST /slack/interactions", work(http.HandlerFunc(s.handleSlackInteraction)))
	mux.Handle("POST /slack/events", work(http.HandlerFunc(s.handleSlackEvent)))
```

- [ ] **Step 4: Write the handler**

Add to `internal/server/server.go`, after `handleSlackInteraction`:

```go
// slackEvent is the subset of Slack's Events API envelope this server reads.
type slackEvent struct {
	Type      string `json:"type"`
	Challenge string `json:"challenge"`
	EventID   string `json:"event_id"`
	Event     struct {
		Type     string `json:"type"`
		User     string `json:"user"`
		BotID    string `json:"bot_id"`
		Text     string `json:"text"`
		Channel  string `json:"channel"`
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"event"`
}

// handleSlackEvent receives Events API deliveries for the opt-in thread capture:
// a human writing `@runlore …` inside an investigation thread.
//
// Only app_mention is subscribed — never message.channels. That is explicit
// consent: RunLore reads nothing in a channel it was not addressed in, and there
// is no firehose to filter.
//
// The endpoint acks BEFORE doing any work. Slack retries anything it does not
// see acked within 3 seconds, and a knowledge write is a forge round-trip; a
// synchronous handler would be retried mid-write and file the note repeatedly.
func (s *Server) handleSlackEvent(w http.ResponseWriter, r *http.Request) {
	if s.threads == nil || s.slackSecret == "" {
		http.Error(w, "slack events not enabled", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if !s.verifySlack(r.Header, body) {
		s.log.Warn("rejected slack event: bad signature")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ev slackEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}

	// The subscription handshake: Slack posts a challenge to the Request URL and
	// expects it echoed. Signature-verified like everything else on this endpoint.
	if ev.Type == "url_verification" {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(ev.Challenge))
		return
	}

	// Ack first, unconditionally: every path below this line is a reason to IGNORE
	// the event, and an ignored event still has to be acked or Slack keeps retrying it.
	w.WriteHeader(http.StatusOK)

	if ev.Type != "event_callback" || ev.Event.Type != "app_mention" {
		return
	}
	// Loop guard: never act on our own messages, or any other app's.
	if ev.Event.BotID != "" || ev.Event.User == "" {
		return
	}
	// Only replies inside a thread carry a root to attribute knowledge to. A
	// top-level mention has ts but no thread_ts.
	if ev.Event.ThreadTS == "" {
		return
	}
	if ev.EventID != "" && !s.seenEvents.add(ev.EventID) {
		s.log.Debug("slack event: duplicate delivery ignored", "event_id", ev.EventID)
		return
	}

	// Detached from the request: the response is already written. WithoutCancel
	// keeps request-scoped values while surviving the handler's return.
	ctx := context.WithoutCancel(r.Context())
	select {
	case s.eventSlots <- struct{}{}:
	default:
		s.log.Warn("slack event: mention dropped, handler pool saturated", "channel", ev.Event.Channel)
		return
	}
	go func() {
		defer func() { <-s.eventSlots }()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("recovered from thread handler panic", "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		s.threads.HandleMention(ctx, ev.Event.Channel, ev.Event.ThreadTS, ev.Event.User, ev.Event.Text)
	}()
}

// seenSet is a bounded set of recently-seen ids. It exists so Slack's delivery
// retries are ignored rather than filing the same note twice. Reset past the cap
// rather than evicted individually: the live working set is the last few
// minutes of events, so the crude bound is exact enough and has no ordering cost.
type seenSet struct {
	mu   sync.Mutex
	cap  int
	seen map[string]struct{}
}

func newSeenSet(capacity int) *seenSet {
	return &seenSet{cap: capacity, seen: make(map[string]struct{}, capacity)}
}

// add records id and reports whether it was NEW (true = act on it).
func (s *seenSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.seen[id]; dup {
		return false
	}
	if len(s.seen) >= s.cap {
		s.seen = make(map[string]struct{}, s.cap)
	}
	s.seen[id] = struct{}{}
	return true
}
```

Add `"sync"` to the imports if not already present (`context`, `encoding/json`, `io`, `runtime/debug` are already imported by this file).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/server/ -run TestEvents -v`
Expected: PASS — all eight tests including every subtest of `TestEventsIgnoresBotAndNonThreadAndNonMention`.

- [ ] **Step 6: Run the full server suite**

Run: `go test ./internal/server/`
Expected: PASS — no regression in the interactions handler or the authguard tests.

- [ ] **Step 7: Commit**

```bash
git add internal/server/server.go internal/server/events_test.go
git commit -m "feat(server): POST /slack/events for thread capture

Subscribes to app_mention only — never message.channels: explicit consent, no
firehose, nothing read in a channel RunLore was not addressed in. Reuses
verifySlack and the leader-forwarding wrapper; 404s unless thread capture is
wired, like the interactions endpoint.

Acks before doing any work. Slack retries anything unacked within 3s and a
knowledge write is a forge round-trip, so a synchronous handler would be
retried mid-write and file the note repeatedly; event_id dedup closes the
window that remains. Detached handlers are bounded by a slot pool."
```

---

### Task 9: Wiring, startup guard, and docs

**Files:**
- Modify: `internal/app/notify.go` (`BuildNotifier` signature; new builders)
- Modify: `internal/app/investigate.go:537,544` (`BuildInvestigator` signature + returns)
- Modify: `internal/app/demo.go:285` (`BuildNotifier` call)
- Modify: `internal/app/serve.go:184` (registry build + `BuildInvestigator` call) and the `acts := server.Actions{…}` block near `:376`
- Test: `internal/app/notify_test.go`
- Modify: `internal/app/investigate_test.go:236,251,265,280` (call sites)
- Modify: `docs/` — the Slack configuration page and the notifications reference

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `func BuildThreadRegistry(cfg *config.Config) (*thread.Registry, error)`
  - `func ThreadCaptureDeliverable(cfg *config.Config, log *slog.Logger) bool`

- [ ] **Step 1: Write the failing test**

Append to `internal/app/notify_test.go`:

```go
func TestBuildThreadRegistryDisabledWithoutCapture(t *testing.T) {
	cfg := &config.Config{}
	cfg.Outcome.LedgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if reg.Enabled() {
		t.Fatal("thread capture off must yield a disabled registry")
	}
}

func TestBuildThreadRegistryUsesTheLedgerDirectory(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Outcome.LedgerPath = filepath.Join(dir, "ledger.jsonl")
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "T", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}

	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if !reg.Enabled() {
		t.Fatal("thread capture on with a ledger path must yield an enabled registry")
	}
	if err := reg.Put(thread.Context{Root: "1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "threads.jsonl")); err != nil {
		t.Fatalf("registry must persist beside the ledger: %v", err)
	}
}

func TestBuildThreadRegistryDisabledWithoutLedgerPath(t *testing.T) {
	// The registry needs somewhere durable to live. Without a ledger path there is
	// no state directory, so capture degrades to unavailable rather than to
	// silently-forgetful.
	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "T", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	reg, err := BuildThreadRegistry(cfg)
	if err != nil {
		t.Fatalf("BuildThreadRegistry: %v", err)
	}
	if reg.Enabled() {
		t.Fatal("no ledger path must yield a disabled registry")
	}
}

func TestThreadCaptureDeliverable(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN_PRESENT", "xoxb-real")

	cfg := &config.Config{}
	cfg.Notify.Slack = config.SlackNotify{
		BotTokenEnv: "SLACK_BOT_TOKEN_PRESENT", Channel: "C1", SigningSecretEnv: "S", ThreadCapture: true,
	}
	if !ThreadCaptureDeliverable(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("a present bot token must be deliverable")
	}

	cfg.Notify.Slack.BotTokenEnv = "SLACK_BOT_TOKEN_ABSENT"
	if ThreadCaptureDeliverable(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) {
		t.Fatal("an empty bot-token env means no message is delivered, so no thread exists to reply in")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run 'TestBuildThreadRegistry|TestThreadCaptureDeliverable' -v`
Expected: FAIL — `undefined: BuildThreadRegistry`, `undefined: ThreadCaptureDeliverable`.

- [ ] **Step 3: Write the builders**

Append to `internal/app/notify.go` (adding imports `path/filepath`, `time`, and `github.com/Smana/runlore/internal/thread`):

```go
// Thread-registry bounds. A thread stays answerable for a week — long enough for
// a Monday follow-up on a Friday incident, short enough that the live set stays
// small — and the size cap is the real backstop for a busy channel.
const (
	threadRegistryTTL = 7 * 24 * time.Hour
	threadRegistryMax = 2000
)

// BuildThreadRegistry assembles the thread-context registry backing
// notify.slack.thread_capture: nil-path (disabled) unless the option is on AND
// the outcome ledger has a path.
//
// The ledger path is the dependency because it names the durable state
// directory — the registry has to survive a restart or a leader failover for the
// same reason the ledger does, and inventing a second location for one file
// would be a second thing to mount. Without it, capture degrades to
// unavailable, which the human is told, rather than to silently forgetful.
func BuildThreadRegistry(cfg *config.Config) (*thread.Registry, error) {
	if !cfg.Notify.Slack.ThreadCapture || cfg.Outcome.LedgerPath == "" {
		return thread.NewRegistry("", threadRegistryTTL, threadRegistryMax)
	}
	path := filepath.Join(filepath.Dir(cfg.Outcome.LedgerPath), "threads.jsonl")
	return thread.NewRegistry(path, threadRegistryTTL, threadRegistryMax)
}

// ThreadCaptureDeliverable reports whether thread capture can actually work, and
// warns when it cannot. Call it only with notify.slack.thread_capture on.
//
// Validate already requires the bot-token fields alongside the option, but
// configuration is not delivery: the env var holding the token can be present
// and EMPTY at runtime (an unmounted secret, a blank Helm value), and the Slack
// builder then returns no notifier at all. No message is posted, so no thread
// exists to reply in — while startup announced the feature as enabled. Same
// guard, same reason, as SlackFeedbackDeliverable.
func ThreadCaptureDeliverable(cfg *config.Config, log *slog.Logger) bool {
	sl := cfg.Notify.Slack
	if notify.SlackBotDelivery(sl) {
		return true
	}
	log.Warn("slack thread_capture enabled but no bot-token delivery target resolved (credential env var empty); "+
		"no message is delivered, so no thread exists to capture knowledge in",
		"bot_token_env", sl.BotTokenEnv, "channel", sl.Channel)
	return false
}
```

`SlackBotDelivery` does not exist yet. Add it to `internal/notify/slack.go` beside `SlackDeliveryTarget` rather than restating the `"bot"` literal in `app`:

```go
// SlackBotDelivery reports whether Slack delivery resolves to the BOT-token path
// (chat.postMessage) rather than an incoming webhook. Thread capture requires
// exactly that path — a webhook returns no message ts, so there is no thread
// root — and asking here keeps the target vocabulary in one package.
func SlackBotDelivery(sl config.SlackNotify) bool {
	return SlackDeliveryTarget(sl) == slackTargetBot
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/app/ -run 'TestBuildThreadRegistry|TestThreadCaptureDeliverable' -v`
Expected: PASS — all four tests.

- [ ] **Step 5: Plumb the sink to the notifier and the notifier out to `serve.go`**

The notifier is **not** built in `serve.go` — `BuildInvestigator` builds it internally (`internal/app/investigate.go:544`) and does not return it. Two signature changes plumb it out. Both are contained in `internal/app`.

**5a.** `internal/app/notify.go` — `BuildNotifier` takes the sink:

```go
// BuildNotifier assembles the configured chat notifiers (best-effort fan-out)
// via the notifier registry. Slack/Matrix (and any registered sink, e.g. the
// generic webhook) self-register; each Build reads its own config.
//
// threads (nil to disable) is handed to notifiers that can capture a thread
// root, so a later reply there can be attributed to this investigation.
func BuildNotifier(cfg *config.Config, threads notify.ThreadSink, log *slog.Logger) (*notify.Multi, error) {
	return notify.BuildEnabled(notify.Deps{Cfg: cfg, Log: log, Threads: threads})
}
```

Update its two callers:
- `internal/app/demo.go:285` → `BuildNotifier(cfg, nil, log)` (a demo run captures nothing).
- `internal/app/investigate.go:544` → `BuildNotifier(cfg, threads, log)`, where `threads` is the new parameter added in 5b.

**5b.** `internal/app/investigate.go:537` — `BuildInvestigator` accepts the sink and returns the notifier it already builds:

```go
func BuildInvestigator(ctx context.Context, cfg *config.Config, deps *Deps, approvals *action.Approvals, auto *action.Auto, metrics *telemetry.Metrics, ledger *outcome.Ledger, threads notify.ThreadSink, log *slog.Logger) (investigate.Investigator, *catalog.Catalog, *notify.Multi, error) {
```

Returning the notifier is what gives `serve.go` a handle on the replier. Update every `return` in the function body to carry it (the early log-only return returns `nil` for it), and update the five call sites:
- `internal/app/serve.go:184` → `inv, cat, notifier, err := BuildInvestigator(ctx, cfg, deps, approvals, auto, metrics, ledger, threadRegistry, log)`
- `internal/app/investigate_test.go:236,251,265,280` → add `nil` before `log`, and add a `_` for the new return.

**5c.** `internal/app/serve.go` — build the registry immediately before that call (line 184), after the ledger is opened:

```go
	threadRegistry, err := BuildThreadRegistry(cfg)
	if err != nil {
		return fmt.Errorf("thread registry: %w", err)
	}
```

**5d.** `internal/app/serve.go` — wire the handler in the `acts := server.Actions{…}` block, after the existing `feedback_buttons` wiring (around `:390`):

```go
	// Opt-in thread capture: wire the handler ONLY when the option is on, the
	// registry persists, a forge can be reached, and a notifier can reply. A
	// capture path that took someone's knowledge and said nothing back — or had
	// nowhere to write it — would be worse than not having one.
	if cfg.Notify.Slack.ThreadCapture && threadRegistry.Enabled() {
		forge := buildForge(cfg, log)
		replier := notifier.ThreadReplier()
		switch {
		case forge == nil:
			log.Warn("slack thread_capture enabled but no forge is configured (forge.kb_repo / credentials); knowledge cannot be written")
		case replier == nil:
			log.Warn("slack thread_capture enabled but no thread-capable notifier resolved; replies cannot be posted")
		default:
			acts.Threads = &thread.Mention{
				Responder: &thread.Responder{
					Forge:             forge,
					Registry:          threadRegistry,
					MaxNotesPerThread: thread.DefaultMaxNotesPerThread,
					OpenPRs:           ratelimit.New(20, time.Hour),
					Log:               log,
				},
				Registry: threadRegistry,
				Replier:  replier,
				Log:      log,
			}
			if ThreadCaptureDeliverable(cfg, log) {
				log.Info("slack thread capture enabled", "endpoint", "/slack/events")
			}
		}
	}
```

`buildForge` (`internal/app/curator.go:39`) is unexported but this is the same package, so no export is needed. It returns a `forgeClient`, which embeds `providers.CurationForge` and therefore satisfies `thread.Forge`. It is cheap and stateless to construct — calling it here as well as inside `BuildCurator` is not a shared-resource concern (contrast `BuildDeps`, which is built once because a second catalog means a second git-sync goroutine).

Add `"github.com/Smana/runlore/internal/thread"` to `serve.go`'s imports; `ratelimit` and `time` are already imported there.

- [ ] **Step 6: Run the full quality gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build clean; every test PASS; `gofmt -l .` silent; `0 issues`.

- [ ] **Step 7: Write the docs**

Find the Slack configuration page: `grep -rln "feedback_buttons" docs/ website/ | head`. In that page, add a section after the feedback-buttons one:

```markdown
### Write knowledge back from a thread

With `thread_capture` on, replying to a finding's thread records what you know
into the knowledge base:

    @runlore note: the real cause was the spot-node reclaim at 14:02

The note lands as a comment on that finding's knowledge-base PR. When the
finding has no PR — an instant recall, or a `no_action` verdict — RunLore opens
a small `Concept` entry PR instead, so the knowledge still lands somewhere. A
human reviews and merges it, like every other entry.

`@runlore reinvestigate: …` is reserved and not supported yet; add the
`reinvestigate` label to the knowledge-base issue to re-run an investigation.

```yaml
notify:
  slack:
    bot_token_env: SLACK_BOT_TOKEN
    channel: C0123456789
    signing_secret_env: SLACK_SIGNING_SECRET
    thread_capture: true
outcome:
  ledger_path: /var/lib/runlore/outcome.jsonl   # required: the registry lives beside it
```

In your Slack app:

1. **OAuth & Permissions** → add the `app_mentions:read` bot scope (`chat:write`
   is already required for delivery), then reinstall the app.
2. **Event Subscriptions** → enable, set the Request URL to
   `https://<your-runlore>/slack/events`, and subscribe to the bot event
   `app_mention`. Slack verifies the URL with a signed challenge, so the endpoint
   must be reachable before you save.

Only `app_mention` is subscribed — RunLore reads nothing in channels where it
was not directly addressed.
```

- [ ] **Step 8: Verify the docs build**

Run: `grep -rn "thread_capture" docs/ website/ | head`
Expected: the new section appears. If the site has a config-reference table that enumerates `notify.slack.*` keys, add `thread_capture` to it — `grep -rn "feedback_buttons" docs/ website/` finds every place the sibling key is listed.

- [ ] **Step 9: Final quality gate and commit**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all clean.

```bash
git add internal/app/ docs/ website/
git commit -m "feat(app): wire Slack thread capture

The registry lives beside the outcome ledger: it must survive a restart and a
leader failover for the same reason the ledger does, and a second state
location would be a second thing to mount. No ledger path means capture is
unavailable — which the human is told — rather than silently forgetful.

The handler is wired only when a notifier can actually reply: a capture path
that took someone's knowledge and said nothing back would be worse than not
having one."
```

---

## Self-Review

**Spec coverage:**

| Spec section | Task |
|---|---|
| `thread.Context` | 1 |
| Registry: bounded, TTL, JSONL persist + replay | 1 |
| `notify.ThreadSink`, nil-safe, called after the summary post | 6 |
| Grammar: `note:` / freeform / reserved `reinvestigate:` | 2 |
| Provenance header, verbatim text | 3 |
| `Concept` typing + kbvalidate conformance | 3 |
| Routing table (open PR → NoteURL → standalone) | 4 |
| `NoteURL` write-back prevents repeat `OpenPR` | 4 |
| Rate limits (per thread, global OpenPR) | 4 |
| Route never model-chosen | 4 (no model in PR1; asserted by construction) |
| Image-markdown neutralisation | 3 |
| `POST /slack/events`: challenge, ack-first, `event_id` dedup, loop guard, `app_mention` only | 8 |
| Signature verification reused | 8 |
| Leader forwarding via `work()` | 8 |
| Config `thread_capture` + Validate rules | 7 |
| Error-handling table (registry miss, forge failure, reply failure, rate limit) | 4, 5 |
| Docs: Slack scope + Request URL + grammar | 9 |

Not in PR1, by design: Matrix transport (PR2), chat layer and `model.chat` (PR3), `providers.ThreadNotifier` is added here because PR1 needs the reply capability.

**Deferred to PR2, flagged so it is not lost:** `thread.Context.DupFingerprint` is defined and stored but unused in PR1 — it exists for the Matrix stamped-field payload and for future entry-amendment. Leaving it unread is intentional; `golangci-lint` does not flag unused struct fields.

**Type consistency:** `Context`, `Registry`, `Parse`/`Parsed`/`Intent`, `NoteBody`, `ConceptEntry`, `Forge`, `Responder.Handle`, `PRNumber`, `Replier`, `Mention.HandleMention`, `ThreadSink.Register`, `ThreadNotifier.ReplyInThread`, `ThreadHandler.HandleMention` are each declared once and referenced with the same signature everywhere. `Register(root, channel string, inv providers.Investigation)` matches between the `notify.ThreadSink` declaration (Task 6) and the `*thread.Registry` implementation (Task 1). `HandleMention(ctx, channel, root, author, text)` matches between `server.ThreadHandler` (Task 8) and `thread.Mention` (Task 5) — note the argument order is channel-then-root in both.

**Two places the plan tells you to verify rather than trust it:** Task 3 Step 3 (`kbvalidate.Issue` field names) and Task 8 Step 3 (`server.New` signature). Both are read-only checks against the real code; if they disagree with the plan, the code wins.
