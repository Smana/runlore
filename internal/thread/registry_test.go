// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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
	if tc != (Context{}) {
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
// non-atomic Get-then-Put sequences racing, rather than one atomic operation.
// Both callers could walk away believing they had established THE entry for
// the thread, each with its own Notes: 0 / NoteURL: "", which is what let
// Mention.HandleMention route both to OpenPR and produce two standalone PRs
// for one thread.
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
