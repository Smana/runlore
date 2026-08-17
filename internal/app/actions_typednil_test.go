// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestActionsInterfaceFieldsAreNeverAssignedRawBuilderResults pins the whole
// CLASS of bug that shipped as `acts.Threads = BuildThreadMention(...)`.
//
// server.Actions is the rung-2/rung-3 wiring bundle, and three of its fields —
// Pauser, Feedback, Threads — are INTERFACES. The functions in this package that
// fill them return CONCRETE POINTERS and return nil to mean "not configured".
// Assigning such a call straight into an interface field stores a TYPED NIL,
// which is a NON-NIL interface value, so every `if x == nil` guard downstream in
// internal/server silently stops working. For Threads that meant /slack/events
// answered 401 instead of 404 to an operator's pre-flight probe, then acked a
// real signed app_mention 200 and lost the human's note to a nil-receiver panic
// recovered inside a detached goroutine. Nothing in the suite went red.
//
// internal/server now normalises a typed-nil Threads in New (liveThreadHandler),
// so that ONE field is safe no matter what this package hands it. This guard is
// the other half, and the more general one: it keeps the call sites honest for
// every interface field of Actions, including the ones New does not normalise
// and the ones that do not exist yet. Both matter, because the two failures are
// different — New protects the server from its callers; this protects the next
// field somebody adds, where nobody will remember to write a normaliser.
//
// It is a static guard for the reason TestRunServeComposesTheChatLayer gives:
// RunServe is not callable from a test — it loads a config file, dials
// Kubernetes, elects a leader and blocks in ListenAndServe — so the only way to
// see what it assigns is to read it.
//
// The field set is read from internal/server's OWN declarations rather than
// hard-coded, so adding an interface field to Actions extends this guard
// automatically, and turning one into a concrete type retires it automatically.
func TestActionsInterfaceFieldsAreNeverAssignedRawBuilderResults(t *testing.T) {
	fields := actionsInterfaceFields(t, filepath.Join("..", "server"), "Actions")

	// Sanity: an empty or Threads-less field set means the reader silently
	// stopped matching (a renamed struct, a moved package) and the guard below
	// would pass forever having checked nothing.
	if !fields["Threads"] {
		t.Fatalf("did not find Threads among server.Actions' interface fields (found %v) — "+
			"this guard has gone inert", sortedKeys(fields))
	}

	violations, sites := typedNilViolations("server.Actions", fields, funcDecls(t, "."))
	if sites == 0 {
		t.Fatal("found no server.Actions value anywhere in internal/app — the wiring moved and " +
			"this guard is now checking nothing")
	}
	for _, v := range violations {
		t.Error(v)
	}
}

// TestTypedNilGuardCatchesTheShapesItClaimsTo is the guard's own mutation test.
//
// typedNilViolations reports rather than asserts precisely so the shapes it
// exists to catch can be fed to it here as source. A checker that silently
// matched nothing — a renamed builder, an assignment form it does not parse —
// would pass the test above forever, which is the failure mode this whole file
// is about. The intact case must report NOTHING, so the mutations below are
// known to fail for the reason claimed and not because the fixture never parsed.
func TestTypedNilGuardCatchesTheShapesItClaimsTo(t *testing.T) {
	fields := map[string]bool{"Threads": true, "Pauser": true}

	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "the shipped fix reports nothing",
			body: `if m := BuildThreadMention(cfg); m != nil {
					acts.Threads = m
				}`,
		},
		{
			name: "a nil-safe Enabled() predicate counts as a guard",
			body: `if ledger.Enabled() {
					acts.Pauser = ledger
				}`,
		},
		{
			name: "the bug as it shipped: a concrete builder assigned raw",
			body: `acts.Threads = BuildThreadMention(cfg)`,
			want: "acts.Threads",
		},
		{
			name: "the same bug inside a composite literal",
			body: `acts = server.Actions{Threads: BuildThreadMention(cfg)}`,
			want: "Threads",
		},
		{
			name: "a variable holding a concrete pointer, never nil-checked",
			body: `m := BuildThreadMention(cfg)
				acts.Threads = m`,
			want: "acts.Threads",
		},
		{
			name: "nil-checking the WRONG variable does not count",
			body: `m := BuildThreadMention(cfg)
				if other != nil {
					acts.Threads = m
				}`,
			want: "acts.Threads",
		},
		{
			name: "the else branch of a nil check is not guarded by it",
			body: `if m != nil {
					_ = m
				} else {
					acts.Threads = m
				}`,
			want: "acts.Threads",
		},
		{
			name: "a builder that returns the INTERFACE is safe to assign raw",
			body: `acts.Threads = BuildSafeHandler(cfg)`,
		},
		{
			name: "explicit nil is always safe",
			body: `acts.Threads = nil`,
		},
		{
			name: "a fresh composite literal can never be nil",
			body: `acts.Threads = &thread.Mention{}`,
		},
		{
			name: "a concrete field left concrete is not this guard's business",
			body: `acts.Approvals = BuildThreadMention(cfg)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(`package x

func BuildThreadMention(cfg *config.Config) *thread.Mention { return nil }
func BuildSafeHandler(cfg *config.Config) server.ThreadHandler { return nil }

func RunServe() {
	acts := server.Actions{}
	%s
	_ = acts
}
`, tc.body)
			got, sites := typedNilViolations("server.Actions", fields, parseFuncDecls(t, src))
			if sites == 0 {
				t.Fatal("the fixture bound no server.Actions value — this case proves nothing")
			}
			joined := strings.Join(got, "\n")
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no violations, got:\n%s", joined)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("want a violation naming %q, got none — the checker does not see this shape", tc.want)
			}
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("want a violation naming %q, got:\n%s", tc.want, joined)
			}
		})
	}
}

// typedNilViolations reports every assignment, in decls, of a value that may be
// a typed-nil concrete pointer into one of the named interface fields.
//
// An assignment is accepted only when the value cannot smuggle a typed nil:
//
//   - the untyped nil literal, or a composite literal (`&T{}`), which is never nil;
//   - the result of a SAME-PACKAGE function whose declared result is not a
//     pointer — i.e. it returns the interface itself, so its own `return nil` is
//     a true nil interface;
//   - an expression that an enclosing `if` condition inspects, which is how this
//     package already writes it (`if auto != nil`, `if ledger.Enabled()`).
//
// Everything else is reported, including a call to a function this checker
// cannot resolve. That is a KNOWN, ACCEPTED FALSE POSITIVE and the same trade
// TestRunServeComposesTheChatLayer makes: the fix is to assign through a
// nil-checked variable, which is what the codebase does everywhere else and is
// never wrong. A guard that let unresolvable calls through would have let the
// original bug through too, since BuildThreadMention lives in another file.
// The struct is matched BY TYPE, not by field name: internal/notify's Deps also
// has a field called Threads, and it is an interface assigned an interface, so a
// name-only match reports it and the guard starts crying wolf on correct code.
// sites reports how many values of that struct the checker actually found, so
// the caller can fail loudly if the answer is zero rather than pass on nothing.
func typedNilViolations(structName string, ifaceFields map[string]bool, decls map[string]*ast.FuncDecl) (violations []string, sites int) {
	// Result shapes of every function declared in this package, so a builder that
	// already returns an interface can be told from one returning *T.
	returnsPointer := map[string]bool{}
	for name, fn := range decls {
		if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
			continue
		}
		_, isPtr := fn.Type.Results.List[0].Type.(*ast.StarExpr)
		returnsPointer[name] = isPtr
	}

	var out []string
	for _, name := range sortedKeys(decls) {
		fn := decls[name]
		if fn.Body == nil {
			continue
		}
		c := &typedNilChecker{
			structName:     structName,
			fields:         ifaceFields,
			returnsPointer: returnsPointer,
			fn:             name,
			bound:          map[string]bool{},
		}
		c.bind(fn.Body)
		c.block(fn.Body.List, nil)
		out = append(out, c.found...)
		sites += c.sites
	}
	sort.Strings(out)
	return out, sites
}

type typedNilChecker struct {
	structName     string
	fields         map[string]bool
	returnsPointer map[string]bool
	fn             string
	bound          map[string]bool // idents holding a value of structName
	sites          int
	found          []string
}

// isStruct reports whether a type expression names the struct being guarded.
// Both the qualified and the bare spelling count, so the guard keeps working if
// the wiring ever moves into internal/server itself.
func (c *typedNilChecker) isStruct(e ast.Expr) bool {
	s := types.ExprString(e)
	return s == c.structName || s == strings.TrimPrefix(c.structName, packageOf(c.structName))
}

func packageOf(qualified string) string {
	if i := strings.Index(qualified, "."); i >= 0 {
		return qualified[:i+1]
	}
	return ""
}

// bind finds the identifiers that hold a value of the guarded struct, so
// `acts.Threads = …` can be told from any other `.Threads =` in the package.
func (c *typedNilChecker) bind(body *ast.BlockStmt) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				lit, ok := rhs.(*ast.CompositeLit)
				if !ok || !c.isStruct(lit.Type) || i >= len(s.Lhs) {
					continue
				}
				if id, ok := s.Lhs[i].(*ast.Ident); ok {
					c.bound[id.Name] = true
				}
			}
		case *ast.ValueSpec:
			if s.Type == nil || !c.isStruct(s.Type) {
				return true
			}
			for _, id := range s.Names {
				c.bound[id.Name] = true
			}
		}
		return true
	})
}

// block walks statements carrying guarded: the set of expressions that some
// enclosing `if` condition inspects, and which are therefore known to have been
// checked before this statement runs.
func (c *typedNilChecker) block(stmts []ast.Stmt, guarded map[string]bool) {
	for _, st := range stmts {
		c.stmt(st, guarded)
	}
}

func (c *typedNilChecker) stmt(st ast.Stmt, guarded map[string]bool) {
	switch s := st.(type) {
	case *ast.IfStmt:
		if s.Init != nil {
			c.stmt(s.Init, guarded)
		}
		// The THEN branch may rely on the condition; the ELSE branch may not —
		// inside `else` a `!= nil` check has established the opposite.
		c.block(s.Body.List, union(guarded, condRefs(s.Cond)))
		if s.Else != nil {
			c.stmt(s.Else, guarded)
		}
	case *ast.BlockStmt:
		c.block(s.List, guarded)
	case *ast.AssignStmt:
		c.assign(s, guarded)
	case *ast.ForStmt:
		c.block(s.Body.List, guarded)
	case *ast.RangeStmt:
		c.block(s.Body.List, guarded)
	case *ast.SwitchStmt:
		c.block(s.Body.List, guarded)
	case *ast.TypeSwitchStmt:
		c.block(s.Body.List, guarded)
	case *ast.SelectStmt:
		c.block(s.Body.List, guarded)
	case *ast.CaseClause:
		c.block(s.Body, guarded)
	case *ast.CommClause:
		c.block(s.Body, guarded)
	case *ast.LabeledStmt:
		c.stmt(s.Stmt, guarded)
	case *ast.ExprStmt:
		c.literals(s.X, guarded)
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			c.literals(r, guarded)
		}
	case *ast.DeclStmt:
		// A `var x = T{Field: Build()}` is the composite-literal shape again.
		ast.Inspect(s, func(n ast.Node) bool { c.literalNode(n, guarded); return true })
	}
}

// assign handles both `acts.Field = v` and any composite literal on either side.
func (c *typedNilChecker) assign(s *ast.AssignStmt, guarded map[string]bool) {
	for _, rhs := range s.Rhs {
		c.literals(rhs, guarded)
	}
	if len(s.Lhs) != len(s.Rhs) {
		return // multi-value call; nothing to attribute per field
	}
	for i, lhs := range s.Lhs {
		sel, ok := lhs.(*ast.SelectorExpr)
		if !ok || !c.fields[sel.Sel.Name] {
			continue
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || !c.bound[recv.Name] {
			continue // a same-named field on some other struct
		}
		if reason := c.unsafe(s.Rhs[i], guarded); reason != "" {
			c.found = append(c.found, fmt.Sprintf(
				"%s: %s = %s: %s. Assign through a nil-checked variable "+
					"(`if v := Build(...); v != nil { %s = v }`): %s is an interface, so a nil concrete "+
					"pointer stored in it is NOT nil and every downstream nil check silently stops working",
				c.fn, types.ExprString(lhs), types.ExprString(s.Rhs[i]), reason,
				types.ExprString(lhs), sel.Sel.Name))
		}
	}
}

// literals finds composite literals nested anywhere in e and checks their
// keyed fields — `server.Actions{Threads: Build(...)}` is the same bug written
// a different way.
func (c *typedNilChecker) literals(e ast.Expr, guarded map[string]bool) {
	ast.Inspect(e, func(n ast.Node) bool { c.literalNode(n, guarded); return true })
}

func (c *typedNilChecker) literalNode(n ast.Node, guarded map[string]bool) {
	lit, ok := n.(*ast.CompositeLit)
	if !ok || !c.isStruct(lit.Type) {
		return
	}
	c.sites++
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || !c.fields[key.Name] {
			continue
		}
		if reason := c.unsafe(kv.Value, guarded); reason != "" {
			c.found = append(c.found, fmt.Sprintf(
				"%s: %s{%s: %s}: %s. Build the value into a nil-checked variable first: "+
					"%s is an interface, so a nil concrete pointer stored in it is NOT nil",
				c.fn, types.ExprString(lit.Type), key.Name, types.ExprString(kv.Value),
				reason, key.Name))
		}
	}
}

// unsafe reports why v may smuggle a typed nil into an interface field, or ""
// when it cannot.
func (c *typedNilChecker) unsafe(v ast.Expr, guarded map[string]bool) string {
	switch e := v.(type) {
	case *ast.Ident:
		if e.Name == "nil" {
			return ""
		}
	case *ast.CompositeLit:
		return "" // T{...} is never nil
	case *ast.UnaryExpr:
		if _, ok := e.X.(*ast.CompositeLit); ok {
			return "" // &T{...} is never nil
		}
	case *ast.CallExpr:
		callee := types.ExprString(e.Fun)
		ptr, known := c.returnsPointer[callee]
		if known && !ptr {
			return "" // returns the interface itself, so its `return nil` is a true nil
		}
		if known {
			return fmt.Sprintf("%s returns a concrete pointer and is assigned raw", callee)
		}
		return fmt.Sprintf("%s is called inline and its result type cannot be resolved here", callee)
	}
	if guarded[types.ExprString(v)] {
		return ""
	}
	return fmt.Sprintf("%s is not nil-checked by any enclosing condition", types.ExprString(v))
}

// condRefs collects every identifier and field selection an `if` condition
// mentions, so `if auto != nil`, `if ledger.Enabled()` and
// `if m := Build(); m != nil` all register the value they inspect.
func condRefs(cond ast.Expr) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(cond, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.Ident:
			out[e.Name] = true
		case *ast.SelectorExpr:
			out[types.ExprString(e)] = true
			// Keep descending: `ledger.Enabled()` must also register `ledger`.
		}
		return true
	})
	return out
}

func union(a, b map[string]bool) map[string]bool {
	out := make(map[string]bool, len(a)+len(b))
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

// actionsInterfaceFields returns the names of struct's fields whose declared
// type is an interface, read from the SHIPPED sources of the package at dir. A
// named type counts when that same package declares it as an interface; `any`
// and an inline `interface{...}` count directly. Anything else — a pointer, a
// slice, a string — cannot hold a typed nil and is not this guard's business.
func actionsInterfaceFields(t *testing.T, dir, structName string) map[string]bool {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	ifaceTypes := map[string]bool{}
	var target *ast.StructType
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			switch typ := ts.Type.(type) {
			case *ast.InterfaceType:
				ifaceTypes[ts.Name.Name] = true
			case *ast.StructType:
				if ts.Name.Name == structName {
					target = typ
				}
			}
			return true
		})
	}
	if target == nil {
		t.Fatalf("no struct %q found under %s — this guard has gone inert", structName, dir)
	}
	out := map[string]bool{}
	for _, f := range target.Fields.List {
		isIface := false
		switch typ := f.Type.(type) {
		case *ast.InterfaceType:
			isIface = true
		case *ast.Ident:
			isIface = ifaceTypes[typ.Name] || typ.Name == "any"
		}
		if !isIface {
			continue
		}
		for _, n := range f.Names {
			out[n.Name] = true
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
