// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/gob"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// vecCacheFile is the on-disk shape of the persisted embedding cache. gob over
// JSON: map[string][]float32 in one stdlib call, ~1/10th the bytes, and no
// human ever reads this file. The header makes staleness detectable — a cache
// written by a different embedding model (or dimensionality) must never be
// served, so any mismatch discards the whole file and the corpus re-embeds.
type vecCacheFile struct {
	Version int
	Model   string
	Dim     int
	Vectors map[string][]float32
}

const vecCacheVersion = 1

// loadVecCache reads a persisted cache, returning nil (cold start) on ANY
// problem: absent, unreadable, corrupt, version/model/dimension mismatch.
// Fail-safe by contract — the cache is an optimization, never a correctness
// input. log is nil-safe.
func loadVecCache(path, model string, log *slog.Logger) map[string][]float32 {
	f, err := os.Open(path) //nolint:gosec // G304: operator-configured cache path
	if err != nil {
		return nil // absent is the common cold-start case; not worth a warn
	}
	defer func() { _ = f.Close() }()
	var vf vecCacheFile
	if err := gob.NewDecoder(f).Decode(&vf); err != nil {
		if log != nil {
			log.Warn("vector cache unreadable; re-embedding from scratch", "path", path, "err", err)
		}
		return nil
	}
	if vf.Version != vecCacheVersion || vf.Model != model || vf.Dim <= 0 {
		if log != nil {
			log.Warn("vector cache stale (version/model changed); re-embedding from scratch",
				"path", path, "cache_model", vf.Model, "model", model)
		}
		return nil
	}
	for _, v := range vf.Vectors {
		if len(v) != vf.Dim {
			if log != nil {
				log.Warn("vector cache dimension-incoherent; re-embedding from scratch", "path", path)
			}
			return nil
		}
	}
	return vf.Vectors
}

// saveVecCache writes the cache atomically (temp + rename, the ledger's
// pattern) so an interrupted write can never leave a torn file for the next
// startup to trip over. Empty caches are not persisted.
func saveVecCache(path, model string, cache map[string][]float32) error {
	if len(cache) == 0 {
		return nil
	}
	dim := 0
	for _, v := range cache {
		dim = len(v)
		break
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".veccache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := gob.NewEncoder(tmp).Encode(vecCacheFile{
		Version: vecCacheVersion, Model: model, Dim: dim, Vectors: cache,
	}); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename cache into place: %w", err)
	}
	return nil
}
