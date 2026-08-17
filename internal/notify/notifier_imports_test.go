// SPDX-License-Identifier: Apache-2.0

package notify_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// notifierImportPrefix is the import path every notifier subpackage sits
// under; a blank import of one is what makes its init() — and therefore its
// Register call — run in a given binary.
const notifierImportPrefix = "github.com/Smana/runlore/internal/notify/"

// TestEveryNotifierSubpackageIsImportedHere guards the collision guard's own
// inputs.
//
// TestRegisteredNotifierNamesDoNotCollideWithConfigFields reflects over
// notify.Registered(), which each notifier subpackage populates from its
// init() — and init() only runs for packages this test binary actually
// imports. Those imports are the hand-written blank-import block at the top of
// config_collision_test.go. A notifier subpackage added without its blank
// import is therefore INVISIBLE to the collision guard: it can register under
// "slack", "matrix" or "thread", shadow that named config.Notify field, and
// every test still passes. That is the same failure class the collision guard
// itself was written to close, relocated one level up into the guard's inputs.
//
// The list it drifts from in practice is internal/app/serve.go's own
// blank-import block — the canonical set of drop-ins the shipped binary links
// in. This guard reads the DIRECTORY instead, because the directory is a
// superset of that list: a notifier subpackage that exists but was forgotten
// in serve.go as well would still be caught here, whereas a serve.go-vs-test
// comparison would wave it through. It also cannot itself drift, the way a
// second hand-maintained list would.
//
// Only packages that actually self-register are required — a future helper
// subpackage under internal/notify carries no init() side effect worth
// importing, so it is skipped rather than forced into an exemption list.
func TestEveryNotifierSubpackageIsImportedHere(t *testing.T) {
	// `go test` runs with the working directory set to the package directory,
	// so "." is internal/notify.
	selfRegistering := selfRegisteringPackages(t, ".")
	if len(selfRegistering) == 0 {
		t.Fatal("no self-registering notifier subpackage found under internal/notify — " +
			"the layout changed and this guard is inert")
	}

	imported := testBinaryImports(t, ".")
	for _, pkg := range selfRegistering {
		if !imported[notifierImportPrefix+pkg] {
			t.Errorf("internal/notify/%s calls notify.Register but no test file in this package imports it, "+
				"so it is missing from notify.Registered() here — add\n"+
				"\t_ \"%s%s\"\n"+
				"to the blank-import block in config_collision_test.go, or "+
				"TestRegisteredNotifierNamesDoNotCollideWithConfigFields silently skips this notifier "+
				"and it can ship with a name that shadows a named config.Notify field",
				pkg, notifierImportPrefix, pkg)
		}
	}
	t.Logf("%d self-registering notifier subpackage(s) on disk: %v", len(selfRegistering), selfRegistering)
}

// selfRegisteringPackages returns the slash-separated path, relative to dir, of
// every package under dir — AT ANY DEPTH — whose non-test Go source calls
// notify.Register: the qualified form a subpackage must use, since it cannot
// call the bare Register the way package notify's own slack.go and matrix.go do.
//
// The walk is recursive because nothing stops a notifier from living at
// internal/notify/<vendor>/<transport>, and one that does needs its own blank
// import exactly like a top-level one. An enumeration that read only the
// immediate children could not see it, and this guard — and with it the
// collision guard whose inputs it protects — would silently skip that notifier.
// Verified with a synthetic notifier at internal/notify/nested/inner
// registering under "thread": the single-level form reported it as absent from
// disk and passed.
func selfRegisteringPackages(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			return nil // registry.go, slack.go et al — package notify itself, not a drop-in
		}
		if path == dir {
			return nil
		}
		// The build tool ignores these, so nothing under them can be imported
		// and nothing under them can register.
		if name := e.Name(); name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return fs.SkipDir
		}
		if !registersNotifier(t, path) {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// registersNotifier reports whether any non-test Go file in dir contains a
// call to notify.Register.
func registersNotifier(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	forEachGoFile(t, dir, func(path string, file *ast.File) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Register" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "notify" {
				found = true
			}
			return true
		})
	})
	return found
}

// testBinaryImports returns every import path the _test.go files in dir pull
// in — the union that decides which init() functions run, and therefore what
// notify.Registered() reports inside this test binary.
func testBinaryImports(t *testing.T, dir string) map[string]bool {
	t.Helper()
	imported := map[string]bool{}
	forEachGoFile(t, dir, func(path string, file *ast.File) {
		if !strings.HasSuffix(path, "_test.go") {
			return
		}
		for _, spec := range file.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", path, spec.Path.Value, err)
			}
			imported[p] = true
		}
	})
	return imported
}

// forEachGoFile parses every .go file directly in dir — not in its
// subdirectories — and hands it to fn. Both callers ask a question about ONE
// package: which files registersNotifier may attribute to the package at dir,
// and which _test.go files are linked into THIS test binary. Descending here
// would answer neither (a nested package's Register call would be credited to
// its parent, and a subpackage's own test imports do not decide what runs in
// this binary); selfRegisteringPackages does the descending instead, one
// package directory at a time.
func forEachGoFile(t *testing.T, dir string, fn func(path string, file *ast.File)) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		fn(path, file)
	}
}
