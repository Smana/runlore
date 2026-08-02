// SPDX-License-Identifier: Apache-2.0

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestWireRecallSetsEveryRuntimeDependency pins what a Recall must be given.
//
// Recall documents `Outcome OutcomeStats // optional; nil ⇒ no outcome decay`, so
// an unset ledger silently removes one of the three recall gates rather than
// failing. That is invisible at runtime: recall still fires, just without the
// outcome-weighted confidence that would have suppressed a poorly-performing
// entry.
func TestWireRecallSetsEveryRuntimeDependency(t *testing.T) {
	r := &investigate.Recall{}
	ledger := &outcome.Ledger{}
	log := slog.New(slog.DiscardHandler)
	m := (*telemetry.Metrics)(nil)

	wireRecall(r, m, ledger, log)

	if r.Outcome == nil {
		t.Error("Outcome is nil — recall runs without the outcome-weighted gate")
	}
	if r.Log == nil {
		t.Error("Log is nil — recall decisions go unrecorded")
	}

	// A nil ledger is a legitimate configuration (the operator disabled it), so it
	// must not panic and must leave decay off rather than inventing a ledger.
	r2 := &investigate.Recall{}
	wireRecall(r2, m, nil, log)
	if r2.Outcome != nil {
		t.Error("a nil ledger must leave Outcome nil, not synthesize one")
	}

	wireRecall(nil, m, ledger, log) // must not panic
}

// TestWireRecallIsTheOnlyPlaceRecallDepsAreSet is the guard that matters.
//
// The bug this fixes was not a wrong value — it was a SECOND place that wired a
// Recall by hand and set one fewer field than the first. Both compiled, both ran,
// and only one had the outcome gate. Centralizing the wiring only helps if it
// stays centralized, so this fails when any other function in the package assigns
// Recall's runtime fields directly.
func TestWireRecallIsTheOnlyPlaceRecallDepsAreSet(t *testing.T) {
	const guarded = "wireRecall"
	fields := map[string]bool{"Outcome": true, "Metrics": true, "Log": true}

	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	inspected := 0

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // test-owned path in this package
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// Only files that mention Recall at all can contain the assignment.
		if !strings.Contains(string(src), "Recall") {
			continue
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		inspected++

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name == guarded || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || !fields[sel.Sel.Name] {
						continue
					}
					id, ok := sel.X.(*ast.Ident)
					if !ok || !strings.Contains(strings.ToLower(id.Name), "recall") {
						continue
					}
					t.Errorf("%s: %s sets %s.%s directly — route it through %s() so every "+
						"path that builds a Recall gets the same dependencies",
						filepath.Base(path), fn.Name.Name, id.Name, sel.Sel.Name, guarded)
				}
				return true
			})
		}
	}

	if inspected == 0 {
		t.Fatal("no source file mentioning Recall was parsed — this guard is inert")
	}
}
