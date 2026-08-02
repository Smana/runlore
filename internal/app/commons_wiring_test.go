// SPDX-License-Identifier: Apache-2.0

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestEveryCatalogPathArmsCommons guards against the two construction paths
// diverging.
//
// BuildCatalog returns a catalog from two places — the git-sync branch and the
// plain-directory branch — and each must arm the commons root. Wiring only one
// means the commons silently works or doesn't depending on whether the operator
// happens to use git-sync, with no error either way.
//
// That is not a hypothetical failure mode in this package: the investigator and the
// reinvestigator each hand-wired a Recall, drifted, and left the reinvestigate path
// running without the outcome gate (#414). Same shape, so the same guard.
func TestEveryCatalogPathArmsCommons(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "catalog.go", nil, 0)
	if err != nil {
		t.Fatalf("parse catalog.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "BuildCatalog" {
			fn = fd
			break
		}
	}
	if fn == nil || fn.Body == nil {
		t.Fatal("BuildCatalog not found — this guard is inert")
	}

	// Count the `return cat` statements (each is a construction path) and the
	// armCommons calls. They must match.
	var returnsCat, armCalls int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ReturnStmt:
			for _, r := range v.Results {
				if id, ok := r.(*ast.Ident); ok && id.Name == "cat" {
					returnsCat++
				}
			}
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "armCommons" {
				armCalls++
			}
		}
		return true
	})

	if returnsCat == 0 {
		t.Fatal("no `return cat` found in BuildCatalog — the shape changed and this guard is inert")
	}
	if armCalls != returnsCat {
		t.Errorf("BuildCatalog has %d catalog-returning paths but only %d armCommons calls — "+
			"a path that skips it leaves the commons silently absent, with no error",
			returnsCat, armCalls)
	}
}
