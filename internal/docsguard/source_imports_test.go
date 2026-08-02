// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/source"
)

// TestEverySourcePackageIsImportedHere guards the guard's own inputs.
//
// TestIntegrationPagesMatchRegistries reflects over source.Registered(), which is
// populated by each source package's init() — and init() only runs for packages
// this test binary actually imports. Those imports are the hand-written blank
// import block at the top of integration_pages_test.go.
//
// So a new source added without its blank import is INVISIBLE to the page guard.
// One direction of that still fails loudly (a page claiming an unregistered id is
// rejected), but the other passes in silence: the source ships with no docs page
// at all and nothing complains, which is precisely the gap the page guard exists
// to close. That is not hypothetical — internal/source/grafana was added on a
// branch that predated this package and arrived with no import here.
//
// Counting the packages on disk is the cross-check the import list cannot perform
// on itself. It deliberately reads the directory rather than a second list: a
// list would need the same maintenance as the imports, and would drift with them.
func TestEverySourcePackageIsImportedHere(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "source")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	var onDisk []string
	for _, e := range entries {
		if !e.IsDir() {
			continue // registry.go, webhook.go et al — the framework, not a source
		}
		onDisk = append(onDisk, e.Name())
	}
	if len(onDisk) == 0 {
		t.Fatalf("no source packages found under %s — the layout changed and this guard is inert", dir)
	}

	registered := map[string]bool{}
	for _, d := range source.Registered() {
		registered[d.Name] = true
	}

	for _, pkg := range onDisk {
		if !registered[pkg] {
			t.Errorf("internal/source/%s exists but is not registered in this test binary — add\n"+
				"\t_ \"github.com/Smana/runlore/internal/source/%s\"\n"+
				"to the blank-import block in integration_pages_test.go, or the page guard silently "+
				"skips this source and it can ship with no docs page", pkg, pkg)
		}
	}
	t.Logf("%d source packages on disk, all registered", len(onDisk))
}
