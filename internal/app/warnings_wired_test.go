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

// warningRaiser is the logger method a warning's message must reach. Narrow on
// purpose: every shipped warning uses log.Warn, and a set that also accepted
// Info or Error would wave through a guard demoted to a level operators filter
// out. Widen it deliberately, with the reason, if a warning ever belongs
// elsewhere.
const warningRaiser = "Warn"

// minScannedFiles is the floor on how many non-test .go files the walk must
// actually parse, and it exists because "found no problems" and "stopped
// looking" are the same green.
//
// A guard whose roots shrink — to package app, to one directory, to a path that
// no longer exists — still discovers the warnings that remain in view, still
// finds them correctly wired, and still passes, while every warning outside the
// new roots goes unchecked. That is not hypothetical: scanning package app alone
// is exactly what the serve_guard half of this did, and it is why
// ChatWithoutCaptureWarning (declared in internal/config) could sit uncalled
// under a green suite.
//
// 150 against ~221 files today: comfortably above package app on its own (26),
// with room for the tree to shrink without a false alarm.
const minScannedFiles = 150

// warningSite is one non-test call of a *Warning function: the file it sits in,
// and the top-level statement of the enclosing function body that contains it.
// The STATEMENT, not the function, is the scope the raise is judged in — see
// TestEveryStartupWarningIsRaisedExactlyOnce.
type warningSite struct {
	file string
	stmt ast.Stmt
}

// TestEveryStartupWarningIsRaisedExactlyOnce closes a CLASS of bug this repo has
// now hit several times: a guard with complete unit coverage and zero — or
// merely decorative — production wiring.
//
// ThreadCaptureDeliverable was written, tested, and left unreachable: its
// warning could never fire because the wiring checked a replier first, which
// implied the condition the guard diagnoses had already passed.
// ChatWithoutCaptureWarning was written, tested, and simply never called — it
// catches an operator who configured a paid model for a feature no transport can
// trigger, and it sat green and silent instead. `_ = RecallDecayWarning(cfg)`,
// computed and thrown away, is the state an interrupted edit actually left on a
// branch. None of them failed a test, because a function nobody calls — or whose
// result nobody logs — is not a function anyone can observe.
//
// So this asserts the property directly rather than one instance of it. Per
// *Warning function, all four parts:
//
//  1. the set is DISCOVERED from the naming convention rather than hand-listed,
//     so a warning added tomorrow is covered the day it is written and cannot be
//     forgotten out of an exemption list — the same reason
//     notifier_imports_test.go reads the directory instead of comparing two
//     hand-written lists;
//  2. across internal/ AND cmd/, in both call spellings — a bare identifier for
//     a same-package call (WebhookAuthWarning in serve.go) and a selector for a
//     cross-package one (config.ChatWithoutCaptureWarning) — since a scanner
//     handling only one shape waves the other through;
//  3. EXACTLY ONE non-test call site: zero leaves the condition neither enforced
//     nor signalled, two prints the same paragraph at operators twice;
//  4. at that site the result is bound to a real variable (not `_`) and that
//     variable reaches a `.Warn(...)` IN THE SAME STATEMENT. Same statement, not
//     merely the same function: `if msg := X(); msg != "" { log.Warn(msg) }` is
//     the shape all five shipped sites use, and anything looser lets a binding
//     be "raised" by an unrelated log call elsewhere in the function.
//
// This is one guard where there were two, written on branches that could not see
// each other. The older warnings_wired guard scanned internal/+cmd/ and both call
// spellings, but asked only that the message reach a logger somewhere in the same
// function; serve_guard's TestEveryStartupWarningIsRaised required exactly one
// call site and a same-statement raise, but looked only at package app and only
// at unqualified calls. Each waved through what the other caught. This takes the
// stronger half of every axis, and TestEveryStartupWarningGuardDetectsItsOwnInertness
// proves it still holds each one rather than having quietly averaged the two.
//
// A deliberately unwired warning has no exemption here and is not meant to: the
// way to satisfy this test is to raise the function, and if there is nowhere to
// raise it from, the guard is not ready to be merged.
//
// KNOWN, ACCEPTED FALSE POSITIVE (inherited, deliberately): splitting the bind
// and the Warn across two statements fails here even though it is
// behaviour-preserving. That is the trade — a pin that costs an occasional
// refactor an explicit update is worth far more than one that green-lights a
// silent regression.
func TestEveryStartupWarningIsRaisedExactlyOnce(t *testing.T) {
	declared, sites, scanned := scanWarnings(t)

	if scanned < minScannedFiles {
		t.Fatalf("the walk parsed only %d non-test .go files under %v, want at least %d — the scanner has "+
			"stopped seeing most of the tree, so every warning outside what it still reaches is unchecked "+
			"while this suite stays green", scanned, scanRoots, minScannedFiles)
	}
	if len(declared) == 0 {
		t.Fatal("no *Warning function found under internal/ or cmd/ — the naming convention changed " +
			"and this guard is inert, which is the exact failure it exists to catch")
	}
	for name, where := range declared {
		switch got := sites[name]; len(got) {
		case 1:
			assertWarningIsRaised(t, got[0].file, got[0].stmt, name, warningRaiser)
		case 0:
			t.Errorf("%s is declared at %s but no non-test file calls it: its warning can never reach an "+
				"operator's logs, so it is green and inert. Raise it on the startup path that owns the "+
				"feature it diagnoses (see WebhookAuthWarning in serve.go, ChatWithoutCaptureWarning in "+
				"buildThreadChat), or delete it.", name, where)
		default:
			at := make([]string, 0, len(got))
			for _, s := range got {
				at = append(at, s.file)
			}
			t.Errorf("%s (declared at %s) has %d non-test call sites (%s), want exactly 1 — a warning "+
				"nobody raises leaves the condition it describes neither enforced nor signalled, and two "+
				"raisers print it twice", name, where, len(got), strings.Join(at, ", "))
		}
	}
}

// TestEveryStartupWarningGuardDetectsItsOwnInertness is the guard's own mutation
// test: a scanner that silently matched nothing would pass
// TestEveryStartupWarningIsRaisedExactlyOnce forever. It feeds the collectors a
// synthetic package covering every shape and requires each to be classified
// right — with one fixture per property the merge could have dropped, so a
// merged guard that checks LESS than either of the two it replaced fails here.
//
// DiscardedWarning is the case an earlier fixture got backwards. It used
// `_ = OtherWarning()` as the WIRED example — so the guard's own self-test
// asserted that a discarded return counts as raised, certifying as fixed the
// very bug the guard exists to catch.
func TestEveryStartupWarningGuardDetectsItsOwnInertness(t *testing.T) {
	dir := t.TempDir()
	src := "package x\n" +
		"\n" +
		"func InertGuardWarning() string { return \"\" }\n" +
		"func DiscardedWarning() string  { return \"\" }\n" +
		"func LoggedWarning() string     { return \"\" }\n" +
		"func QualifiedWarning() string  { return \"\" }\n" +
		"func TwiceWarning() string      { return \"\" }\n" +
		"func TwiceInOneFuncWarning() string { return \"\" }\n" +
		"func FarLoggedWarning() string  { return \"\" }\n" +
		"func InfoOnlyWarning() string   { return \"\" }\n" +
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
		"\tif msg := InfoOnlyWarning(); msg != \"\" {\n" +
		"\t\tlog.Info(msg)\n" +
		"\t}\n" +
		"\tfar := FarLoggedWarning()\n" +
		"\tlog.Warn(far)\n" +
		"}\n" +
		"\n" +
		"func twiceInOneFunc(log logger) {\n" +
		"\tif msg := TwiceInOneFuncWarning(); msg != \"\" {\n" +
		"\t\tlog.Warn(msg)\n" +
		"\t}\n" +
		"\tif msg := TwiceInOneFuncWarning(); msg != \"\" {\n" +
		"\t\tlog.Warn(msg)\n" +
		"\t}\n" +
		"}\n" +
		"\n" +
		"func twiceA(log logger) {\n" +
		"\tif msg := TwiceWarning(); msg != \"\" {\n" +
		"\t\tlog.Warn(msg)\n" +
		"\t}\n" +
		"}\n" +
		"\n" +
		"func twiceB(log logger) {\n" +
		"\tif msg := TwiceWarning(); msg != \"\" {\n" +
		"\t\tlog.Warn(msg)\n" +
		"\t}\n" +
		"}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	declared, sites, scanned := scanWarningsIn(t, []string{dir})
	if scanned != 1 {
		t.Fatalf("the fixture package is one file; scanned=%d, so the file counter does not count files", scanned)
	}

	for _, name := range []string{
		"InertGuardWarning", "DiscardedWarning", "LoggedWarning", "QualifiedWarning",
		"TwiceWarning", "TwiceInOneFuncWarning", "FarLoggedWarning", "InfoOnlyWarning",
	} {
		if _, ok := declared[name]; !ok {
			t.Fatalf("the declaration scanner missed %s — it would miss a real one too", name)
		}
	}

	// COUNTING. Zero and two are both failures, and the pre-merge "is it called at
	// all?" property could not see the second.
	if n := len(sites["InertGuardWarning"]); n != 0 {
		t.Errorf("the call scanner reported %d call sites for a function nothing calls — the guard cannot fail", n)
	}
	if n := len(sites["TwiceWarning"]); n != 2 {
		t.Errorf("two call sites in two functions were counted as %d — the exactly-once property is gone, "+
			"and a warning printed twice at startup now passes", n)
	}
	// Two raises of the same warning in ONE function. This is the case a
	// map[string]ast.Stmt collapses to a single site, and it is the shape a
	// duplicate actually takes — a second raise pasted into RunServe beside the
	// first, not a second raiser in a second function. The two-function case above
	// passes against that bug, which is why both are here.
	if n := len(sites["TwiceInOneFuncWarning"]); n != 2 {
		t.Errorf("two call sites in ONE function were counted as %d — a duplicate raise added next to the "+
			"existing one is invisible, which is the likeliest way this regresses", n)
	}
	// A qualified call and a bare one bound to the SAME idiomatic `msg` in one
	// function must both be seen: RunServe does exactly this, and an ident-keyed
	// map would let one binding evict the other.
	for _, name := range []string{"LoggedWarning", "QualifiedWarning", "DiscardedWarning"} {
		if n := len(sites[name]); n != 1 {
			t.Errorf("%s: got %d call sites, want 1 — config.ChatWithoutCaptureWarning has the qualified "+
				"shape, and `_ = X()` is still a CALL (it is the raise it fails, not the call)", name, n)
		}
	}

	// RAISING. Run the same assertion the real guard runs, against a recorder, and
	// require each fixture to land on the right side of it.
	for _, tc := range []struct {
		name      string
		wantRaise bool
		why       string
	}{
		{"LoggedWarning", true, "a bare call bound to msg and passed to log.Warn in the same statement is the shipped shape"},
		{"QualifiedWarning", true, "a qualified call raised the same way must be accepted too"},
		{"DiscardedWarning", false, "`_ = DiscardedWarning()` throws the message away; counting it as raised is exactly how an inert guard passes"},
		{"FarLoggedWarning", false, "bound in one statement and logged in the NEXT is not the same statement — " +
			"accepting it restores the same-function looseness the merge removed"},
		{"InfoOnlyWarning", false, "raised at Info, not Warn — a warning demoted to a level operators filter out is not raised"},
	} {
		got := sites[tc.name]
		if len(got) != 1 {
			t.Errorf("%s: want 1 call site to assert against, got %d", tc.name, len(got))
			continue
		}
		rec := &testing.T{}
		assertWarningIsRaised(rec, got[0].file, got[0].stmt, tc.name, warningRaiser)
		if raised := !rec.Failed(); raised != tc.wantRaise {
			t.Errorf("%s: assertWarningIsRaised said raised=%v, want %v — %s",
				tc.name, raised, tc.wantRaise, tc.why)
		}
	}
}

// scanWarnings collects, from the shipped (non-test) tree, every *Warning
// declaration and every non-test site that calls one.
func scanWarnings(t *testing.T) (declared map[string]string, sites map[string][]warningSite, scanned int) {
	t.Helper()
	return scanWarningsIn(t, scanRoots)
}

// scanWarningsIn is scanWarnings over an explicit set of roots, so the guard's
// own mutation test can point it at a synthetic package.
//
// declared maps a warning's name to the file declaring it; sites maps it to
// every non-test call, each carrying the top-level statement containing it —
// which is the scope assertWarningIsRaised judges. scanned is how many non-test
// .go files were actually parsed; the caller asserts a floor on it, because a
// scanner that quietly stops looking at most of the tree finds nothing to
// complain about and reports success — see minScannedFiles.
func scanWarningsIn(t *testing.T, roots []string) (declared map[string]string, sites map[string][]warningSite, scanned int) {
	t.Helper()
	declared, sites = map[string]string{}, map[string][]warningSite{}
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
			scanned++
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				// Methods are excluded: the convention is a package-level pure
				// function, and a method named *Warning would be a different
				// shape with a different call site to look for.
				if fn.Recv == nil && strings.HasSuffix(fn.Name.Name, warningSuffix) {
					declared[fn.Name.Name] = path
				}
				for name, stmts := range warningCallsIn(fn) {
					for _, stmt := range stmts {
						sites[name] = append(sites[name], warningSite{file: filepath.Base(path), stmt: stmt})
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return declared, sites, scanned
}

// warningCallsIn returns every *Warning called inside fn, mapped to the
// top-level statement of fn's body containing each call — the scope the raise
// must happen in. A warning calling itself is not a call site.
//
// A SLICE per name, not a single statement: two calls to the same warning inside
// ONE function are two call sites. An earlier draft returned
// map[string]ast.Stmt and silently collapsed them into one, so the
// exactly-once property held only across functions — and its fixture put the
// duplicate pair in two different functions, so it never noticed. The shape that
// actually happens is a second raise pasted into RunServe beside the first.
func warningCallsIn(fn *ast.FuncDecl) map[string][]ast.Stmt {
	out := map[string][]ast.Stmt{}
	// stack tracks the ancestry of the node being visited, so a call site can be
	// resolved back to the statement it sits in, not merely to its function.
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		name := warningCallName(n)
		if name == "" || name == fn.Name.Name {
			return true
		}
		out[name] = append(out[name], outermostStmt(stack))
		return true
	})
	return out
}

// warningCallName reports the *Warning function n calls, or "" if n is not such
// a call. Both shapes are recognised: a same-package bare identifier
// (WebhookAuthWarning in serve.go) and a qualified selector
// (config.ChatWithoutCaptureWarning).
func warningCallName(n ast.Node) string {
	call, ok := n.(*ast.CallExpr)
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
