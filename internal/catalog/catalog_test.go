// SPDX-License-Identifier: Apache-2.0

package catalog

import "testing"

func TestCatalogSearch(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "helmrelease.md", `---
type: Playbook
title: HelmRelease upgrade failure
description: Helm chart bump leaves the release Ready=False.
tags: [flux, helmrelease, upgrade]
---
A chart bump that adds a DB migration can stall the release.
`)
	writeEntry(t, dir, "network.md", `---
type: Playbook
title: CiliumNetworkPolicy drops
description: Connectivity timeouts caused by a default-deny policy.
tags: [cilium, network, dns]
---
Check Hubble for DROPPED verdicts.
`)

	c, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("want 2 indexed, got %d", c.Len())
	}
	hits, err := c.Search("helmrelease chart migration", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Title != "HelmRelease upgrade failure" {
		t.Fatalf("expected the HelmRelease playbook to rank first, got %v", titles(hits))
	}
}

func titles(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Title
	}
	return out
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "a.md", "---\ntitle: Alpha\n---\nx")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != 1 {
		t.Fatalf("len = %d, want 1", c.Len())
	}
	// A new entry appears (e.g. a merged curation PR pulled by git-sync) and we reload.
	writeEntry(t, dir, "b.md", "---\ntitle: Beta\n---\ny")
	if _, err := c.Reload(dir); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("after reload len = %d, want 2", c.Len())
	}
	if hits, _ := c.Search("Beta", 3); len(hits) == 0 {
		t.Fatal("reloaded entry not searchable")
	}
}

func TestSearchScored(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "a.md", "---\ntitle: HelmRelease upgrade failure\ntags: [flux, helmrelease]\n---\nchart bump stalls the release")
	writeEntry(t, dir, "b.md", "---\ntitle: Network policy drops\ntags: [cilium]\n---\nconnectivity timeouts")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := c.SearchScored("helmrelease chart upgrade", 2)
	if err != nil {
		t.Fatalf("SearchScored: %v", err)
	}
	if len(hits) == 0 || hits[0].Entry.Title != "HelmRelease upgrade failure" {
		t.Fatalf("unexpected top hit: %+v", hits)
	}
	if hits[0].Score <= 0 {
		t.Fatalf("expected a positive relevance score, got %v", hits[0].Score)
	}
}

func TestSearchLiftsResourceTerm(t *testing.T) {
	// The Resource field drives the recall structural filter; it must also feed
	// BM25 so a query naming the resource gets lexical lift. The distinguishing
	// term ("harbor-core") lives ONLY in Resource here — not in title/desc/tags/
	// body — so the entry can only rank first if Resource is indexed.
	entries := []Entry{
		{Title: "Pod restart loop", Description: "container keeps restarting", Resource: "tooling/harbor-core", Body: "the pod will not become ready"},
		{Title: "Network policy drops", Description: "connectivity timeouts", Resource: "apps/web", Body: "default-deny blocks traffic"},
	}
	idx, err := buildIndex(entries)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	defer func() { _ = idx.Close() }()
	c := &Catalog{index: idx, entries: entries}
	hits, err := c.SearchScored("harbor-core", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Entry.Resource != "tooling/harbor-core" {
		t.Fatalf("a resource-term query must rank the matching-resource entry first, got %+v", titles2(hits))
	}
}

func titles2(es []ScoredEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Entry.Title
	}
	return out
}

func TestNewIndexMappingUsesBM25(t *testing.T) {
	// bleve defaults to legacy TF-IDF when ScoringModel is unset. Both index
	// sites are forced through this helper, so asserting it guarantees BM25
	// everywhere. This is the regression guard for the silent-fallback bug.
	if got := newIndexMapping().ScoringModel; got != "bm25" {
		t.Fatalf("ScoringModel = %q, want \"bm25\"", got)
	}
}

func TestBuildIndexScores(t *testing.T) {
	// Proves the BM25 mapping is accepted (NewMemOnly errors on an unsupported
	// scoring model) and that scoring + ranking work end-to-end. We do NOT assert
	// magnitudes — TF-IDF also length-normalizes, so magnitude-based BM25-vs-TFIDF
	// discrimination is brittle; the helper assertion above is the reliable guard.
	entries := []Entry{
		{Title: "OOMKilled pod", Body: "container exceeded its memory limit"},
		{Title: "Image pull failure", Body: "registry returned forbidden"},
	}
	idx, err := buildIndex(entries)
	if err != nil {
		t.Fatalf("buildIndex: %v", err)
	}
	defer func() { _ = idx.Close() }()
	c := &Catalog{index: idx, entries: entries}
	hits, err := c.SearchScored("memory limit", 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 || hits[0].Score <= 0 {
		t.Fatalf("expected a positive-scored hit, got %+v", hits)
	}
	if hits[0].Entry.Title != "OOMKilled pod" {
		t.Fatalf("expected the OOM entry ranked first, got %q", hits[0].Entry.Title)
	}
}

func TestEmptyCatalog(t *testing.T) {
	c := NewEmpty()
	if c.Len() != 0 {
		t.Fatalf("empty len = %d", c.Len())
	}
	hits, err := c.Search("anything", 3)
	if err != nil {
		t.Fatalf("search empty: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("empty hits = %d", len(hits))
	}
}

func TestNewEmptyNotReady(t *testing.T) {
	if NewEmpty().Ready() {
		t.Fatal("a freshly-created empty catalog must not report ready before first sync")
	}
}

func TestReloadMarksReady(t *testing.T) {
	dir := t.TempDir() // zero entries on purpose: a synced-but-empty KB is still ready
	c := NewEmpty()
	if _, err := c.Reload(dir); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !c.Ready() {
		t.Fatal("after a successful Reload the catalog must report ready (even with 0 entries)")
	}
}

func TestStaticCatalogReadyOnLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.Ready() {
		t.Fatal("a static-dir catalog must be ready immediately after New")
	}
}

// TestFindFingerprint: curated entries persist their DupFingerprint in
// frontmatter; the catalog must answer exact-identity lookups on it so the
// curator can dedup deterministically instead of relying on a BM25 threshold.
func TestFindFingerprint(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "curated.md", `---
type: Incident
title: Harbor down
description: valkey down
resource: tooling/harbor-core
fingerprint: deadbeefcafebabe
---
body
`)
	writeEntry(t, dir, "hand-written.md", `---
type: Playbook
title: No fingerprint here
description: hand-authored
---
body
`)
	c, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e, ok := c.FindFingerprint("deadbeefcafebabe")
	if !ok || e.Title != "Harbor down" {
		t.Fatalf("want the curated entry, got ok=%v e=%+v", ok, e)
	}
	if _, ok := c.FindFingerprint("0000000000000000"); ok {
		t.Fatal("unknown fingerprint must not match")
	}
	// An empty fingerprint must never match anything — hand-written entries have
	// no fingerprint field, and an empty query is "nothing to key on".
	if _, ok := c.FindFingerprint(""); ok {
		t.Fatal("empty fingerprint must never match")
	}
}
