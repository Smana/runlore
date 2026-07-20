// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/gob"
	"os"
	"path/filepath"
	"testing"
)

func TestVecCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	in := map[string][]float32{"k1": {1, 0}, "k2": {0.5, 0.5}}
	if err := saveVecCache(path, "bge-m3", in); err != nil {
		t.Fatal(err)
	}
	out := loadVecCache(path, "bge-m3", nil)
	if len(out) != 2 || out["k1"][0] != 1 || out["k2"][1] != 0.5 {
		t.Fatalf("round trip = %v", out)
	}
	// No temp residue from the atomic write.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".veccache-*"))
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// A model swap must never serve stale vectors: the whole cache is discarded.
func TestVecCacheModelMismatchDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	if err := saveVecCache(path, "bge-m3", map[string][]float32{"k": {1}}); err != nil {
		t.Fatal(err)
	}
	if out := loadVecCache(path, "text-embedding-3-small", nil); out != nil {
		t.Fatalf("model mismatch returned %v, want nil (cold start)", out)
	}
}

func TestVecCacheCorruptFileColdStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	if err := os.WriteFile(path, []byte("not gob at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := loadVecCache(path, "bge-m3", nil); out != nil {
		t.Fatalf("corrupt file returned %v, want nil", out)
	}
	if out := loadVecCache(filepath.Join(t.TempDir(), "absent.gob"), "bge-m3", nil); out != nil {
		t.Fatalf("absent file returned %v, want nil", out)
	}
}

// Dimension coherence: a cache whose vectors disagree with the recorded Dim is
// corrupt — discard, don't serve.
func TestVecCacheDimMismatchDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(f).Encode(vecCacheFile{
		Version: vecCacheVersion, Model: "bge-m3", Dim: 3,
		Vectors: map[string][]float32{"k": {1, 0}}, // len 2 ≠ Dim 3
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out := loadVecCache(path, "bge-m3", nil); out != nil {
		t.Fatalf("dim mismatch returned %v, want nil", out)
	}
}

// End-to-end: a second catalog with the same cache file embeds NOTHING on its
// first reload — the restart/HA-failover win this feature exists for.
func TestVecCachePersistsAcrossCatalogs(t *testing.T) {
	dir := t.TempDir()
	writeTitledEntry(t, dir, "a.md", "cilium agent crashloop")
	writeTitledEntry(t, dir, "b.md", "postgres disk pressure")
	cachePath := filepath.Join(t.TempDir(), "vectors.gob")

	first := NewEmpty()
	emb1 := &countingEmbedder{}
	first.SetEmbedder(emb1)
	first.EnableVectorCache(cachePath, "fake-model")
	if _, err := first.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb1.calls) == 0 || !first.HasVectors() {
		t.Fatalf("first catalog: calls=%d hasVectors=%v", len(emb1.calls), first.HasVectors())
	}

	second := NewEmpty()
	emb2 := &countingEmbedder{}
	second.SetEmbedder(emb2)
	second.EnableVectorCache(cachePath, "fake-model")
	if _, err := second.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb2.calls) != 0 {
		t.Errorf("second catalog embedded %d batches, want 0 (cache warm from disk)", len(emb2.calls))
	}
	if !second.HasVectors() {
		t.Error("second catalog has no vectors despite warm cache")
	}
}
