// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeTitledEntry writes a minimal valid OKF entry whose body embeds the title,
// so a search for the title's terms ranks it. (The package's existing writeEntry
// helper writes verbatim content; this one wraps a title in frontmatter.)
func writeTitledEntry(t *testing.T, dir, name, title string) {
	t.Helper()
	entry := "---\ntype: Incident\ntitle: " + title + "\n---\nBody about " + title + ".\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
}

// searchTitles returns the top-k result titles for q — the comparable view used
// by the parity property below.
func searchTitles(t *testing.T, c *Catalog, q string) []string {
	t.Helper()
	hits, err := c.SearchScored(q, 10)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Entry.Title
	}
	return out
}

// TestReloadDeltaMatchesFullRebuild pins the core property: an incremental
// reload must be indistinguishable from a from-scratch load of the same dir.
func TestReloadDeltaMatchesFullRebuild(t *testing.T) {
	dir := t.TempDir()
	writeTitledEntry(t, dir, "a.md", "cilium agent crashloop")
	writeTitledEntry(t, dir, "b.md", "postgres disk pressure")
	writeTitledEntry(t, dir, "c.md", "dns resolution flaking")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate: modify a, delete b, add d.
	writeTitledEntry(t, dir, "a.md", "cilium agent oomkilled")
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	writeTitledEntry(t, dir, "d.md", "ingress certificate expired")

	skipped, err := c.ReloadDelta(context.Background(), dir,
		&SyncDelta{Changed: []string{"a.md", "d.md"}, Removed: []string{"b.md"}})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("delta reload: skipped=%v err=%v", skipped, err)
	}

	fresh, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != fresh.Len() {
		t.Fatalf("Len: delta=%d fresh=%d", c.Len(), fresh.Len())
	}
	for _, q := range []string{"cilium oomkilled", "postgres disk", "certificate expired", "dns flaking"} {
		if got, want := searchTitles(t, c, q), searchTitles(t, fresh, q); !slices.Equal(got, want) {
			t.Errorf("query %q: delta=%v fresh=%v", q, got, want)
		}
	}
}

// TestReloadDeltaChangedEntryNowUnparseable: a changed file that fails to parse
// is skipped by Load — its stale doc must not linger in the index.
func TestReloadDeltaChangedEntryNowUnparseable(t *testing.T) {
	dir := t.TempDir()
	writeTitledEntry(t, dir, "a.md", "cilium agent crashloop")
	writeTitledEntry(t, dir, "b.md", "postgres disk pressure")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("---\ntitle: [broken\n---\nx"), 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := c.ReloadDelta(context.Background(), dir, &SyncDelta{Changed: []string{"a.md"}})
	if err != nil || len(skipped) != 1 {
		t.Fatalf("skipped=%v err=%v, want exactly the broken entry skipped", skipped, err)
	}
	for _, title := range searchTitles(t, c, "cilium crashloop") {
		if title == "cilium agent crashloop" {
			t.Error("stale doc for now-unparseable entry still indexed")
		}
	}
}

// TestReloadDeltaNilFallsBackToFull: nil delta must behave exactly like
// ReloadContext (the first-sync / diff-error path).
func TestReloadDeltaNilFallsBackToFull(t *testing.T) {
	dir := t.TempDir()
	writeTitledEntry(t, dir, "a.md", "cilium agent crashloop")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeTitledEntry(t, dir, "b.md", "postgres disk pressure")
	if _, err := c.ReloadDelta(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Errorf("Len=%d, want 2 after nil-delta full reload", c.Len())
	}
}
