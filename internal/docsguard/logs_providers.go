// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// logsProviderIDs returns the value of every `Provider*` constant declared in
// internal/logs/detect.go, read from the source file itself.
//
// Why parse rather than list them: Go cannot enumerate a package's constants at
// runtime, so the obvious implementation is a hand-written slice — and that is
// what this replaced. The slice named detect.go as its source while being an
// independent copy of it, so adding a provider left the guard silently
// asserting against a stale set: the new provider's page was rejected as "not
// registered" even though it was, and (worse, in the other direction) deleting
// a provider constant left the guard still demanding its page.
//
// That is the same drift this whole package exists to catch, reproduced inside
// the catcher. Reading the real declaration is the only version that cannot
// disagree with it.
func logsProviderIDs(root string) ([]string, error) {
	path := filepath.Join(root, "internal", "logs", "detect.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	var ids []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Provider") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					return nil, fmt.Errorf("%s: unquote %s: %w", path, name.Name, err)
				}
				ids = append(ids, v)
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%s: no Provider* string constants found — the declaration "+
			"shape changed and this guard is now inert", path)
	}
	return ids, nil
}
