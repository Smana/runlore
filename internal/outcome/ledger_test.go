package outcome

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// replayOpenCounts is an independent oracle: it computes the per-entry recall
// aggregate by replaying every event in the file from scratch, mirroring the
// documented Episodes()→OpenCounts() semantics (LIFO pairing, order-independent
// resolve-before-open buffering). The cached OpenCounts() must equal this for
// any event sequence.
func replayOpenCounts(t *testing.T, path string) map[string]Aggregate {
	t.Helper()
	ref, err := New(path)
	if err != nil {
		t.Fatalf("oracle New: %v", err)
	}
	eps, err := ref.Episodes() // Episodes still replays the file, so it is a fresh source of truth
	if err != nil {
		t.Fatalf("oracle Episodes: %v", err)
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
	return counts
}

func assertCacheEqualsReplay(t *testing.T, l *Ledger, path string) {
	t.Helper()
	cached, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	want := replayOpenCounts(t, path)
	if len(cached) != len(want) {
		t.Fatalf("cache != replay (entries %d vs %d)\n cached=%+v\n replay=%+v", len(cached), len(want), cached, want)
	}
	// Compare field-by-field. LastConfirmed is a time.Time and MUST be compared by
	// instant (.Equal), not reflect.DeepEqual: the cache holds in-memory (time.Local)
	// times while the replay holds JSON-parsed times, so their *Location pointers differ
	// (e.g. time.Local vs time.UTC when the process runs in UTC, as CI does) even when
	// the instants are identical — DeepEqual would spuriously fail.
	for k, w := range want {
		got, ok := cached[k]
		if !ok {
			t.Fatalf("cache missing entry %q\n cached=%+v\n replay=%+v", k, cached, want)
		}
		if got.Recalls != w.Recalls || got.Resolved != w.Resolved || !got.LastConfirmed.Equal(w.LastConfirmed) {
			t.Fatalf("cache != replay for %q: cached=%+v replay=%+v", k, got, w)
		}
	}
}

// TestOpenCountsCacheEqualsReplaySequence drives a varied open/resolve sequence
// (recurrence, multiple entries, resolve-before-open, fresh, interleaving) and
// asserts the live cached OpenCounts() equals a from-scratch replay of the file.
func TestOpenCountsCacheEqualsReplaySequence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(10000, 0)
	at := func(d int) time.Time { return t0.Add(time.Duration(d) * time.Second) }

	// recurrence on one entry: 3 opens, 1 resolve (LIFO ⇒ exactly 1 resolved)
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "x.md", At: at(0)})
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "x.md", At: at(1)})
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "x.md", At: at(2)})
	_, _, _ = l.Resolve("fp1", at(5))
	// a fresh open (must never appear in counts)
	_ = l.Open(Event{Fingerprint: "fr", Kind: "fresh", At: at(6)})
	_, _, _ = l.Resolve("fr", at(7))
	// second entry, resolved once
	_ = l.Open(Event{Fingerprint: "fp2", Kind: "recall", Entry: "y.md", At: at(8)})
	_, _, _ = l.Resolve("fp2", at(9))
	// resolve-before-open: the resolve lands first, then the open pairs with it
	_, _, _ = l.Resolve("fp3", at(10))
	_ = l.Open(Event{Fingerprint: "fp3", Kind: "recall", Entry: "z.md", At: at(11)})
	// unresolved recall
	_ = l.Open(Event{Fingerprint: "fp4", Kind: "recall", Entry: "w.md", At: at(12)})

	assertCacheEqualsReplay(t, l, p)

	// Spot-check the documented shape too, not only DeepEqual against the oracle.
	counts, _ := l.OpenCounts()
	if got := counts["x.md"]; got.Recalls != 3 || got.Resolved != 1 || !got.LastConfirmed.Equal(at(5)) {
		t.Fatalf("x.md = %+v", got)
	}
	if got := counts["z.md"]; got.Recalls != 1 || got.Resolved != 1 {
		t.Fatalf("resolve-before-open z.md = %+v", got)
	}
	if got := counts["w.md"]; got.Recalls != 1 || got.Resolved != 0 {
		t.Fatalf("unresolved w.md = %+v", got)
	}
	if _, ok := counts["fresh"]; ok {
		t.Fatalf("fresh must not be counted: %+v", counts)
	}
}

// TestOpenCountsCacheReflectsAppendsAfterConstruction ensures the cache is
// updated INCREMENTALLY on appends made after New(), not only at load.
func TestOpenCountsCacheReflectsAppendsAfterConstruction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p) // constructed over an empty/absent file
	if c, _ := l.OpenCounts(); len(c) != 0 {
		t.Fatalf("fresh ledger must start empty, got %+v", c)
	}
	t0 := time.Unix(11000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	if c, _ := l.OpenCounts(); c["a.md"].Recalls != 1 || c["a.md"].Resolved != 0 {
		t.Fatalf("after Open, want recalls=1 resolved=0, got %+v", c["a.md"])
	}
	_, _, _ = l.Resolve("fp", t0.Add(time.Minute))
	if c, _ := l.OpenCounts(); c["a.md"].Resolved != 1 || !c["a.md"].LastConfirmed.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after Resolve, want resolved=1 with LastConfirmed bumped, got %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, l, p)
}

// TestOpenCountsCacheBuiltFromExistingFile ensures opening a ledger over a file
// that already has events builds the cache once at construction (no append needed).
func TestOpenCountsCacheBuiltFromExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	seed, _ := New(p)
	t0 := time.Unix(12000, 0)
	_ = seed.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	_ = seed.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0.Add(time.Second)})
	_, _, _ = seed.Resolve("fp", t0.Add(5*time.Second))

	reopened, err := New(p)
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	c, _ := reopened.OpenCounts()
	if c["a.md"].Recalls != 2 || c["a.md"].Resolved != 1 || !c["a.md"].LastConfirmed.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("reopened cache from file = %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, reopened, p)
}

// TestOpenCountsCacheSkipsCorruptLinesOnLoad ensures the initial cache build
// skips corrupt JSONL lines (matching readEvents' tolerance) rather than failing.
func TestOpenCountsCacheSkipsCorruptLinesOnLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	t0 := time.Unix(13000, 0)
	good1, _ := json.Marshal(Event{Event: "open", Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	good2, _ := json.Marshal(Event{Event: "resolve", Fingerprint: "fp", At: t0.Add(time.Minute)})
	content := string(good1) + "\n" + "this is not json{{{\n" + string(good2) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	l, err := New(p)
	if err != nil {
		t.Fatalf("New over file with a corrupt line must not fail: %v", err)
	}
	c, _ := l.OpenCounts()
	if c["a.md"].Recalls != 1 || c["a.md"].Resolved != 1 {
		t.Fatalf("corrupt line must be skipped, good events counted: %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, l, p)
}

// TestOpenCountsCacheRandomizedEqualsReplay fuzzes many random open/resolve
// sequences (across fingerprints and entries, including resolve-before-open) and
// asserts the cached OpenCounts() always equals a from-scratch replay.
func TestOpenCountsCacheRandomizedEqualsReplay(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	fps := []string{"f0", "f1", "f2"}
	entries := []string{"a.md", "b.md", ""} // "" ⇒ fresh-style open
	t0 := time.Unix(14000, 0)
	for trial := 0; trial < 40; trial++ {
		p := filepath.Join(t.TempDir(), fmt.Sprintf("o%d.jsonl", trial))
		l, _ := New(p)
		n := 5 + rng.Intn(25)
		for i := 0; i < n; i++ {
			fp := fps[rng.Intn(len(fps))]
			at := t0.Add(time.Duration(i) * time.Second)
			if rng.Intn(2) == 0 {
				entry := entries[rng.Intn(len(entries))]
				kind := "recall"
				if entry == "" {
					kind = "fresh"
				}
				_ = l.Open(Event{Fingerprint: fp, Kind: kind, Entry: entry, At: at})
			} else {
				_, _, _ = l.Resolve(fp, at)
			}
		}
		assertCacheEqualsReplay(t, l, p)
	}
}

// TestOpenCountsCacheConcurrent exercises concurrent Open/Resolve and OpenCounts
// to catch data races (run under -race). It asserts the final cached aggregate
// equals a from-scratch replay of the file.
func TestOpenCountsCacheConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(15000, 0)
	const writers = 8
	const perWriter = 50
	var writersWG, readersWG sync.WaitGroup

	// readers hammer OpenCounts concurrently with the writers, until stop is closed
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := l.OpenCounts(); err != nil {
						t.Errorf("OpenCounts: %v", err)
						return
					}
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			fp := fmt.Sprintf("fp%d", w)
			entry := fmt.Sprintf("e%d.md", w)
			for i := 0; i < perWriter; i++ {
				at := t0.Add(time.Duration(w*perWriter+i) * time.Second)
				_ = l.Open(Event{Fingerprint: fp, Kind: "recall", Entry: entry, At: at})
				_, _, _ = l.Resolve(fp, at.Add(time.Millisecond))
			}
		}(w)
	}

	writersWG.Wait() // all appends done
	close(stop)      // let readers exit
	readersWG.Wait()

	assertCacheEqualsReplay(t, l, p)
	c, _ := l.OpenCounts()
	for w := 0; w < writers; w++ {
		entry := fmt.Sprintf("e%d.md", w)
		if c[entry].Recalls != perWriter || c[entry].Resolved != perWriter {
			t.Fatalf("%s: want recalls=%d resolved=%d, got %+v", entry, perWriter, perWriter, c[entry])
		}
	}
}

// TestReloadResyncsExternalWrites simulates multi-replica HA failover: a SECOND
// Ledger pointed at the SAME file (another replica's leader) appends opens/resolves
// while the first instance is a follower. The first instance's incrementally-built
// cache stays stale until Reload re-replays the shared file — exactly what a
// re-acquired leader must do so its recall-decay aggregate is not stale.
func TestReloadResyncsExternalWrites(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	a, err := New(p) // "pod A": builds its cache over the (empty) file
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	if c, _ := a.OpenCounts(); len(c) != 0 {
		t.Fatalf("pod A must start empty, got %+v", c)
	}

	// "pod B": another replica writes to the shared file while A is a follower.
	b, err := New(p)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	t0 := time.Unix(16000, 0)
	_ = b.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	_, _, _ = b.Resolve("fp", t0.Add(time.Minute))

	// A's cache never saw B's writes, so it is stale.
	if c, _ := a.OpenCounts(); c["a.md"].Recalls != 0 {
		t.Fatalf("pre-Reload pod A must be stale (recalls=0), got %+v", c["a.md"])
	}

	// Re-acquiring leadership triggers Reload, re-syncing with B's external writes.
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if c, _ := a.OpenCounts(); c["a.md"].Recalls != 1 || c["a.md"].Resolved != 1 || !c["a.md"].LastConfirmed.Equal(t0.Add(time.Minute)) {
		t.Fatalf("post-Reload pod A must reflect B's writes, got %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, a, p)

	// Reload rebuilds the open-index too, so A can resolve an open B recorded.
	_ = b.Open(Event{Fingerprint: "fp2", Kind: "recall", Entry: "c.md", At: t0.Add(2 * time.Minute)})
	if err := a.Reload(); err != nil {
		t.Fatalf("Reload 2: %v", err)
	}
	if _, ok, _ := a.Resolve("fp2", t0.Add(3*time.Minute)); !ok {
		t.Fatal("post-Reload pod A must see B's open in its rebuilt index")
	}
}

// TestReloadErrorPreservesPriorCache is a regression test for the ordering bug where
// loadLocked wiped the cache maps before calling readEvents: if readEvents fails the
// cache was left empty rather than preserving the pre-Reload state. This test appends
// a line longer than the bufio.Scanner buffer (>1 MiB) so readEvents returns
// bufio.ErrTooLong, then asserts that Reload returns a non-nil error AND that
// OpenCounts still equals the pre-Reload value (not empty).
func TestReloadErrorPreservesPriorCache(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(20000, 0)
	// Populate the ledger so OpenCounts is non-empty.
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "a.md", At: t0})
	_ = l.Open(Event{Fingerprint: "fp2", Kind: "recall", Entry: "b.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("fp1", t0.Add(2*time.Second))

	// Capture the pre-Reload cache.
	before, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts before: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("pre-condition: OpenCounts must be non-empty before Reload")
	}

	// Append a line >1 MiB (the Scanner max token size) to make readEvents fail with
	// bufio.ErrTooLong. This is uid-independent (no chmod needed).
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open ledger file for poisoning: %v", err)
	}
	// 1 MiB + 1 byte of non-JSON to exceed the scanner buffer.
	giant := make([]byte, 1024*1024+1)
	for i := range giant {
		giant[i] = 'x'
	}
	giant[len(giant)-1] = '\n'
	if _, err := f.Write(giant); err != nil {
		_ = f.Close()
		t.Fatalf("write giant line: %v", err)
	}
	_ = f.Close()

	// Reload must return a non-nil error.
	reloadErr := l.Reload()
	if reloadErr == nil {
		t.Fatal("Reload over a >1 MiB line must return a non-nil error")
	}

	// Cache must be unchanged — not empty.
	after, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts after failed Reload: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed Reload must preserve prior cache: before=%+v after=%+v", before, after)
	}
	for k, w := range before {
		got, ok := after[k]
		if !ok {
			t.Fatalf("entry %q missing from cache after failed Reload: before=%+v after=%+v", k, before, after)
		}
		if got.Recalls != w.Recalls || got.Resolved != w.Resolved || !got.LastConfirmed.Equal(w.LastConfirmed) {
			t.Fatalf("cache mismatch for %q after failed Reload: before=%+v after=%+v", k, w, got)
		}
	}
}

// TestReloadDisabledOrNilNoop ensures Reload is a safe no-op on a disabled (path=="")
// or nil ledger — the leadership wiring calls it unconditionally.
func TestReloadDisabledOrNilNoop(t *testing.T) {
	dis, _ := New("")
	if err := dis.Reload(); err != nil {
		t.Fatalf("disabled Reload must be a no-op: %v", err)
	}
	var nilLedger *Ledger
	if err := nilLedger.Reload(); err != nil {
		t.Fatalf("nil Reload must be a no-op: %v", err)
	}
}

// TestPendingResolvesBounded verifies the defensive per-fingerprint cap on buffered
// orphan resolves: far more spurious resolves than the cap stay bounded and the excess
// is counted, while the legitimate resolve-before-open pairing still works.
func TestPendingResolvesBounded(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(17000, 0)
	// Many orphan resolves for one fingerprint (e.g. duplicate/replayed resolve
	// webhooks whose open was never recorded).
	n := maxPendingResolvesPerFingerprint + 100
	for i := 0; i < n; i++ {
		_, _, _ = l.Resolve("orphan", t0.Add(time.Duration(i)*time.Second))
	}
	l.mu.Lock()
	got := len(l.pendingResolves["orphan"])
	dropped := l.droppedResolves
	l.mu.Unlock()
	if got > maxPendingResolvesPerFingerprint {
		t.Fatalf("pendingResolves must be bounded at %d, got %d", maxPendingResolvesPerFingerprint, got)
	}
	if dropped == 0 {
		t.Fatalf("excess orphan resolves must be counted as dropped, got 0")
	}
	// The brief-window case still pairs: a buffered resolve pairs with a later open.
	_ = l.Open(Event{Fingerprint: "orphan", Kind: "recall", Entry: "z.md", At: t0.Add(time.Duration(n) * time.Second)})
	if c, _ := l.OpenCounts(); c["z.md"].Recalls != 1 || c["z.md"].Resolved != 1 {
		t.Fatalf("a buffered resolve must still pair with a later open: %+v", c["z.md"])
	}
}

// TestDeriveFingerprintStableAndPrefixed pins the synthetic fingerprint derivation
// used for sources with no external alert id (GitOps failures, reinvestigate polls):
// it is deterministic (same key ⇒ same id, so recurrences roll up), carries the given
// prefix, distinguishes distinct keys, and is recognised by Derived().
func TestDeriveFingerprintStableAndPrefixed(t *testing.T) {
	a := DeriveFingerprint(GitOpsFingerprintPrefix, "argocd/airflow:Degraded")
	b := DeriveFingerprint(GitOpsFingerprintPrefix, "argocd/airflow:Degraded")
	if a != b {
		t.Fatalf("derivation must be deterministic: %q != %q", a, b)
	}
	if len(a) <= len(GitOpsFingerprintPrefix) || a[:len(GitOpsFingerprintPrefix)] != GitOpsFingerprintPrefix {
		t.Fatalf("derived id %q must carry the %q prefix", a, GitOpsFingerprintPrefix)
	}
	if c := DeriveFingerprint(GitOpsFingerprintPrefix, "other/thing:Failed"); c == a {
		t.Fatalf("distinct keys must derive distinct fingerprints, both %q", c)
	}
	if !Derived(a) {
		t.Fatalf("a gitops-derived fingerprint must be reported Derived: %q", a)
	}
	if !Derived(DeriveFingerprint(ReinvestigateFingerprintPrefix, "issue-7")) {
		t.Fatal("a reinvestigate-derived fingerprint must be reported Derived")
	}
	if Derived("f0e1a2b3") { // a real Alertmanager fingerprint (opaque hex, no prefix)
		t.Fatal("a real Alertmanager fingerprint must NOT be reported Derived")
	}
}

// boolPtr is a test helper for the tri-state Resolvable field.
func boolPtr(b bool) *bool { return &b }

// TestNonResolvableRecallOpenNotCountedButInEpisodes pins Defect 2: a recall open from
// a source with NO resolve channel (Resolvable=false — GitOps / send_resolved off) must
// NOT increment Recalls (so a correct entry's resolve-rate can't decay on evidence that
// can never arrive), yet it MUST still appear in Episodes so recurrence counting keeps
// working.
func TestNonResolvableRecallOpenNotCountedButInEpisodes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(21000, 0)
	// A resolvable recall (alert) — counts toward decay.
	_ = l.Open(Event{Fingerprint: "amfp", Kind: "recall", Entry: "x.md", Resolvable: boolPtr(true), At: t0})
	// A non-resolvable recall (gitops) — must NOT count toward decay.
	_ = l.Open(Event{Fingerprint: "gitops:abc", Kind: "recall", Entry: "x.md", Resolvable: boolPtr(false), At: t0.Add(time.Second)})

	c, _ := l.OpenCounts()
	if c["x.md"].Recalls != 1 {
		t.Fatalf("only the resolvable recall must count toward Recalls, got %+v", c["x.md"])
	}

	// Both opens must still be present as episodes (recurrence counting is unaffected).
	eps, _ := l.Episodes()
	if len(eps) != 2 {
		t.Fatalf("both opens (resolvable and not) must appear in Episodes, got %d", len(eps))
	}
	// And the property survives a reload (the cache is rebuilt from the file).
	l2, _ := New(p)
	if c2, _ := l2.OpenCounts(); c2["x.md"].Recalls != 1 {
		t.Fatalf("after reload, non-resolvable recall must still not count, got %+v", c2["x.md"])
	}
}

// TestLegacyOpenMissingResolvableCountsAsResolvable pins backward compatibility: an
// open written by an OLD binary has no "resolvable" field (nil on decode). It came from
// Alertmanager/PagerDuty, which do emit resolves, so it must be treated as resolvable
// and count toward Recalls exactly as before.
func TestLegacyOpenMissingResolvableCountsAsResolvable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	// A legacy open line: no "resolvable" key at all.
	legacy := `{"event":"open","fingerprint":"fp","kind":"recall","entry":"x.md","at":"2026-07-01T10:00:00Z"}` + "\n"
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c, _ := l.OpenCounts(); c["x.md"].Recalls != 1 {
		t.Fatalf("a legacy open (no resolvable field) must count as resolvable, got %+v", c["x.md"])
	}
}

// countLines returns the number of JSONL lines in the file, and how many are
// checkpoint records.
func countLines(t *testing.T, path string) (total, checkpoints int) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		total++
		var e Event
		if json.Unmarshal([]byte(line), &e) == nil && e.Event == "checkpoint" {
			checkpoints++
		}
	}
	return total, checkpoints
}

// TestCompactionPreservesAggregatesAndUnresolvedOpens pins Defect 3: when the file
// exceeds max_events it is compacted on reload into a checkpoint + a recent tail, and
// the reloaded state must be IDENTICAL — per-entry aggregates (OpenCounts), Occurrences,
// and any still-unresolved open (which must remain resolvable). The file must actually
// shrink and gain exactly one checkpoint record.
func TestCompactionPreservesAggregatesAndUnresolvedOpens(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := NewWithMaxEvents(p, 8) // small bound to force compaction on reload
	t0 := time.Unix(30000, 0)
	at := func(d int) time.Time { return t0.Add(time.Duration(d) * time.Second) }

	// Six resolved recall pairs (12 events) on entry x.md under trigger key "k".
	for i := 0; i < 6; i++ {
		fp := fmt.Sprintf("fp%d", i)
		_ = l.Open(Event{Fingerprint: fp, Kind: "recall", Entry: "x.md", TriggerKey: "k", Resolvable: boolPtr(true), At: at(i * 2)})
		_, _, _ = l.Resolve(fp, at(i*2+1))
	}
	// One still-unresolved recall open that must survive compaction.
	_ = l.Open(Event{Fingerprint: "live", Kind: "recall", Entry: "x.md", Resolvable: boolPtr(true), At: at(100)})

	before, _ := l.OpenCounts()
	if before["x.md"].Recalls != 7 || before["x.md"].Resolved != 6 {
		t.Fatalf("pre-condition: want recalls=7 resolved=6, got %+v", before["x.md"])
	}
	nOcc, lastOcc, _ := l.Occurrences("k")

	// Reload from the file — this triggers compaction (13 raw events > bound 8).
	l2, err := NewWithMaxEvents(p, 8)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The file was compacted: fewer lines, exactly one checkpoint.
	total, ckpts := countLines(t, p)
	if ckpts != 1 {
		t.Fatalf("compacted file must carry exactly one checkpoint, got %d (total lines %d)", ckpts, total)
	}
	if total > 8 {
		t.Fatalf("compacted file must be bounded at max_events (8), got %d lines", total)
	}

	// Aggregates are identical across the compaction round-trip.
	after, _ := l2.OpenCounts()
	if after["x.md"].Recalls != before["x.md"].Recalls ||
		after["x.md"].Resolved != before["x.md"].Resolved ||
		!after["x.md"].LastConfirmed.Equal(before["x.md"].LastConfirmed) {
		t.Fatalf("compaction changed aggregates: before=%+v after=%+v", before["x.md"], after["x.md"])
	}
	// Occurrences survive too.
	if n, last, _ := l2.Occurrences("k"); n != nOcc || !last.Equal(lastOcc) {
		t.Fatalf("compaction changed Occurrences: before=(%d,%v) after=(%d,%v)", nOcc, lastOcc, n, last)
	}
	// The still-unresolved open survives: a resolve for it still pairs.
	if _, ok, _ := l2.Resolve("live", at(200)); !ok {
		t.Fatal("an unresolved open must survive compaction (still resolvable after reload)")
	}

	// A SECOND reload (now over the compacted file: checkpoint + tail) is still correct —
	// the checkpoint round-trips through another compaction cycle.
	l3, err := NewWithMaxEvents(p, 8)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if c := (mustCounts(t, l3))["x.md"]; c.Recalls != 7 {
		t.Fatalf("after second reload, aggregates must still hold, got %+v", c)
	}
}

func mustCounts(t *testing.T, l *Ledger) map[string]Aggregate {
	t.Helper()
	c, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	return c
}

// TestCompactionDisabledWhenZero ensures max_events=0 disables compaction: the file is
// left fully intact (no checkpoint) however large it grows.
func TestCompactionDisabledWhenZero(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := NewWithMaxEvents(p, 0) // disabled
	t0 := time.Unix(31000, 0)
	for i := 0; i < 20; i++ {
		fp := fmt.Sprintf("fp%d", i)
		_ = l.Open(Event{Fingerprint: fp, Kind: "recall", Entry: "x.md", Resolvable: boolPtr(true), At: t0.Add(time.Duration(i) * time.Second)})
		_, _, _ = l.Resolve(fp, t0.Add(time.Duration(i)*time.Second+time.Millisecond))
	}
	if _, err := NewWithMaxEvents(p, 0); err != nil {
		t.Fatalf("reload: %v", err)
	}
	total, ckpts := countLines(t, p)
	if ckpts != 0 {
		t.Fatalf("compaction disabled must write no checkpoint, got %d", ckpts)
	}
	if total != 40 {
		t.Fatalf("compaction disabled must keep all 40 events, got %d", total)
	}
}

// TestCorruptLineCountedNotSilent pins that a corrupt JSONL line is surfaced (counted),
// not silently dropped — the good events still load.
func TestCorruptLineCountedNotSilent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	t0 := time.Unix(32000, 0)
	good1, _ := json.Marshal(Event{Event: "open", Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	good2, _ := json.Marshal(Event{Event: "resolve", Fingerprint: "fp", At: t0.Add(time.Minute)})
	content := string(good1) + "\nnot json at all{{{\n" + string(good2) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.mu.Lock()
	corrupt := l.corruptLines
	l.mu.Unlock()
	if corrupt != 1 {
		t.Fatalf("want exactly 1 corrupt line counted, got %d", corrupt)
	}
	if c, _ := l.OpenCounts(); c["a.md"].Recalls != 1 {
		t.Fatalf("good events must still load around the corrupt line, got %+v", c["a.md"])
	}
}

func TestStatusDisabled(t *testing.T) {
	l, _ := New("")
	s := l.Status()
	if s.Configured {
		t.Fatalf("empty path must be Configured=false: %+v", s)
	}
	if s.Present || s.Events != 0 {
		t.Fatalf("disabled ledger: want Present=false Events=0, got %+v", s)
	}
}

func TestStatusConfiguredButAbsent(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "missing.jsonl"))
	s := l.Status()
	if !s.Configured {
		t.Fatalf("a non-empty path must be Configured=true: %+v", s)
	}
	if s.Present {
		t.Fatalf("an absent file must be Present=false (this is the silent no-op the warning catches): %+v", s)
	}
	if s.Events != 0 {
		t.Fatalf("absent file: want Events=0, got %d", s.Events)
	}
}

func TestStatusPresentWithEvents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(1000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0})
	_, _, _ = l.Resolve("fp", t0.Add(time.Minute))
	s := l.Status()
	if !s.Configured || !s.Present {
		t.Fatalf("a written ledger must be Configured && Present: %+v", s)
	}
	if s.Events != 2 {
		t.Fatalf("want Events=2 (1 open + 1 resolve), got %d", s.Events)
	}
}

func TestStatusPresentButEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	l, _ := New(p)
	s := l.Status()
	if !s.Configured || !s.Present {
		t.Fatalf("an existing (empty) file is Present: %+v", s)
	}
	if s.Events != 0 {
		t.Fatalf("empty file: want Events=0, got %d", s.Events)
	}
}

func TestLedgerOpenResolveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(1000, 0)
	if err := l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "harbor.md", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ep, ok, err := l.Resolve("fp1", t0.Add(90*time.Second))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if ep.Kind != "recall" || ep.Entry != "harbor.md" || ep.Duration != 90*time.Second || !ep.Resolved {
		t.Fatalf("episode: %+v", ep)
	}
}

func TestLedgerResolveWithoutOpen(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if _, ok, err := l.Resolve("never-fired", time.Unix(1, 0)); ok || err != nil {
		t.Fatalf("resolve with no open should be ok=false, got ok=%v err=%v", ok, err)
	}
}

func TestLedgerReplayRebuildsOpenIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(2000, 0)
	_ = l.Open(Event{Fingerprint: "fpA", Kind: "fresh", At: t0})
	// New ledger over the same file replays the open event.
	l2, err := New(p)
	if err != nil {
		t.Fatalf("replay New: %v", err)
	}
	if _, ok, _ := l2.Resolve("fpA", t0.Add(time.Minute)); !ok {
		t.Fatal("replay must rebuild the open-index so fpA resolves")
	}
}

func TestLedgerDisabledWhenPathEmpty(t *testing.T) {
	l, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if err := l.Open(Event{Fingerprint: "x"}); err != nil {
		t.Fatalf("Open on disabled ledger must be a no-op, got %v", err)
	}
	if _, ok, _ := l.Resolve("x", time.Now()); ok {
		t.Fatal("disabled ledger Resolve must be ok=false")
	}
}

func TestReadEventsReturnsAllInOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(3000, 0)
	_ = l.Open(Event{Fingerprint: "a", Kind: "fresh", At: t0})
	_ = l.Open(Event{Fingerprint: "b", Kind: "recall", Entry: "x.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("a", t0.Add(2*time.Second))
	events, _, err := l.readEvents()
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Event != "open" || events[0].Fingerprint != "a" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[2].Event != "resolve" || events[2].Fingerprint != "a" {
		t.Fatalf("event[2] = %+v", events[2])
	}
}

func TestReadEventsDisabledOrAbsent(t *testing.T) {
	dis, _ := New("")
	if ev, corrupt, err := dis.readEvents(); err != nil || ev != nil || corrupt != 0 {
		t.Fatalf("disabled: want nil,0,nil; got %v,%v,%v", ev, corrupt, err)
	}
	absent, _ := New(filepath.Join(t.TempDir(), "missing.jsonl"))
	if ev, corrupt, err := absent.readEvents(); err != nil || ev != nil || corrupt != 0 {
		t.Fatalf("absent file: want nil,0,nil; got %v,%v,%v", ev, corrupt, err)
	}
}

func TestResolveMarksEpisodeResolved(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(4000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", At: t0})
	ep, ok, err := l.Resolve("fp", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if !ep.Resolved {
		t.Fatalf("a matched resolve must set Resolved=true: %+v", ep)
	}
}

func TestEpisodesReconstructsRecurrence(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(5000, 0)
	for i := 0; i < 3; i++ {
		_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Duration(i) * time.Second)})
	}
	_, _, _ = l.Resolve("fp", t0.Add(10*time.Second))
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("want 3 episodes (recurrence preserved), got %d", len(eps))
	}
	resolved := 0
	for _, e := range eps {
		if e.Resolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("want exactly 1 resolved episode, got %d", resolved)
	}
}

func TestEpisodesResolvedPairingAndDuration(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(6000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "first.md", At: t0})
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "second.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("fp", t0.Add(5*time.Second))
	eps, _ := l.Episodes()
	var second Episode
	for _, e := range eps {
		if e.Entry == "first.md" && e.Resolved {
			t.Fatal("LIFO: the earlier open must remain unresolved")
		}
		if e.Entry == "second.md" {
			second = e
		}
	}
	if !second.Resolved || second.Duration != 4*time.Second {
		t.Fatalf("most-recent open should resolve with 4s duration: %+v", second)
	}
}

func TestEpisodesEmptyAndDisabled(t *testing.T) {
	dis, _ := New("")
	if eps, err := dis.Episodes(); err != nil || eps != nil {
		t.Fatalf("disabled: want nil,nil; got %v,%v", eps, err)
	}
	empty, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if eps, err := empty.Episodes(); err != nil || len(eps) != 0 {
		t.Fatalf("empty ledger: want 0 episodes; got %v,%v", eps, err)
	}
}

// TestEpisodesResolveBeforeOpenPairs verifies the new order-independent pairing:
// a resolve written BEFORE its open (a transient incident that cleared
// mid-investigation, so the resolve webhook landed before the open was recorded)
// now PAIRS with the later open — the open becomes a resolved episode.
func TestEpisodesResolveBeforeOpenPairs(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(9000, 0)
	_, _, _ = l.Resolve("fp", t0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Second)})
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || !eps[0].Resolved {
		t.Fatalf("resolve-before-open must pair: want 1 resolved episode, got %+v", eps)
	}
	// Resolve predates the open here, so Duration is guarded to 0 (never negative).
	if eps[0].Duration != 0 {
		t.Fatalf("duration must be guarded non-negative when resolve predates open: %v", eps[0].Duration)
	}
}

// TestEpisodesCoalescedMultiFingerprintResolves checks the ledger-level shape of
// a coalesced attribution: opening the same Title/Kind for several constituent
// fingerprints, then a resolve for any one of them, marks that episode resolved.
func TestEpisodesCoalescedMultiFingerprintResolves(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(9500, 0)
	for _, fp := range []string{"a", "b", "c"} {
		if err := l.Open(Event{Fingerprint: fp, Kind: "recall", Entry: "x.md", Title: "BatchAlert", At: t0}); err != nil {
			t.Fatalf("Open(%s): %v", fp, err)
		}
	}
	ep, ok, err := l.Resolve("c", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve(c): want ok=true, got ok=%v err=%v", ok, err)
	}
	if !ep.Resolved || ep.Title != "BatchAlert" {
		t.Fatalf("constituent resolve must mark its episode resolved: %+v", ep)
	}
}

func TestEpisodesTwoFingerprintsInterleaved(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(9100, 0)
	_ = l.Open(Event{Fingerprint: "A", Kind: "recall", Entry: "a.md", At: t0})
	_ = l.Open(Event{Fingerprint: "B", Kind: "recall", Entry: "b.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("A", t0.Add(2*time.Second))
	eps, _ := l.Episodes()
	if len(eps) != 2 {
		t.Fatalf("want 2 episodes, got %d", len(eps))
	}
	for _, e := range eps {
		if e.Entry == "a.md" && !e.Resolved {
			t.Fatalf("A should be resolved: %+v", e)
		}
		if e.Entry == "b.md" && e.Resolved {
			t.Fatalf("B should remain unresolved: %+v", e)
		}
	}
}

func TestOpenCountsAggregatesRecallEntries(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7000, 0)
	for i := 0; i < 3; i++ {
		_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Duration(i) * time.Second)})
	}
	resolveAt := t0.Add(10 * time.Second)
	_, _, _ = l.Resolve("fp", resolveAt)
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	a := counts["x.md"]
	if a.Recalls != 3 || a.Resolved != 1 {
		t.Fatalf("want recalls=3 resolved=1, got %+v", a)
	}
	if !a.LastConfirmed.Equal(resolveAt) {
		t.Fatalf("LastConfirmed = %v, want %v", a.LastConfirmed, resolveAt)
	}
}

func TestOpenCountsSkipsFresh(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(8000, 0)
	_ = l.Open(Event{Fingerprint: "f1", Kind: "fresh", At: t0}) // fresh → no entry
	_, _, _ = l.Resolve("f1", t0.Add(time.Minute))
	counts, _ := l.OpenCounts()
	if len(counts) != 0 {
		t.Fatalf("fresh opens must not appear in OpenCounts, got %+v", counts)
	}
}

func TestOpenCountsEmptyMapWhenDisabled(t *testing.T) {
	dis, _ := New("")
	counts, err := dis.OpenCounts()
	if err != nil || counts == nil || len(counts) != 0 {
		t.Fatalf("disabled: want empty non-nil map; got %v,%v", counts, err)
	}
}

func TestOpenCountsUnresolvedRecallCounted(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7100, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0}) // never resolved
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	a := counts["x.md"]
	if a.Recalls != 1 || a.Resolved != 0 {
		t.Fatalf("unresolved recall: want recalls=1 resolved=0, got %+v", a)
	}
	if !a.LastConfirmed.IsZero() {
		t.Fatalf("unresolved recall: LastConfirmed must be zero, got %v", a.LastConfirmed)
	}
}

func TestOpenCountsMultipleEntries(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7200, 0)
	_ = l.Open(Event{Fingerprint: "A", Kind: "recall", Entry: "a.md", At: t0})
	_ = l.Open(Event{Fingerprint: "B", Kind: "recall", Entry: "b.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("A", t0.Add(2*time.Second))
	counts, _ := l.OpenCounts()
	if len(counts) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(counts), counts)
	}
	if counts["a.md"].Resolved != 1 || counts["a.md"].Recalls != 1 {
		t.Fatalf("a.md should be recalls=1 resolved=1: %+v", counts["a.md"])
	}
	if counts["b.md"].Recalls != 1 || counts["b.md"].Resolved != 0 {
		t.Fatalf("b.md should be recalls=1 resolved=0: %+v", counts["b.md"])
	}
}

// TestOccurrencesByTriggerKey verifies the byTrigger index counts prior opens per
// TriggerKey and surfaces the newest one's time + curated URL — the recurrence facts
// the notifier renders — and that the index survives a replay (restart / failover).
func TestOccurrencesByTriggerKey(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	t1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(6 * time.Hour)
	_ = l.Open(Event{Fingerprint: "f1", TriggerKey: "k", CuratedURL: "https://kb/1", At: t1})
	_ = l.Open(Event{Fingerprint: "f2", TriggerKey: "k", CuratedURL: "https://kb/2", At: t2})
	_ = l.Open(Event{Fingerprint: "f3", TriggerKey: "other", At: t2})
	n, last, url := l.Occurrences("k")
	if n != 2 || !last.Equal(t2) || url != "https://kb/2" {
		t.Fatalf("Occurrences = %d %v %q", n, last, url)
	}
	// index survives a replay (restart)
	l2, _ := New(l.path)
	if n, _, _ := l2.Occurrences("k"); n != 2 {
		t.Fatalf("after replay: %d", n)
	}
}

// TestOccurrencesEmptyKeyAndDisabled pins the zero-value cases: a disabled ledger,
// an empty key, and a never-seen key all report (0, zero, "").
func TestOccurrencesEmptyKeyAndDisabled(t *testing.T) {
	dis, _ := New("")
	if n, last, url := dis.Occurrences("k"); n != 0 || !last.IsZero() || url != "" {
		t.Fatalf("disabled: want 0,zero,\"\"; got %d %v %q", n, last, url)
	}
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if n, last, url := l.Occurrences(""); n != 0 || !last.IsZero() || url != "" {
		t.Fatalf("empty key: want 0,zero,\"\"; got %d %v %q", n, last, url)
	}
	if n, _, _ := l.Occurrences("never-seen"); n != 0 {
		t.Fatalf("never-seen key: want 0, got %d", n)
	}
}

// TestLedgerEnabled pins the exported wiring predicate: only a ledger with a
// configured path reports Enabled, and a nil receiver is safe (mirrors enabled()).
func TestLedgerEnabled(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !l.Enabled() {
		t.Fatal("configured ledger must report Enabled")
	}
	dis, _ := New("")
	if dis.Enabled() {
		t.Fatal("disabled ledger (empty path) must not report Enabled")
	}
	var nilLedger *Ledger
	if nilLedger.Enabled() {
		t.Fatal("nil ledger must not report Enabled")
	}
}

func TestEpisodeCarriesDupFingerprint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(2000, 0)
	if err := l.Open(Event{Fingerprint: "fp1", DupFingerprint: "dup-abc", Title: "T", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// live Resolve carries it through
	ep, ok, err := l.Resolve("fp1", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if ep.DupFingerprint != "dup-abc" {
		t.Fatalf("Resolve episode dup = %q, want dup-abc", ep.DupFingerprint)
	}
	// replayed Episodes() carries it too
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || eps[0].DupFingerprint != "dup-abc" {
		t.Fatalf("Episodes dup = %+v, want one with dup-abc", eps)
	}
}

// TestFeedbackValidatesRatingAndDisabledNoop pins the Feedback contract edges: an
// unknown rating is an error (never silently recorded), and a disabled ledger
// no-ops like every other write.
func TestFeedbackValidatesRatingAndDisabledNoop(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if err := l.Feedback("k", "sideways", "U1", time.Unix(30000, 0)); err == nil {
		t.Fatal("rating 'sideways' must be rejected")
	}
	d, _ := New("") // disabled
	if err := d.Feedback("k", "up", "U1", time.Unix(30000, 0)); err != nil {
		t.Fatalf("disabled ledger Feedback must no-op, got %v", err)
	}
}

// TestFeedbackCreditsRecalledEntryAndSurvivesReplay: a 👍/👎 credits the entry of
// the newest open for the TriggerKey, and the fold is rebuilt identically from a
// fresh replay of the file (restart / leader failover).
func TestFeedbackCreditsRecalledEntryAndSurvivesReplay(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(30000, 0)
	if err := l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", TriggerKey: "k", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Feedback("k", "down", "U1", t0.Add(time.Minute)); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if err := l.Feedback("k", "up", "U2", t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	c, _ := l.OpenCounts()
	if a := c["e.md"]; a.FeedbackDown != 1 || a.FeedbackUp != 1 || a.Recalls != 1 {
		t.Fatalf("live fold e.md = %+v, want up=1 down=1 recalls=1", a)
	}
	l2, err := New(p)
	if err != nil {
		t.Fatalf("replay New: %v", err)
	}
	c2, _ := l2.OpenCounts()
	if a := c2["e.md"]; a.FeedbackDown != 1 || a.FeedbackUp != 1 || a.Recalls != 1 {
		t.Fatalf("replayed fold e.md = %+v, want up=1 down=1 recalls=1", a)
	}
}

// TestFeedbackDedupPerUserLatestWins: one live vote per (TriggerKey, user) — a
// duplicate click is idempotent, a changed vote MOVES (the previous rating is
// un-credited), and the invariant survives a fresh replay.
func TestFeedbackDedupPerUserLatestWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(31000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", TriggerKey: "k", At: t0})
	_ = l.Feedback("k", "down", "U1", t0.Add(1*time.Minute))
	_ = l.Feedback("k", "down", "U1", t0.Add(2*time.Minute)) // duplicate click
	if c, _ := l.OpenCounts(); c["e.md"].FeedbackDown != 1 {
		t.Fatalf("duplicate vote must be idempotent, got %+v", c["e.md"])
	}
	_ = l.Feedback("k", "up", "U1", t0.Add(3*time.Minute)) // changed mind
	if c, _ := l.OpenCounts(); c["e.md"].FeedbackUp != 1 || c["e.md"].FeedbackDown != 0 {
		t.Fatalf("changed vote must move, got %+v", c["e.md"])
	}
	l2, _ := New(p)
	if c, _ := l2.OpenCounts(); c["e.md"].FeedbackUp != 1 || c["e.md"].FeedbackDown != 0 {
		t.Fatalf("replayed dedup state = %+v, want up=1 down=0", c["e.md"])
	}
}

// TestFeedbackAttributionFollowsNewestOpen: the vote credits the entry of the
// NEWEST open for the TriggerKey at vote time — a fresh investigation (no entry)
// credits nothing, and a later re-vote against a newer recall open moves the
// credit to the new entry.
func TestFeedbackAttributionFollowsNewestOpen(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(32000, 0)
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "e1.md", TriggerKey: "k", At: t0})
	_ = l.Open(Event{Fingerprint: "fp2", Kind: "fresh", TriggerKey: "k", At: t0.Add(time.Hour)})
	_ = l.Feedback("k", "down", "U1", t0.Add(time.Hour+time.Minute))
	c, _ := l.OpenCounts()
	if a := c["e1.md"]; a.FeedbackDown != 0 {
		t.Fatalf("vote on a fresh investigation must not credit an older entry, got %+v", a)
	}
	_ = l.Open(Event{Fingerprint: "fp3", Kind: "recall", Entry: "e2.md", TriggerKey: "k", At: t0.Add(2 * time.Hour)})
	_ = l.Feedback("k", "down", "U1", t0.Add(2*time.Hour+time.Minute))
	c, _ = l.OpenCounts()
	if c["e2.md"].FeedbackDown != 1 || c["e1.md"].FeedbackDown != 0 {
		t.Fatalf("re-vote must credit the newest open's entry only: e1=%+v e2=%+v", c["e1.md"], c["e2.md"])
	}
}

// TestFeedbackOnNonResolvableRecallCounts pins the reason feedback exists: a
// non-resolvable recall (GitOps — no resolve signal can ever arrive) is excluded
// from resolve-based decay, so human feedback is its ONLY ground-truth channel
// and must be folded regardless of resolvability.
func TestFeedbackOnNonResolvableRecallCounts(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(33000, 0)
	rf := false
	_ = l.Open(Event{Fingerprint: "gitops:abc", Kind: "recall", Entry: "e.md", TriggerKey: "k", Resolvable: &rf, At: t0})
	_ = l.Feedback("k", "down", "U1", t0.Add(time.Minute))
	c, _ := l.OpenCounts()
	if a := c["e.md"]; a.Recalls != 0 || a.FeedbackDown != 1 {
		t.Fatalf("non-resolvable recall: want recalls=0 (excluded) down=1 (folded), got %+v", a)
	}
}

// TestFeedbackDoesNotDisturbEpisodesOrPairing: feedback lines are invisible to
// open/resolve pairing — Episodes() and the resolve credit are unchanged.
func TestFeedbackDoesNotDisturbEpisodesOrPairing(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(34000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", TriggerKey: "k", At: t0})
	_ = l.Feedback("k", "down", "U1", t0.Add(time.Minute))
	if _, ok, err := l.Resolve("fp", t0.Add(2*time.Minute)); err != nil || !ok {
		t.Fatalf("Resolve after feedback: ok=%v err=%v", ok, err)
	}
	eps, err := l.Episodes()
	if err != nil || len(eps) != 1 || !eps[0].Resolved {
		t.Fatalf("Episodes = %+v (err=%v), want exactly one resolved episode", eps, err)
	}
	if c, _ := l.OpenCounts(); c["e.md"].Resolved != 1 || c["e.md"].FeedbackDown != 1 {
		t.Fatalf("aggregate = %+v, want resolved=1 down=1", c["e.md"])
	}
}

// TestFeedbackSurvivesCompaction: votes, attribution and counters absorbed into a
// checkpoint are reconstructed exactly — including the per-user dedup (a repeated
// vote after compaction stays idempotent; a changed one still moves).
func TestFeedbackSurvivesCompaction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p) // no compaction while writing
	t0 := time.Unix(35000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", TriggerKey: "k", At: t0})
	_ = l.Feedback("k", "down", "U1", t0.Add(time.Minute))
	// Pad with unrelated resolved pairs so the feedback lines fall inside the
	// absorbed prefix, not the retained tail.
	for i := 0; i < 20; i++ {
		fp := fmt.Sprintf("pad%d", i)
		_ = l.Open(Event{Fingerprint: fp, Kind: "fresh", At: t0.Add(time.Duration(i+10) * time.Minute)})
		_, _, _ = l.Resolve(fp, t0.Add(time.Duration(i+11)*time.Minute))
	}
	c, _ := NewWithMaxEvents(p, 5) // reload triggers compaction (42 events > 5)
	if a, _ := c.OpenCounts(); a["e.md"].FeedbackDown != 1 {
		t.Fatalf("post-compaction fold = %+v, want down=1", a["e.md"])
	}
	// The checkpointed vote still dedups…
	_ = c.Feedback("k", "down", "U1", t0.Add(24*time.Hour))
	if a, _ := c.OpenCounts(); a["e.md"].FeedbackDown != 1 {
		t.Fatalf("checkpointed vote must stay idempotent, got %+v", a["e.md"])
	}
	// …and still moves on a changed rating.
	_ = c.Feedback("k", "up", "U1", t0.Add(25*time.Hour))
	if a, _ := c.OpenCounts(); a["e.md"].FeedbackUp != 1 || a["e.md"].FeedbackDown != 0 {
		t.Fatalf("checkpointed vote must move, got %+v", a["e.md"])
	}
}
