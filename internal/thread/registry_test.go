// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"errors"
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
