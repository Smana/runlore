// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEmbedder maps text to a small vector by keyword presence — deterministic, so
// cosine reflects keyword overlap (enough to exercise fusion + ordering mechanics).
// The trailing 0.1 dim keeps a keyword-free text from being a zero vector.
type fakeEmbedder struct{ err error }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = []float32{kw(t, "harbor"), kw(t, "network"), kw(t, "cert"), 0.1}
	}
	return out, nil
}

func kw(t, k string) float32 {
	if strings.Contains(strings.ToLower(t), k) {
		return 1
	}
	return 0
}

func hybridDir(t *testing.T) string {
	dir := t.TempDir()
	writeEntry(t, dir, "harbor.md", "---\ntitle: Harbor registry down\n---\nharbor image pull fails")
	writeEntry(t, dir, "network.md", "---\ntitle: Network drops\n---\nnetwork connectivity timeouts")
	writeEntry(t, dir, "cert.md", "---\ntitle: Certificate expiry\n---\ncert renewal failed")
	return dir
}

func TestSearchHybridFusesAndOrdersByCosine(t *testing.T) {
	c := &Catalog{}
	c.SetEmbedder(fakeEmbedder{})
	if _, err := c.Reload(hybridDir(t)); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !c.HasVectors() {
		t.Fatal("vectors should be built when an embedder is configured")
	}
	hits, err := c.SearchHybrid(context.Background(), "harbor registry image", 5)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Entry.Title, "Harbor") {
		t.Fatalf("expected the Harbor entry first, got %+v", hits)
	}
	if hits[0].Score < 0.5 {
		t.Fatalf("top hit cosine should be high for a clear match, got %v", hits[0].Score)
	}
	for i := 1; i < len(hits); i++ { // Score (cosine) must be descending for the recall gate
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("hits not ordered by descending cosine: %+v", hits)
		}
	}
}

func TestSearchHybridFallsBackWithoutEmbedder(t *testing.T) {
	c, err := New(hybridDir(t)) // no embedder
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.HasVectors() {
		t.Fatal("no embedder → no vectors → HasVectors false")
	}
	hits, err := c.SearchHybrid(context.Background(), "network connectivity timeouts", 5)
	if err != nil {
		t.Fatalf("SearchHybrid (fallback): %v", err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Entry.Title, "Network") {
		t.Fatalf("BM25 fallback should surface the Network entry, got %+v", hits)
	}
}

// countingEmbedder records every Embed call so tests can assert exactly which
// texts were sent (the cache's whole point: unchanged entries are never re-sent).
type countingEmbedder struct {
	calls [][]string
	fail  bool
}

func (f *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls = append(f.calls, append([]string(nil), texts...))
	if f.fail {
		return nil, errors.New("embed boom")
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i])), 1}
	}
	return out, nil
}

func writeHybridEntry(t *testing.T, dir, name, title, body string) {
	t.Helper()
	md := "---\ntype: Incident\ntitle: " + title + "\ndescription: d\nresource: ns/app\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReloadEmbedsOnlyChangedEntries: reload #1 embeds the full corpus; editing
// ONE entry and reloading embeds exactly that one; an unchanged reload embeds
// nothing. Deleted entries are evicted from the cache.
func TestReloadEmbedsOnlyChangedEntries(t *testing.T) {
	dir := t.TempDir()
	writeHybridEntry(t, dir, "a.md", "Alpha incident", "alpha body")
	writeHybridEntry(t, dir, "b.md", "Beta incident", "beta body")

	emb := &countingEmbedder{}
	c := NewEmpty()
	c.SetEmbedder(emb)

	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb.calls) != 1 || len(emb.calls[0]) != 2 {
		t.Fatalf("first reload: calls=%v, want one call with 2 texts", emb.calls)
	}
	if !c.HasVectors() {
		t.Fatal("first reload: HasVectors=false, want true")
	}

	// Edit ONE entry → only its text is re-embedded.
	writeHybridEntry(t, dir, "b.md", "Beta incident", "beta body EDITED")
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb.calls) != 2 || len(emb.calls[1]) != 1 {
		t.Fatalf("edited reload: calls=%v, want second call with exactly 1 text", emb.calls)
	}
	if !strings.Contains(emb.calls[1][0], "EDITED") {
		t.Fatalf("edited reload embedded the wrong text: %q", emb.calls[1][0])
	}
	if !c.HasVectors() {
		t.Fatal("edited reload: HasVectors=false, want true")
	}

	// Unchanged reload → zero embed traffic.
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb.calls) != 2 {
		t.Fatalf("unchanged reload: calls=%d, want 2 (no new embed call)", len(emb.calls))
	}

	// Delete an entry → cache must not pin it forever: re-adding it later re-embeds.
	if err := os.Remove(filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	writeHybridEntry(t, dir, "a.md", "Alpha incident", "alpha body")
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	last := emb.calls[len(emb.calls)-1]
	if len(last) != 1 || !strings.Contains(last[0], "alpha") {
		t.Fatalf("re-added entry not re-embedded after eviction: %v", emb.calls)
	}
}

func TestReloadEmbedFailureDegradesToBM25(t *testing.T) {
	c := &Catalog{}
	c.SetEmbedder(fakeEmbedder{err: fmt.Errorf("embeddings endpoint down")})
	if _, err := c.Reload(hybridDir(t)); err != nil {
		t.Fatalf("Reload must succeed despite an embed failure (graceful degradation): %v", err)
	}
	if c.HasVectors() {
		t.Fatal("an embed failure must leave vectors nil → BM25-only")
	}
	hits, err := c.SearchHybrid(context.Background(), "cert renewal failed", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("SearchHybrid must still work via BM25 fallback: hits=%d err=%v", len(hits), err)
	}
}
