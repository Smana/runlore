// SPDX-License-Identifier: Apache-2.0

package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// warningSuffix is the naming convention every operator-facing startup guard in
// RunLore follows: WebhookAuthWarning, RecallDecayWarning,
// ChatWithoutCaptureWarning. Each is a pure function returning the message to
// log, or "" when no warning is warranted — which makes every one of them
// perfectly unit-testable in isolation, and perfectly inert if nobody logs what
// it returns.
const warningSuffix = "Warning"

// scanRoots are the directories walked for both declarations and call sites,
// relative to internal/app (go test sets the working directory to the package
// under test).
var scanRoots = []string{"../../internal", "../../cmd"}

// TestEveryStartupWarningIsEmitted closes a CLASS of bug this repo has now hit
// twice: a guard with complete unit coverage and zero production callers.
//
// ThreadCaptureDeliverable was written, tested, and left unreachable — its
// warning could never fire because the wiring checked a replier first, which
// implied the condition the guard diagnoses had already passed.
// ChatWithoutCaptureWarning was written, tested, and simply never called: it
// catches an operator who configured a paid model for a feature no transport
// can trigger, and it sat green and silent instead. Neither failed a test,
// because a function nobody calls is not a function anyone can observe.
//
// So this asserts the property directly rather than one instance of it: every
// *Warning function must have at least one non-test caller. The list is
// DERIVED from the naming convention, not hand-maintained, so a warning added
// tomorrow is covered the day it is written and cannot be forgotten out of an
// exemption list — the same reason notifier_imports_test.go reads the directory
// instead of comparing two hand-written lists.
//
// A deliberately unwired warning has no exemption here and is not meant to: the
// way to satisfy this test is to call the function, and if there is nowhere to
// call it from, the guard is not ready to be merged.
func TestEveryStartupWarningIsEmitted(t *testing.T) {
	declared, called := scanWarnings(t)

	if len(declared) == 0 {
		t.Fatal("no *Warning function found under internal/ or cmd/ — the naming convention changed " +
			"and this guard is inert, which is the exact failure it exists to catch")
	}
	for name, where := range declared {
		if !called[name] {
			t.Errorf("%s is declared at %s but no non-test file calls it: its warning can never reach an "+
				"operator's logs, so it is green and inert. Emit it on the startup path that owns the "+
				"feature it diagnoses (see WebhookAuthWarning in serve.go, ChatWithoutCaptureWarning in "+
				"buildThreadChat), or delete it.", name, where)
		}
	}
}

// TestEveryStartupWarningIsEmittedDetectsAnUnwiredGuard is the guard's own
// mutation test: a scanner that silently matched nothing would pass
// TestEveryStartupWarningIsEmitted forever. It feeds the collectors a synthetic
// package covering all three shapes and requires each to be classified right.
//
// DiscardedWarning is the case this fixture previously got backwards. It used
// `_ = OtherWarning()` as the WIRED example — so the guard's own self-test
// asserted that a discarded return counts as emitted, certifying as fixed the
// very bug the guard exists to catch. A discarded warning is now a NEGATIVE,
// alongside the never-called one.
func TestEveryStartupWarningIsEmittedDetectsAnUnwiredGuard(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n" +
		"\n" +
		"func InertGuardWarning() string { return \"\" }\n" +
		"func DiscardedWarning() string  { return \"\" }\n" +
		"func LoggedWarning() string     { return \"\" }\n" +
		"func QualifiedWarning() string  { return \"\" }\n" +
		"\n" +
		"func discards() { _ = DiscardedWarning() }\n" +
		"\n" +
		"func emits(log logger) {\n" +
		"\tif msg := LoggedWarning(); msg != \"\" {\n" +
		"\t\tlog.Warn(msg)\n" +
		"\t}\n" +
		"\tif msg := pkg.QualifiedWarning(); msg != \"\" {\n" +
		"\t\tlog.Warn(msg, \"key\", 1)\n" +
		"\t}\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	declared, called := scanWarningsIn(t, []string{dir})

	for _, name := range []string{"InertGuardWarning", "DiscardedWarning", "LoggedWarning", "QualifiedWarning"} {
		if _, ok := declared[name]; !ok {
			t.Fatalf("the declaration scanner missed %s — it would miss a real one too", name)
		}
	}
	if called["InertGuardWarning"] {
		t.Fatal("the call scanner reported a caller for a function nothing calls — the guard cannot fail")
	}
	if called["DiscardedWarning"] {
		t.Fatal("`_ = DiscardedWarning()` was counted as emitted: the result is thrown away, so no operator " +
			"can ever see it. Counting it is exactly how an inert guard passes this test.")
	}
	if !called["LoggedWarning"] {
		t.Fatal("a plain (unqualified) call whose message reaches log.Warn was missed — it would miss a real one too")
	}
	if !called["QualifiedWarning"] {
		t.Fatal("a qualified call (pkg.XWarning) whose message reaches log.Warn was missed — " +
			"config.ChatWithoutCaptureWarning has exactly this shape")
	}
	// Two guards binding the same idiomatic `msg` in one function must both be
	// seen; RunServe does exactly this, and an ident-keyed map would drop one.
	if !called["LoggedWarning"] || !called["QualifiedWarning"] {
		t.Fatal("two warnings bound to the same identifier in one function: both must be reported")
	}
}

// scanWarnings collects, from the shipped (non-test) tree, every *Warning
// declaration and every *Warning called.
func scanWarnings(t *testing.T) (declared map[string]string, called map[string]bool) {
	t.Helper()
	return scanWarningsIn(t, scanRoots)
}

// scanWarningsIn is scanWarnings over an explicit set of roots, so the guard's
// own mutation test can point it at a synthetic package.
//
// Calls are matched both as a bare identifier (a same-package call, e.g.
// WebhookAuthWarning in serve.go) and as a selector (config.ChatWithoutCapture
// Warning), since the two live in different packages and a scanner that handled
// only one shape would wave the other through.
func scanWarningsIn(t *testing.T, roots []string) (declared map[string]string, called map[string]bool) {
	t.Helper()
	declared, called = map[string]string{}, map[string]bool{}
	fset := token.NewFileSet()

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				// Methods are excluded: the convention is a package-level pure
				// function, and a method named *Warning would be a different
				// shape with a different call site to look for.
				if fn.Recv == nil && strings.HasSuffix(fn.Name.Name, warningSuffix) {
					declared[fn.Name.Name] = path
				}
				// A call is only counted from inside the function that contains
				// it, so the binding it produces and the logger that consumes it
				// are matched within one scope.
				for name := range loggedWarningsIn(fn) {
					called[name] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return declared, called
}

// loggerMethods are the slog methods a warning's message may reach. A *Warning
// whose result flows into one of these has been emitted; one whose result is
// discarded, or merely compared and dropped, has not.
var loggerMethods = map[string]bool{
	"Warn": true, "Warnf": true,
	"Error": true, "Errorf": true,
	"Info": true, "Infof": true,
}

// loggedWarningsIn returns the *Warning functions called inside fn whose
// returned message demonstrably reaches a logger.
//
// Merely finding a call is not enough, and that gap is the reason this helper
// exists. Every one of these guards is a pure function returning a string, so
// `_ = ChatWithoutCaptureWarning(cfg)` compiles, runs, satisfies a
// call-site-exists check, and emits precisely nothing — which is the exact
// failure TestEveryStartupWarningIsEmitted claims to catch. The earlier version
// counted any call, and its own synthetic fixture used the discarded form as
// the WIRED example, so the guard certified the bug as fixed.
//
// The shape recognised is the one all three real call sites use:
//
//	if msg := SomeWarning(x); msg != "" {
//	    log.Warn(msg)
//	}
//
// i.e. the result is bound to a non-blank identifier, and that identifier is
// then passed to a logger method somewhere in the same function. Requiring the
// same function keeps this a syntactic check with no type information, which is
// what lets it run over ../../internal and ../../cmd without building them.
func loggedWarningsIn(fn *ast.FuncDecl) map[string]bool {
	if fn.Body == nil {
		return nil
	}
	// binding: identifier name -> every *Warning whose result it has held.
	//
	// A SET per identifier, not one name: RunServe emits several guards and each
	// uses the same idiomatic `msg` in its own if-scope, so a map keyed by
	// identifier alone would let the last binding evict the earlier ones and
	// report a genuinely-wired warning as inert.
	binding := map[string]map[string]bool{}
	// logged: identifiers passed to a logger method anywhere in this function.
	logged := map[string]bool{}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for i, rhs := range as.Rhs {
				name := warningCallName(rhs)
				if name == "" || i >= len(as.Lhs) {
					continue
				}
				// `_ = SomeWarning()` binds nothing and can never be logged.
				if id, ok := as.Lhs[i].(*ast.Ident); ok && id.Name != "_" {
					if binding[id.Name] == nil {
						binding[id.Name] = map[string]bool{}
					}
					binding[id.Name][name] = true
				}
			}
		}
		if call, ok := n.(*ast.CallExpr); ok {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !loggerMethods[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				if id, ok := arg.(*ast.Ident); ok {
					logged[id.Name] = true
				}
			}
		}
		return true
	})

	out := map[string]bool{}
	for ident, warnings := range binding {
		if !logged[ident] {
			continue
		}
		for warning := range warnings {
			out[warning] = true
		}
	}
	return out
}

// warningCallName reports the *Warning function e calls, or "" if e is not such
// a call. Both shapes are recognised: a same-package bare identifier
// (WebhookAuthWarning in serve.go) and a qualified selector
// (config.ChatWithoutCaptureWarning).
func warningCallName(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		if strings.HasSuffix(fn.Name, warningSuffix) {
			return fn.Name
		}
	case *ast.SelectorExpr:
		if strings.HasSuffix(fn.Sel.Name, warningSuffix) {
			return fn.Sel.Name
		}
	}
	return ""
}
