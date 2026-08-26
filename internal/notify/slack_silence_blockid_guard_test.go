// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSilenceBlockIDPrefixMatchesTheServerHandler is a cross-package guard on a
// constant that exists twice and has to agree byte for byte.
//
// slack.go stamps the silence control's block_id as silenceBlockIDPrefix +
// TriggerKey; internal/server's interactions handler strips that prefix back off
// to recover the key. internal/server deliberately does not import internal/notify
// (the HTTP layer knows nothing about renderers, and the constant is unexported
// here), so the handler carries its own literal. Rename one and every real click
// lands in the "could not identify the incident to silence" branch — with all
// tests still green, because each package tests its own half against its own copy.
// A "must match" comment on both sides is not a guard.
//
// Reading the OTHER package's source rather than importing it is what makes the
// guard possible at all: the constant is unexported, and exporting it purely to be
// asserted on would trade a real layering boundary for a test's convenience. This
// repo already parses source for the same reason — see internal/docsguard and
// TestActionsInterfaceFieldsAreNeverAssignedRawBuilderResults in internal/app.
//
// Every non-test file in internal/server is parsed, so moving the
// handler between files inside it does not break the guard; only changing what it
// strips does.
func TestSilenceBlockIDPrefixMatchesTheServerHandler(t *testing.T) {
	const serverDir = "../server"

	entries, err := os.ReadDir(serverDir)
	if err != nil {
		t.Fatalf("read %s: %v", serverDir, err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(serverDir, name)
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := blockIDPrefixLiteral(n)
			if !ok {
				return true
			}
			found++
			if lit != silenceBlockIDPrefix {
				t.Errorf("%s strips block_id prefix %q, but notify stamps %q — a real click would fail to identify the incident",
					path, lit, silenceBlockIDPrefix)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatalf("no strings.CutPrefix(<...>.BlockID, \"…\") call found in %s: the guard has gone blind — "+
			"either the handler stopped recovering the TriggerKey from block_id, or it now does it some other way "+
			"and this test must be taught the new shape", serverDir)
	}
}

// blockIDPrefixLiteral reports the string literal a node strips off a BlockID
// field: it matches `strings.CutPrefix(<expr>.BlockID, "literal")` and nothing
// else, so an unrelated CutPrefix elsewhere in internal/server cannot make the
// guard fire — or, worse, satisfy its "at least one" check.
func blockIDPrefixLiteral(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 2 {
		return "", false
	}
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || fn.Sel.Name != "CutPrefix" {
		return "", false
	}
	if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "strings" {
		return "", false
	}
	if field, ok := call.Args[0].(*ast.SelectorExpr); !ok || field.Sel.Name != "BlockID" {
		return "", false
	}
	lit, ok := call.Args[1].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}
