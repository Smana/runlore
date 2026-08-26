// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestWidenedBannerMatchesCloudToolsConstant is a cross-package guard on a banner
// that exists twice: AWSCloudVocabulary().WidenedBanner renders it for the day
// cloud_what_changed renders from the vocabulary (Task 2); cloud_tools.go's
// unexported widenedBanner constant renders it TODAY, and is the only one a real
// investigation ever sees until Task 2 lands. Rename one and the "resource matched
// nothing, retrying unscoped" banner a user actually sees keeps stating the OLD
// scope-match rule — with both packages' own tests still green, because each tests
// only its own copy. As internal/notify/slack_silence_blockid_guard_test.go:17-36
// puts it: a "must match" comment on both sides is not a guard.
//
// widenedBanner is unexported, and exporting it purely so this test could import it
// would trade cloud_tools.go's ownership of its own literal for this test's
// convenience — the same tradeoff internal/notify declined for
// silenceBlockIDPrefix. So this reads cloud_tools.go's source instead of importing
// it, exactly as that guard does.
func TestWidenedBannerMatchesCloudToolsConstant(t *testing.T) {
	const investigateDir = "../investigate"
	const constName = "widenedBanner"

	entries, err := os.ReadDir(investigateDir)
	if err != nil {
		t.Fatalf("read %s: %v", investigateDir, err)
	}
	fset := token.NewFileSet()
	var template string
	found := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(investigateDir, name)
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		if s, ok := constStringLiteral(file, constName); ok {
			template, found = s, true
		}
	}
	if !found {
		t.Fatalf("no const %s found under %s: the guard has gone blind — either the constant was renamed or "+
			"moved, or Task 2 already replaced it with vocabulary rendering, and this test must be taught the "+
			"new shape (or deleted, if AWSCloudVocabulary().WidenedBanner is now the only implementation left)",
			constName, investigateDir)
	}

	const sentinel = "apps/team/some-resource"
	want := fmt.Sprintf(template, sentinel)
	got := providers.AWSCloudVocabulary().WidenedBanner(sentinel)
	if got != want {
		t.Errorf("AWSCloudVocabulary().WidenedBanner drifted from cloud_tools.go's widenedBanner const\n got:  %q\nwant: %q", got, want)
	}
}

// constStringLiteral evaluates a top-level `const <name> = <string expression>`
// declaration in file, following the `+`-concatenation cloud_tools.go writes its
// long banners with across several lines. Anything it does not recognize (a
// non-string value, an expression shape other than literal-plus-literal) reports
// not-found rather than guessing, so a rewrite this simple evaluator cannot follow
// fails the test loudly via "guard has gone blind" instead of silently comparing
// against a wrong value.
func constStringLiteral(file *ast.File, name string) (string, bool) {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				if s, ok := evalStringExpr(vs.Values[i]); ok {
					return s, true
				}
			}
		}
	}
	return "", false
}

// evalStringExpr evaluates a string-literal expression, including `+`-concatenated
// chains of literals, without pulling in go/types — the guard only ever needs to
// read a const built from plain string literals, never a general Go expression.
func evalStringExpr(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := evalStringExpr(v.X)
		if !ok {
			return "", false
		}
		r, ok := evalStringExpr(v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	default:
		return "", false
	}
}
