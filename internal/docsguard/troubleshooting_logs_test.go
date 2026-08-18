// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// troubleshootingPath is the operations page that tells an operator which log
// line to grep for each symptom. Every claim on it is a quoted string that must
// exist in the code, and nothing compiles, imports, or executes the page — so a
// reworded log message leaves it directing operators at text the agent has never
// emitted, with no error anywhere.
const troubleshootingPath = "website/content/docs/operations/troubleshooting.md"

// quotedLogRE matches the page's own convention for quoting a log line:
// msg="…". Structured slog output renders the message under the `msg` key, so
// this is both how the docs write it and how an operator greps for it.
var quotedLogRE = regexp.MustCompile(`msg="([^"]*)"`)

// logMethods are the slog methods whose FIRST argument is the message an
// operator sees as msg=. The Context variants take ctx first; that shift is
// handled where the argument is read.
var logMethods = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
}

// logScanRoots are the trees searched for emitted messages, relative to the repo
// root.
var logScanRoots = []string{"internal", "cmd"}

// minQuotedLogLines and minEmittedLogLines are inertness floors. A regex that
// stopped matching, or a scan that stopped finding log calls, proves nothing and
// reports success — the same failure this package exists to catch, reproduced
// inside the catcher (see logsProviderIDs). The page quotes 29 messages against
// ~1000 emitted today; the floors sit well below both, so ordinary editing does
// not trip them.
const (
	minQuotedLogLines  = 20
	minEmittedLogLines = 300
)

// TestTroubleshootingQuotesRealLogLines pins every msg="…" on the troubleshooting
// page to a message the agent can actually emit.
//
// The page is a symptom-to-grep table: an operator reads "runlore_incidents_
// dropped_on_shutdown_total > 0" and is told to grep msg="held incident DROPPED:
// …". When the code's wording moves and the page does not, that grep returns
// nothing — which is indistinguishable from "the thing never happened", the
// single most misleading answer a troubleshooting page can give.
//
// It had drifted twice by the time this guard was written:
//
//   - msg="finding duplicates a catalog entry; not filing" — the code logs
//     "dedup: duplicates a catalog entry; not filing". The row is the one an
//     operator reads when RunLore declines to file a finding, so the grep that
//     confirms the dedup gate fired matched nothing.
//   - msg="investigation rate limit engaged; throttling…" — an ellipsis where
//     the real message continues "throttling new investigations this window".
//     A reader who greps the quoted string verbatim gets nothing back.
//
// The message is matched EXACTLY, not by prefix. An elided quote is precisely
// the failure above: it looks like documentation and does not work as a grep.
// Where a message is genuinely too long to quote whole, quote a shorter true
// substring outside msg="…" rather than an elided one inside it.
func TestTroubleshootingQuotesRealLogLines(t *testing.T) {
	root := repoRoot(t)

	page, err := os.ReadFile(filepath.Join(root, troubleshootingPath))
	if err != nil {
		t.Fatalf("read %s: %v", troubleshootingPath, err)
	}
	quoted := map[string][]int{} // message -> 1-based line numbers quoting it
	for i, line := range strings.Split(string(page), "\n") {
		for _, m := range quotedLogRE.FindAllStringSubmatch(line, -1) {
			quoted[m[1]] = append(quoted[m[1]], i+1)
		}
	}
	if len(quoted) < minQuotedLogLines {
		t.Fatalf("%s: found only %d distinct msg=\"…\" quotes, want at least %d — the page's quoting "+
			"convention changed and this guard is now inert, which is the exact failure it exists to catch",
			troubleshootingPath, len(quoted), minQuotedLogLines)
	}

	emitted := emittedLogMessages(t, root)
	if len(emitted) < minEmittedLogLines {
		t.Fatalf("found only %d log messages under %v, want at least %d — the scanner has stopped "+
			"recognising log calls, so every quote on the page passes unchecked",
			len(emitted), logScanRoots, minEmittedLogLines)
	}

	for _, msg := range unquotableMessages(quoted, emitted) {
		t.Errorf("%s:%v quotes msg=%q, which no log call in %v emits — an operator who greps it gets "+
			"nothing back, which reads as \"this never happened\" rather than \"the docs are stale\".%s",
			troubleshootingPath, quoted[msg], msg, logScanRoots, nearestHint(msg, emitted))
	}
}

// unquotableMessages returns the quoted messages that no log call emits.
//
// Extracted so TestTroubleshootingLogGuardDetectsDrift exercises the REAL
// matching rule rather than a restatement of it. Its first version asserted
// against the emitted map directly, which left the comparison above untested:
// relaxing it from equality to a prefix match — the exact way this guard would
// be weakened to make a truncated quote "pass" — changed nothing that failed.
//
// Equality, deliberately. A quote is a grep an operator will run verbatim, so a
// prefix or a substring is not a weaker check, it is the wrong one.
func unquotableMessages(quoted map[string][]int, emitted map[string]string) []string {
	var out []string
	for msg := range quoted {
		if _, ok := emitted[msg]; !ok {
			out = append(out, msg)
		}
	}
	sort.Strings(out)
	return out
}

// TestTroubleshootingLogGuardDetectsDrift is the guard's own mutation test: it
// runs unquotableMessages — the same matcher the guard above runs — over a
// synthetic page of near-misses, and requires every one to be reported. A guard
// that matched on a prefix, or that silently found nothing to check, would pass
// the test above forever.
func TestTroubleshootingLogGuardDetectsDrift(t *testing.T) {
	emitted := emittedLogMessages(t, repoRoot(t))

	const accurate = "dedup: duplicates a catalog entry; not filing"
	if got := unquotableMessages(map[string][]int{accurate: {1}}, emitted); len(got) != 0 {
		t.Fatalf("the guard rejected %q, a message internal/curator/curator.go really emits — "+
			"it would reject every accurate quote on the page too", accurate)
	}

	// Every way this page has drifted, or could. Each is a near-miss for the real
	// message above, and each must be reported: a quote is a grep an operator
	// runs verbatim, so "nearly right" returns nothing at all.
	drifted := map[string][]int{
		"dedup: duplicates a catalog entry":                 {1}, // truncated (a prefix of the real one)
		"dedup: duplicates a catalog entry; not filing…":    {2}, // elided with an ellipsis
		"finding duplicates a catalog entry; not filing":    {3}, // reworded — the drift actually found
		"Dedup: duplicates a catalog entry; not filing":     {4}, // case changed
		" dedup: duplicates a catalog entry; not filing":    {5}, // leading space
		"dedup: duplicates a catalog entry; not filing now": {6}, // extended
	}
	got := unquotableMessages(drifted, emitted)
	if len(got) != len(drifted) {
		accepted := make([]string, 0, len(drifted))
		for msg := range drifted {
			if !slices.Contains(got, msg) {
				accepted = append(accepted, msg)
			}
		}
		sort.Strings(accepted)
		t.Errorf("the guard accepted %d of %d drifted quotes (%q) — the match is no longer exact, "+
			"so a truncated or reworded quote passes and the page keeps directing operators at a "+
			"grep that returns nothing", len(accepted), len(drifted), accepted)
	}
}

// emittedLogMessages returns every string literal passed as the MESSAGE argument
// of an slog call under logScanRoots, mapped to the file it is emitted from.
//
// Parsed from the source rather than listed, for the reason logsProviderIDs
// gives: a hand-written list is an independent copy that drifts in exactly the
// direction the guard exists to catch. Non-test files only — a message that only
// a test emits is not one an operator can grep for.
//
// A message COMPUTED at runtime (a fmt.Sprintf, a concatenation) is skipped
// rather than approximated: it cannot be quoted verbatim on the page either, so a
// doc line claiming one is a doc line that was already wrong.
//
// A message held in a VARIABLE is not that case, and is resolved. `msg := "…"`
// followed by `log.Warn(msg, …)` puts the literal in the source exactly as an
// operator would grep it, and kbvalidate.WarnDraft picks its message that way to
// keep one set of attributes across two severities. Treating those as unemittable
// failed a page that quoted them correctly. Resolution is scoped to the literals
// assigned to that identifier inside the enclosing function, plus file-level
// consts, so it never invents a message no call site can reach.
func emittedLogMessages(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()

	for _, dir := range logScanRoots {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
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
			for _, decl := range f.Decls {
				fn, isFn := decl.(*ast.FuncDecl)
				if !isFn {
					continue
				}
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					at, ok := logMethods[sel.Sel.Name]
					if !ok || at >= len(call.Args) {
						return true
					}
					for _, msg := range messageLiterals(call.Args[at], fn, f) {
						if _, seen := out[msg]; !seen {
							out[msg] = path
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return out
}

// nearestHint names the emitted message a stale quote most likely drifted from,
// so the failure says what to write instead of only what is wrong. Chosen by
// longest shared word run, which is what a rewording leaves behind.
func nearestHint(msg string, emitted map[string]string) string {
	words := strings.Fields(msg)
	best, bestScore := "", 0
	for cand := range emitted {
		score := 0
		cw := strings.Fields(cand)
		for _, w := range words {
			for _, c := range cw {
				if w == c {
					score++
					break
				}
			}
		}
		if score > bestScore {
			best, bestScore = cand, score
		}
	}
	if bestScore < 2 {
		return ""
	}
	return "\n    closest emitted message: " + strconv.Quote(best) + " (" + emitted[best] + ")"
}

// messageLiterals yields the string literals an slog message argument can be at
// run time: the literal itself, or — when the argument is a plain identifier —
// every literal assigned to that name inside fn, plus any file-level const of
// that name. Anything else (a call, a concatenation, a field) yields nothing,
// because nothing greppable reaches the log line.
func messageLiterals(arg ast.Expr, fn *ast.FuncDecl, file *ast.File) []string {
	if lit, ok := arg.(*ast.BasicLit); ok {
		if lit.Kind != token.STRING {
			return nil
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return nil
		}
		return []string{s}
	}
	ident, ok := arg.(*ast.Ident)
	if !ok {
		return nil
	}

	var out []string
	add := func(e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		if s, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, s)
		}
	}
	bindings := func(names []ast.Expr, values []ast.Expr) {
		for i, n := range names {
			if id, ok := n.(*ast.Ident); ok && id.Name == ident.Name && i < len(values) {
				add(values[i])
			}
		}
	}

	ast.Inspect(fn, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			bindings(v.Lhs, v.Rhs)
		case *ast.ValueSpec:
			for i, name := range v.Names {
				if name.Name == ident.Name && i < len(v.Values) {
					add(v.Values[i])
				}
			}
		}
		return true
	})

	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name == ident.Name && i < len(vs.Values) {
					add(vs.Values[i])
				}
			}
		}
	}
	return out
}
