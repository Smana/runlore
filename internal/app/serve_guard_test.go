// SPDX-License-Identifier: Apache-2.0

package app

// TestServeGuardFailClosed locks the fail-closed invariant for RequireWebhookAuth
// on the serve startup path (internal/app/config.go): a model-configured deployment
// with an empty webhook token must refuse to start; a non-empty token must be
// accepted. This is entirely self-contained — it constructs configs in-code so it
// does not depend on any YAML file that another workstream may be editing.
//
// The invariant being locked: once a model is wired, every alert webhook POST
// drives a paid LLM call, so an anonymous webhook would let anyone in the network
// run up an arbitrary bill (or poison the investigation history). The guard lives
// on the serve path only (not in config.Validate) because `lore investigate`
// legitimately needs a model without a webhook.
import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func TestServeGuardFailClosed(t *testing.T) {
	t.Run("model configured, no webhook token → refused (fail closed)", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Model.Provider = "anthropic" // built-in endpoint; no base_url needed
		// Server.WebhookTokenEnv names the env var — it is set here to show intent,
		// but the resolved token (second arg) is what matters for the guard.
		cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"

		if err := RequireWebhookAuth(cfg, "" /* empty resolved token */); err == nil {
			t.Fatal("RequireWebhookAuth must refuse when model is configured and webhook token is empty")
		}
	})

	t.Run("model configured, webhook token set → accepted", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Model.Provider = "anthropic"
		cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"

		if err := RequireWebhookAuth(cfg, "some-secret-token"); err != nil {
			t.Fatalf("RequireWebhookAuth must accept when model is configured and webhook token is non-empty: %v", err)
		}
	})

	t.Run("no model, no webhook token → accepted (log-only investigator has no billing exposure)", func(t *testing.T) {
		cfg := &config.Config{} // zero Model: no provider, no base_url
		if err := RequireWebhookAuth(cfg, ""); err != nil {
			t.Fatalf("RequireWebhookAuth must accept when no model is configured: %v", err)
		}
	})
}

// TestRecallDecayWarningIsEmittedOnceAtStartup pins that the warning is RAISED —
// computed, and actually logged — exactly once, on the serve startup path.
//
// The condition is pure config, so it is equally true on every incident. Called from
// the investigation path (or from wireRecall, or the recall gate) it would repeat the
// same paragraph on every alert and be muted within a day. It belongs on the serve
// startup path, once, next to the other startup config warnings.
//
// Counting call sites is NOT enough, and an earlier version of this guard that did
// only that waved through both of the ways this has actually broken:
//
//   - `_ = RecallDecayWarning(cfg)` — computed and thrown away. This is the exact
//     state an interrupted edit left on this branch: one call inside RunServe, guard
//     green, warning never printed.
//   - the call moved into a FuncLit that RunServe merely DEFINES (e.g. the per-alert
//     `resolve` closure) — still lexically inside RunServe, so the count stayed 1,
//     while the warning fired once per resolved alert.
//
// So the guard asserts three things about the single call site: it sits directly in
// caller's own body and not in a nested function literal; its result is bound to a
// real variable (not `_`); and that variable reaches a `.Warn(...)` call in the same
// statement. Go's unused-variable rule covers the rest — `msg := f()` with msg unused
// does not compile — so binding plus a Warn is the whole contract.
//
// KNOWN, ACCEPTED FALSE POSITIVE: extracting the call into a helper that RunServe
// then calls once fails here, even though it is behaviour-preserving. That is the
// deliberate trade — a pin that costs an occasional refactor an explicit update is
// worth far more than one that green-lights a silent regression. Widen `caller` if
// the helper is genuinely the better structure.
//
// `lore investigate` is deliberately NOT a caller: that one-shot CLI never wires a
// ledger into Recall at all (see wireRecall — only the serve path calls it), so
// outcome decay is off there by construction rather than by misconfiguration, and
// warning about it would be noise. `lore curate` already has its own ledger startup
// report (LogLedgerStartup).
func TestRecallDecayWarningIsEmittedOnceAtStartup(t *testing.T) {
	const guarded, caller, raiser = "RecallDecayWarning", "RunServe", "Warn"

	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	callerCalls := 0

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // test-owned path in this package
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), guarded) {
			continue
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == guarded {
				continue
			}
			// stack tracks the ancestry of the node being visited, so a call site can be
			// judged by WHERE it sits, not merely by which function declares it.
			var stack []ast.Node
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return false
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != guarded {
					return true
				}
				if fn.Name.Name != caller {
					t.Errorf("%s: %s calls %s — it must be raised on the serve startup path (%s), "+
						"not from a helper or any other function",
						filepath.Base(path), fn.Name.Name, guarded, caller)
					return true
				}
				callerCalls++
				// Lexically inside RunServe is not the same as running at startup: a
				// FuncLit here is a closure RunServe hands to the incident path.
				for _, anc := range stack[:len(stack)-1] {
					if _, isLit := anc.(*ast.FuncLit); isLit {
						t.Errorf("%s: %s calls %s inside a function literal — that runs whenever the "+
							"closure runs (per alert / per resolve), not once at startup",
							filepath.Base(path), caller, guarded)
						return true
					}
				}
				assertWarningIsRaised(t, filepath.Base(path), outermostStmt(stack), guarded, raiser)
				return true
			})
		}
	}

	if callerCalls != 1 {
		t.Fatalf("%s must call %s exactly once (got %d) — otherwise the startup warning "+
			"is either absent or duplicated", caller, guarded, callerCalls)
	}
}

// outermostStmt returns the top-level statement of the enclosing function body that
// contains the visited node — stack[0] is the *ast.BlockStmt body itself, so the next
// statement down is the one whose whole subtree (an `if` with its init AND its body)
// must be inspected together. Nil if the stack holds no statement.
func outermostStmt(stack []ast.Node) ast.Stmt {
	for _, n := range stack[1:] {
		if s, ok := n.(ast.Stmt); ok {
			return s
		}
	}
	return nil
}

// assertWarningIsRaised checks the guarded call's result is bound to a real variable
// and that the variable is passed to a `.Warn(...)` call within the same statement —
// i.e. the warning is emitted, not computed and dropped. Go rejects an unused local,
// so `_ = f()` and a bare `f()` are the only ways to discard the result, and both are
// caught here.
func assertWarningIsRaised(t *testing.T, file string, stmt ast.Stmt, guarded, raiser string) {
	t.Helper()
	if stmt == nil {
		t.Errorf("%s: could not locate the statement calling %s", file, guarded)
		return
	}
	bound := ""
	ast.Inspect(stmt, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			call, isCall := rhs.(*ast.CallExpr)
			if !isCall {
				continue
			}
			if id, isIdent := call.Fun.(*ast.Ident); !isIdent || id.Name != guarded {
				continue
			}
			if i < len(as.Lhs) {
				if lhs, isIdent := as.Lhs[i].(*ast.Ident); isIdent && lhs.Name != "_" {
					bound = lhs.Name
				}
			}
		}
		return true
	})
	if bound == "" {
		t.Errorf("%s: the result of %s is discarded (assigned to `_`, or called as a bare "+
			"statement) — it must be bound and logged, or the warning is computed and never printed",
			file, guarded)
		return
	}
	raised := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != raiser {
			return true
		}
		for _, arg := range call.Args {
			if id, isIdent := arg.(*ast.Ident); isIdent && id.Name == bound {
				raised = true
			}
		}
		return true
	})
	if !raised {
		t.Errorf("%s: %s is bound to %q but never passed to a .%s(...) call in the same "+
			"statement — the warning is computed and never raised",
			file, guarded, bound, raiser)
	}
}
