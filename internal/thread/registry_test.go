// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	r.Register("slack", "111.222", "C1", inv)

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

// TestRegistryUpdateOnUnknownRootIsDistinguishableFromSuccess pins the fix for
// the defect where an Update on a root the registry does not track returned
// nil exactly as a successful update would — a caller (Responder, writing back
// the per-thread note counter) could not tell "counted" from "silently
// dropped", which is what let the per-thread cap go permanently inert for any
// thread the registry had lost track of.
func TestRegistryUpdateOnUnknownRootIsDistinguishableFromSuccess(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Update("nope", func(c *Context) { c.Notes++ }); !errors.Is(err, ErrThreadNotTracked) {
		t.Fatalf("Update(unknown root) = %v, want ErrThreadNotTracked", err)
	}
}

// TestRegistryUpdateOnDisabledRegistryIsStillANoOp pins that a disabled
// registry (no ledger path) keeps its existing silent-no-op contract: that
// case is refused upstream by config validation whenever thread capture is
// on, so it must not start returning ErrThreadNotTracked and be confused for
// a tracking miss.
func TestRegistryUpdateOnDisabledRegistryIsStillANoOp(t *testing.T) {
	r, err := NewRegistry("", time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Update("anything", func(c *Context) { c.Notes++ }); err != nil {
		t.Fatalf("Update on a disabled registry must remain a no-op, got %v", err)
	}
}

// TestRegistryGetOrCreateHitWinsOverFallback pins that a registry hit is
// always returned over a caller-supplied fallback — a hit carries NoteURL /
// Notes state a fresh fallback stamp cannot reconstruct.
func TestRegistryGetOrCreateHitWinsOverFallback(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := r.Put(Context{Root: "r1", Notes: 3, CuratedURL: "https://github.com/o/r/pull/1"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	tc, created, err := r.GetOrCreate("r1", Context{Root: "r1", CuratedURL: "https://github.com/o/r/pull/999"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if created {
		t.Fatal("a registry hit must not be reported as created")
	}
	if tc.Notes != 3 || tc.CuratedURL != "https://github.com/o/r/pull/1" {
		t.Fatalf("GetOrCreate must return the tracked entry, not the fallback: %+v", tc)
	}
}

// TestRegistryGetOrCreatePersistsFallbackOnMiss pins the miss half of the
// contract: a genuine miss creates and durably persists the fallback,
// reporting created = true.
func TestRegistryGetOrCreatePersistsFallbackOnMiss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.jsonl")
	r, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tc, created, err := r.GetOrCreate("r1", Context{CuratedURL: "https://github.com/o/r/pull/42"})
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if !created {
		t.Fatal("a genuine miss must report created = true")
	}
	if tc.Root != "r1" {
		t.Fatalf("Root = %q, want it stamped to the requested root", tc.Root)
	}

	// Durable: a fresh registry opened from the same path must replay it.
	r2, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	stored, ok := r2.Get("r1")
	if !ok {
		t.Fatal("GetOrCreate's write was not durably persisted")
	}
	if stored.CuratedURL != "https://github.com/o/r/pull/42" {
		t.Fatalf("CuratedURL = %q, want it preserved", stored.CuratedURL)
	}
}

// TestRegistryGetOrCreateOnDisabledRegistryReturnsSentinel pins that a
// disabled registry (no ledger path) cannot silently report the same outcome
// as "a concurrent caller already established this entry" — created=false,
// err=nil is exactly that outcome, and a disabled registry establishes
// nothing, so it must return a distinguishable, non-nil error instead.
func TestRegistryGetOrCreateOnDisabledRegistryReturnsSentinel(t *testing.T) {
	r, err := NewRegistry("", time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	tc, created, gerr := r.GetOrCreate("r1", Context{Root: "r1", CuratedURL: "https://github.com/o/r/pull/1"})
	if !errors.Is(gerr, ErrThreadNotEstablishable) {
		t.Fatalf("GetOrCreate on a disabled registry = %v, want ErrThreadNotEstablishable", gerr)
	}
	if created {
		t.Fatal("a registry that established nothing must not report created = true")
	}
	if !reflect.DeepEqual(tc, Context{}) {
		t.Fatalf("tc = %+v, want the zero value alongside this error", tc)
	}
}

// TestRegistryGetOrCreateOnEmptyRootReturnsSentinel mirrors the
// disabled-registry case for an empty root: neither may be reported as the
// "concurrent winner" outcome (created=false, err=nil).
func TestRegistryGetOrCreateOnEmptyRootReturnsSentinel(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, created, gerr := r.GetOrCreate("", Context{CuratedURL: "https://github.com/o/r/pull/1"})
	if !errors.Is(gerr, ErrThreadNotEstablishable) {
		t.Fatalf("GetOrCreate(root=\"\") = %v, want ErrThreadNotEstablishable", gerr)
	}
	if created {
		t.Fatal("an empty root must not report created = true")
	}
}

// TestRegistryGetOrCreateConcurrentFirstCallersEstablishExactlyOneEntry pins
// the fix for the concurrency hole in the rehydrate path: two never-before-
// tracked messages arriving close together used to each independently decide
// "the registry misses, so I persist my own copy of the fallback" — two
// non-atomic Get-then-Put sequences racing, rather than one atomic operation,
// where a late Put could CLOBBER an earlier caller's entry (discarding
// NoteURL/Notes state that caller had already written back). This test pins
// that GetOrCreate now closes THAT: every concurrent caller shares the one
// entry the first one's goroutine persists, rather than each risking
// creating (and clobbering with) its own.
//
// It does not, by itself, prove no duplicate PR is ever opened — two callers
// sharing this one entry can still both read its NoteURL == "" before either
// one's forge write updates it. That outcome is closed separately, by
// Responder.write's per-root guard — see
// TestResponderConcurrentFirstNotesOnSameRootProduceExactlyOneOpenPR.
//
// Driven with real goroutines under -race, not a simulated ordering: the
// hazard is a genuine data race between two lock acquisitions (Get, then
// Put), not a logic bug a single goroutine could exhibit.
func TestRegistryGetOrCreateConcurrentFirstCallersEstablishExactlyOneEntry(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 100)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	created := make([]bool, n)
	got := make([]Context, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fallback := Context{Root: "unknown", CuratedURL: fmt.Sprintf("https://github.com/o/r/pull/%d", i+1)}
			tc, wasCreated, gerr := r.GetOrCreate("unknown", fallback)
			created[i], got[i], errs[i] = wasCreated, tc, gerr
		}(i)
	}
	wg.Wait()

	for i, gerr := range errs {
		if gerr != nil {
			t.Fatalf("GetOrCreate[%d]: %v", i, gerr)
		}
	}

	winners := 0
	for _, c := range created {
		if c {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 — exactly one concurrent caller must establish the entry", winners)
	}

	established := got[0].CuratedURL
	for i, tc := range got {
		if tc.CuratedURL != established {
			t.Fatalf("caller %d observed CuratedURL %q, want the single established value %q — every caller must see the SAME entry, not its own fallback",
				i, tc.CuratedURL, established)
		}
	}

	stored, ok := r.Get("unknown")
	if !ok {
		t.Fatal("registry lost the entry after concurrent GetOrCreate calls")
	}
	if stored.CuratedURL != established {
		t.Fatalf("stored.CuratedURL = %q, want %q", stored.CuratedURL, established)
	}
}

// TestRegistryLockRootReleasesOnPanic pins that the per-root write guard is
// released even when the code holding it panics: the release must be
// deferred right after acquisition, or one panicking write would leave every
// later write on that root blocked forever — the guard's own worst-case
// leak, distinct from the "unbounded map growth" leak the guard's doc
// comment addresses separately.
func TestRegistryLockRootReleasesOnPanic(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	func() {
		defer func() { _ = recover() }()
		release := r.lockRoot("r1")
		defer release()
		panic("simulated failure mid-write")
	}()

	done := make(chan struct{})
	go func() {
		release := r.lockRoot("r1")
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lockRoot(\"r1\") deadlocked after a panicking holder — the guard was not released")
	}
}

// TestRegistryLockRootMapDoesNotLeakEntries pins the bound on the guard map:
// once every holder of a root's lock has released it, no trace of that root
// remains in the bookkeeping map — it is bounded by concurrency (how many
// writes are in flight right now), not by history (how many roots were ever
// written to).
func TestRegistryLockRootMapDoesNotLeakEntries(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			release := r.lockRoot(fmt.Sprintf("root-%d", i%3))
			release()
		}(i)
	}
	wg.Wait()

	r.mu.Lock()
	remaining := len(r.writeLocks)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("writeLocks has %d entries left after every holder released — the map is leaking", remaining)
	}
}

// TestConcurrencyDocsDoNotOverclaimGetOrCreateAloneClosesTheDuplicatePR pins
// that four places explaining the concurrent-first-message fix no longer
// carry the overclaim an earlier version made: that folding GetOrCreate's
// check and create into one atomic operation, by itself, closes the
// duplicate-PR outcome. It does not — it closes the CLOBBER (a losing
// caller's Put overwriting a winner's already-updated entry); the
// duplicate-PR outcome is closed separately, by Responder.write's per-root
// guard (commit "serialise writes per thread root"). Each marker below is a
// distinctive substring from the text this test replaces, chosen so the test
// fails (proving the overclaim is still present) until the doc comment is
// actually corrected, and stays failing if it ever regresses.
func TestConcurrencyDocsDoNotOverclaimGetOrCreateAloneClosesTheDuplicatePR(t *testing.T) {
	cases := []struct {
		file    string
		removed string
	}{
		{"registry.go", "Closing this does not close every race downstream of it"},
		{"mention.go", "for one thread. See Registry.GetOrCreate's doc comment for the residual"},
		// Split across a concatenation so the marker itself, read back out of
		// THIS file's own source below, is not a false self-match: the two
		// literals are adjacent only once joined at runtime, never as a
		// contiguous run of source bytes.
		{"registry_test.go", "Mention.HandleMention route both to OpenPR" + " and produce two standalone PRs"},
		{"../../dev/superpowers/specs/2026-08-14-thread-interaction-design.md",
			"Only the `NoteURL` write-back is lost, so a second note may open a second PR."},
	}
	for _, tt := range cases {
		src, err := os.ReadFile(tt.file) //nolint:gosec // test-owned relative path
		if err != nil {
			t.Fatalf("read %s: %v", tt.file, err)
		}
		if strings.Contains(string(src), tt.removed) {
			t.Errorf("%s still carries the superseded text %q — GetOrCreate's atomicity alone does not "+
				"close the duplicate-PR outcome (it closes the clobber); say what each mechanism actually does",
				tt.file, tt.removed)
		}
	}
}

func TestRegistryRegisterIgnoresEmptyRoot(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.Register("slack", "", "C1", providers.Investigation{Title: "x"})
	if _, ok := r.Get(""); ok {
		t.Fatal("an empty root must never be stored")
	}
}

// TestRegistryRegisterUsesCallerTransport proves Register stamps the
// transport its CALLER passed, not a hardcoded one — Register used to always
// write Transport: "slack" regardless of which notifier called it, which
// mislabels every Matrix-originated note as coming from Slack in the
// generated KB PR (thread.NoteBody / thread.ConceptEntry render tc.Transport
// verbatim). A registry fed by a Matrix delivery must record "matrix".
func TestRegistryRegisterUsesCallerTransport(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.Register("matrix", "$evt123", "!room:example.org", providers.Investigation{Title: "OOMKilled"})

	got, ok := r.Get("$evt123")
	if !ok {
		t.Fatal("Register did not store the thread")
	}
	if got.Transport != "matrix" {
		t.Fatalf("Transport = %q, want matrix — Register must not hardcode the caller's transport", got.Transport)
	}
}

// evidenceItems flattens one context's evidence into a single slice, for the
// bound assertions below.
func evidenceItems(ev Evidence) []string {
	var all []string
	all = append(all, ev.RootCauses...)
	all = append(all, ev.RuledOut...)
	all = append(all, ev.Unresolved...)
	all = append(all, ev.DataGaps...)
	return all
}

// TestRegistryRegisterCarriesInvestigationEvidence pins that Register keeps
// what the investigation FOUND — its root causes, the hypotheses it ruled
// out, what it left unresolved, the signals it could not obtain — alongside
// the identity fields it already kept.
//
// The design spec makes this load-bearing: `@runlore reinvestigate:` is a
// declared NON-GOAL precisely because the chat layer "already holds the
// investigation's ruled-out hypotheses, open questions and data gaps" and can
// answer "did you check the NetworkPolicies?" from them without re-running an
// investigation. Register is the only point in the process where RuledOut and
// DataGaps exist at all — the curator's KB draft never writes them and the
// outcome ledger has no such fields — so anything dropped here is gone.
//
// The root causes are fed in with their confidences ASCENDING, against the
// order a "ranked" list would be in. evidenceFrom does not sort — it takes a
// prefix of the investigation's own order, the same order every other reader
// of RootCauses treats as authoritative (notify/slack.go's top cause,
// curator/draft.go's numbered "## Cause" list, curator/fingerprint.go's
// cause). Storing a differently-ordered list here would tell the chat model
// something other than what the on-call was shown. A fixture already sorted
// by confidence could not tell the two apart.
func TestRegistryRegisterCarriesInvestigationEvidence(t *testing.T) {
	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	inv := providers.Investigation{
		Title: "ImageGalleryUnavailable",
		RootCauses: []providers.Hypothesis{
			{Summary: "Registry TLS certificate expired", Confidence: 0.2},
			{Summary: "NetworkPolicy denies egress from apps to the registry", Confidence: 0.8},
		},
		RuledOut:   []string{"Node memory pressure — kubelet reported no evictions"},
		Unresolved: []string{"Why the policy changed at 02:00 UTC"},
		DataGaps:   []string{"Hubble flows unavailable for the incident window"},
	}
	r.Register("slack", "111.222", "C1", inv)

	got, ok := r.Get("111.222")
	if !ok {
		t.Fatal("Register did not store the thread")
	}
	ev := got.Evidence
	wantCauses := []string{"Registry TLS certificate expired", "NetworkPolicy denies egress from apps to the registry"}
	if !slices.Equal(ev.RootCauses, wantCauses) {
		t.Fatalf("RootCauses = %q, want %q — the investigation's own order, unsorted", ev.RootCauses, wantCauses)
	}
	if len(ev.RuledOut) != 1 || ev.RuledOut[0] != inv.RuledOut[0] {
		t.Fatalf("RuledOut = %q, want %q", ev.RuledOut, inv.RuledOut)
	}
	if len(ev.Unresolved) != 1 || ev.Unresolved[0] != inv.Unresolved[0] {
		t.Fatalf("Unresolved = %q, want %q", ev.Unresolved, inv.Unresolved)
	}
	if len(ev.DataGaps) != 1 || ev.DataGaps[0] != inv.DataGaps[0] {
		t.Fatalf("DataGaps = %q, want %q", ev.DataGaps, inv.DataGaps)
	}
}

// TestRegistryEvidenceIsBoundedAtCapture pins that the bound is a property of
// what is STORED, not of what some later renderer chooses to show. The
// registry holds up to DefaultRegistryMax contexts and appends every one of
// them to JSONL, so an unbounded evidence blob is an unbounded file — the cap
// therefore has to land on the way in, at Register.
//
// The items are built from 3-byte runes behind a 2-byte prefix on purpose, so
// that MaxEvidenceItemBytes lands mid-rune: a cap applied on a byte boundary
// would store invalid UTF-8, and a fixture that happens to align would not
// notice.
//
// Every bound below is a LITERAL, not the constant the capture path itself
// applies. Written as `len(item) > MaxEvidenceItemBytes+2` and
// `total > MaxEvidenceBytes` these assertions restated the production
// expression — MaxEvidenceBytes is literally
// (MaxEvidenceRootCauses + 3*MaxEvidenceListItems) * (MaxEvidenceItemBytes+2)
// — so both sides moved together and they held for any cap. Measured: raising
// MaxEvidenceItemBytes from 100 to 111 widened the whole ceiling by 11% and
// this test passed unchanged.
func TestRegistryEvidenceIsBoundedAtCapture(t *testing.T) {
	// The bound, stated as the numbers it comes to: 3 root causes plus three
	// lists of 5 is 18 items, each at most 102 bytes (the 100-byte per-item cap
	// plus truncate's overshoot — it cuts at n-1 and appends a 3-byte "…"), so
	// 1836 bytes of evidence text for one stored context. The registry keeps up
	// to DefaultRegistryMax of these on disk, so this is a sizing decision, not
	// an implementation detail free to drift.
	const (
		wantRootCauses = 3
		wantListItems  = 5
		maxItemBytes   = 102
		maxTotalBytes  = 1836
	)

	r, err := NewRegistry(filepath.Join(t.TempDir(), "threads.jsonl"), time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	long := strings.Repeat("→", 4*MaxEvidenceItemBytes) // far past the per-item cap, in 3-byte runes
	var causes []providers.Hypothesis
	var lines []string
	for i := range 50 { // far past every list cap
		causes = append(causes, providers.Hypothesis{Summary: fmt.Sprintf("%d %s", i, long)})
		lines = append(lines, fmt.Sprintf("%d %s", i, long))
	}
	r.Register("slack", "111.222", "C1", providers.Investigation{
		RootCauses: causes,
		RuledOut:   lines,
		Unresolved: lines,
		DataGaps:   lines,
	})

	got, ok := r.Get("111.222")
	if !ok {
		t.Fatal("Register did not store the thread")
	}
	ev := got.Evidence
	if len(ev.RootCauses) != wantRootCauses {
		t.Fatalf("RootCauses kept %d items, want %d", len(ev.RootCauses), wantRootCauses)
	}
	for name, list := range map[string][]string{"RuledOut": ev.RuledOut, "Unresolved": ev.Unresolved, "DataGaps": ev.DataGaps} {
		if len(list) != wantListItems {
			t.Fatalf("%s kept %d items, want %d", name, len(list), wantListItems)
		}
	}
	total := 0
	for _, item := range evidenceItems(ev) {
		if !utf8.ValidString(item) {
			t.Fatalf("stored item is not valid UTF-8 — the cap must land on a rune boundary: %q", item)
		}
		if len(item) > maxItemBytes {
			t.Fatalf("stored item is %d bytes, past the %d-byte per-item bound: %q", len(item), maxItemBytes, item)
		}
		total += len(item)
	}
	if total > maxTotalBytes {
		t.Fatalf("stored evidence is %d bytes, past the %d-byte per-context budget the registry was sized for "+
			"(× %d live contexts)", total, maxTotalBytes, DefaultRegistryMax)
	}
	// MaxEvidenceBytes is what the rest of the code — chat.go's writeEvidence,
	// the doc comments quoting 1836 bytes — reads as this ceiling, so it has to
	// be the same number the assertions above just held the stored bytes to.
	if MaxEvidenceBytes != maxTotalBytes {
		t.Errorf("MaxEvidenceBytes = %d, want %d: the constant the code applies has drifted from the budget this test measures against",
			MaxEvidenceBytes, maxTotalBytes)
	}
}

// TestRegistryEvidenceRoundTripsThroughTheLedger is the assertion that matters
// most here: a field that marshals but does not unmarshal looks correct in
// memory and is silently gone after the restart or leader failover the
// registry's JSONL replay exists to survive.
func TestRegistryEvidenceRoundTripsThroughTheLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.jsonl")
	r1, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	inv := providers.Investigation{
		Title:      "ImageGalleryUnavailable",
		RootCauses: []providers.Hypothesis{{Summary: "NetworkPolicy denies egress to the registry"}},
		RuledOut:   []string{"Node memory pressure — kubelet reported no evictions"},
		Unresolved: []string{"Why the policy changed at 02:00 UTC"},
		DataGaps:   []string{"Hubble flows unavailable for the incident window"},
	}
	r1.Register("slack", "111.222", "C1", inv)

	r2, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := r2.Get("111.222")
	if !ok {
		t.Fatal("replay lost the entry")
	}
	want := evidenceItems(Evidence{
		RootCauses: []string{inv.RootCauses[0].Summary},
		RuledOut:   inv.RuledOut,
		Unresolved: inv.Unresolved,
		DataGaps:   inv.DataGaps,
	})
	if diff := fmt.Sprint(evidenceItems(got.Evidence)); diff != fmt.Sprint(want) {
		t.Fatalf("evidence did not survive the ledger round-trip:\n got %s\nwant %s", diff, fmt.Sprint(want))
	}
}

// TestRegistryLoadsRecordsWrittenBeforeEvidence pins backward compatibility:
// the registry replays an append-only log that older binaries wrote, so a line
// with no evidence field at all must still decode — to an identity-only
// context, with no error and no dropped entry.
func TestRegistryLoadsRecordsWrittenBeforeEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.jsonl")
	legacy := fmt.Sprintf(
		`{"Transport":"slack","Root":"111.222","Channel":"C1","TriggerKey":"tk","Title":"OOMKilled","Verdict":"action_required","Notes":2,"At":%q}`+"\n",
		time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("seed legacy record: %v", err)
	}

	r, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry over a pre-evidence record: %v", err)
	}
	got, ok := r.Get("111.222")
	if !ok {
		t.Fatal("a record written before evidence existed must still replay")
	}
	if got.Title != "OOMKilled" || got.Notes != 2 {
		t.Fatalf("legacy record lost fields: %+v", got)
	}
	if items := evidenceItems(got.Evidence); len(items) != 0 {
		t.Fatalf("Evidence = %q, want empty for a pre-evidence record", items)
	}
}

// TestRegistryRegisterWithoutEvidenceStoresNone pins that an investigation
// with nothing to say produces an ABSENT evidence payload rather than a
// payload of empty strings — the registry appends a line per write, so a
// placeholder on every evidence-free record is pure file growth.
func TestRegistryRegisterWithoutEvidenceStoresNone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.jsonl")
	r, err := NewRegistry(path, time.Hour, 10)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	r.Register("slack", "111.222", "C1", providers.Investigation{
		Title:      "ImageGalleryUnavailable",
		RootCauses: []providers.Hypothesis{{Summary: "   "}}, // present but blank
		RuledOut:   []string{"", " "},
	})

	got, ok := r.Get("111.222")
	if !ok {
		t.Fatal("Register did not store the thread")
	}
	if items := evidenceItems(got.Evidence); len(items) != 0 {
		t.Fatalf("Evidence = %q, want empty — blank items must be dropped, not stored", items)
	}
	line, err := os.ReadFile(path) //nolint:gosec // G304: path is this test's own temp file
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if strings.Contains(string(line), "Evidence") {
		t.Fatalf("an evidence-free record must not persist an evidence field: %s", line)
	}
}
