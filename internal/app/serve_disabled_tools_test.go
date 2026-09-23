// SPDX-License-Identifier: Apache-2.0

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestDisabledToolsNoticeIsEmittedAtServeStartup pins #467: `lore serve` names the
// investigation tools it runs WITHOUT, once, on the startup path — exactly as
// `lore investigate` already does on stderr. serve announces at Info everything it
// turned ON (the ledger, the coalescer, the debounce, the watcher, ...), so an
// install missing its evidence backends looked identical in the logs to one that has
// them all, and the `minimal` Helm profile — no metrics.url, no logs.url — was
// indistinguishable from a misconfigured full one.
//
// The notice reuses disabledTools, the CLI's pure function over config, so the two
// commands can never disagree about what counts as "off". It is pinned the way the
// recall-decay warning is (TestRecallDecayWarningIsEmittedOnceAtStartup): RunServe
// must call it directly in its own body — not inside a closure that runs per alert —
// exactly once, bind the result to a real variable, and pass that variable to a
// .Info(...) in the same statement. Info, not Warn: it fires on every boot of a
// deliberately-minimal install, and a warning that fires on purpose is tuned out.
//
// Unlike the recall-decay pin, other callers are NOT an error: the CLI is one.
func TestDisabledToolsNoticeIsEmittedAtServeStartup(t *testing.T) {
	const guarded, caller, raiser, file = "disabledTools", "RunServe", "Info", "serve.go"

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	calls := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name != caller {
			continue
		}
		// stack tracks the ancestry of the visited node, so a call site is judged by
		// WHERE it sits — a FuncLit inside RunServe is a closure handed to the incident
		// path, not startup.
		var stack []ast.Node
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok || callName(call) != guarded {
				return true
			}
			calls++
			for _, anc := range stack[:len(stack)-1] {
				if _, isLit := anc.(*ast.FuncLit); isLit {
					t.Errorf("%s: %s calls %s inside a function literal — that runs whenever the "+
						"closure runs, not once at startup", file, caller, guarded)
					return true
				}
			}
			assertWarningIsRaised(t, file, outermostStmt(stack), guarded, raiser)
			return true
		})
	}
	if calls != 1 {
		t.Fatalf("%s must call %s exactly once (got %d) — otherwise the startup notice is "+
			"either absent or duplicated", caller, guarded, calls)
	}
}
