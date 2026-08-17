// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestActionsInterfaceFieldsAreNeverAssignedRawBuilderResults pins the whole
// CLASS of bug that shipped as `acts.Threads = BuildThreadMention(...)`.
//
// server.Actions is the rung-2/rung-3 wiring bundle, and its optional fields —
// Pauser, Feedback, Threads — are INTERFACES. The functions in this package that
// fill them return CONCRETE POINTERS and return nil to mean "not configured":
// BuildThreadMention returns *thread.Mention, BuildAuto returns *action.Auto.
// Assigning such a call straight into an interface field stores a TYPED NIL,
// which is a NON-NIL interface value, so every `if x == nil` guard downstream in
// internal/server silently stops working. For Threads that meant /slack/events
// answered 401 instead of 404 to an operator's pre-flight probe, then acked a
// real signed app_mention 200 and lost the human's note to a nil-receiver panic
// recovered inside a detached goroutine. Nothing in the suite went red.
//
// internal/server now normalises all three fields in New (liveIface), so the
// server is correct whatever this package hands it. This guard is the other
// half, and it is the one that scales: it keeps the call sites honest for every
// interface field of Actions, including fields added later, for which nobody
// will remember to extend the normaliser.
//
// It is a static guard for the reason TestRunServeComposesTheChatLayer gives:
// RunServe is not callable from a test — it loads a config file, dials
// Kubernetes, elects a leader and blocks in ListenAndServe — so the only way to
// see what it assigns is to read it.
//
// The field set is read from internal/server's OWN declarations, resolving
// cross-package interface names against their real sources, so adding an
// interface field to Actions extends this guard automatically and turning one
// into a concrete type retires it automatically.
func TestActionsInterfaceFieldsAreNeverAssignedRawBuilderResults(t *testing.T) {
	fields, unresolved := actionsInterfaceFields(t, filepath.Join("..", "server"), "Actions")

	// A field type this reader could not classify is treated as an interface
	// (fail closed) but must still be reported: silently guessing is how a guard
	// starts lying about its own coverage.
	for _, u := range unresolved {
		t.Logf("NOTE: %s — treated as an interface (fail closed); "+
			"teach actionsInterfaceFields to resolve it if this is noisy", u)
	}

	// Sanity: a field set missing any of the three known interface fields means
	// the reader silently stopped matching (a renamed struct, a moved package)
	// and the guard below would pass forever having checked nothing.
	if !fields["Threads"] || !fields["Pauser"] || !fields["Feedback"] {
		t.Fatalf("expected Threads, Pauser and Feedback among server.Actions' interface fields, got %v — "+
			"this guard has gone inert", sortedKeys(fields))
	}

	violations, sites := typedNilViolations("server.Actions", fields, parsePackage(t, "."))
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
// is about. The intact cases must report NOTHING, so the mutations are known to
// fail for the reason claimed and not because the fixture never parsed.
//
// The INDIRECTION cases below are not hypothetical. An earlier version of this
// guard walked only top-level function bodies and bound only local variables,
// and every one of them reintroduced the exact shipped bug while the guard
// stayed green. A guard is only worth its runtime if it survives the refactor
// that moves the assignment somewhere else.
func TestTypedNilGuardCatchesTheShapesItClaimsTo(t *testing.T) {
	fields := map[string]bool{"Threads": true, "Pauser": true}

	for _, tc := range []struct {
		name, src, want string
	}{
		// ---- correct code: must report nothing ----
		{
			name: "the shipped fix reports nothing",
			src: wrapActions(`if m := BuildThreadMention(cfg); m != nil {
					acts.Threads = m
				}`),
		},
		{
			name: "a nil-safe Enabled() predicate counts as a guard",
			src:  wrapActions(`if ledger.Enabled() { acts.Pauser = ledger }`),
		},
		{
			name: "the idiomatic early return guards everything after it",
			src: wrapActions(`m := BuildThreadMention(cfg)
				if m == nil {
					return
				}
				acts.Threads = m`),
		},
		{
			name: "an early exit via panic guards too",
			src: wrapActions(`m := BuildThreadMention(cfg)
				if m == nil {
					panic("unreachable")
				}
				acts.Threads = m`),
		},
		{
			name: "a value from an interface-returning builder is safe through a variable",
			src: wrapActions(`h := BuildSafeHandler(cfg)
				acts.Threads = h`),
		},
		{
			name: "a builder that returns the INTERFACE is safe to assign raw",
			src:  wrapActions(`acts.Threads = BuildSafeHandler(cfg)`),
		},
		{
			name: "an untouched zero-value interface local is safe",
			src: wrapActions(`var h server.ThreadHandler
				acts.Threads = h`),
		},
		{name: "explicit nil is always safe", src: wrapActions(`acts.Threads = nil`)},
		{
			name: "a fresh composite literal can never be nil",
			src:  wrapActions(`acts.Threads = &thread.Mention{}`),
		},
		{
			name: "a concrete field left concrete is not this guard's business",
			src:  wrapActions(`acts.Approvals = BuildThreadMention(cfg)`),
		},

		// ---- the bug, written every way it can be written ----
		{
			name: "the bug as it shipped: a concrete builder assigned raw",
			src:  wrapActions(`acts.Threads = BuildThreadMention(cfg)`),
			want: "acts.Threads",
		},
		{
			name: "the same bug inside a composite literal",
			src:  wrapActions(`acts = server.Actions{Threads: BuildThreadMention(cfg)}`),
			want: "Threads",
		},
		{
			name: "a variable holding a concrete pointer, never nil-checked",
			src: wrapActions(`m := BuildThreadMention(cfg)
				acts.Threads = m`),
			want: "acts.Threads",
		},
		{
			name: "nil-checking the WRONG variable does not count",
			src: wrapActions(`m := BuildThreadMention(cfg)
				if other != nil { acts.Threads = m }`),
			want: "acts.Threads",
		},
		{
			name: "an == nil check that does NOT exit guards nothing",
			src: wrapActions(`m := BuildThreadMention(cfg)
				if m == nil { acts.Threads = m }`),
			want: "acts.Threads",
		},
		{
			name: "the else branch of a nil check is not guarded by it",
			src: wrapActions(`if m != nil {
					_ = m
				} else {
					acts.Threads = m
				}`),
			want: "acts.Threads",
		},
		{
			name: "inside a switch case",
			src: wrapActions(`switch mode {
				case "a":
					acts.Threads = BuildThreadMention(cfg)
				}`),
			want: "acts.Threads",
		},

		// ---- the indirections an earlier version of this guard missed ----
		{
			name: "a helper taking *server.Actions",
			src: `package x

func BuildThreadMention(cfg *config.Config) *thread.Mention { return nil }

func RunServe() {
	acts := server.Actions{}
	wireThreads(&acts, cfg)
	_ = acts
}

func wireThreads(acts *server.Actions, cfg *config.Config) {
	acts.Threads = BuildThreadMention(cfg)
}
`,
			want: "acts.Threads",
		},
		{
			name: "a helper taking server.Actions by value and returning it",
			src: `package x

func BuildThreadMention(cfg *config.Config) *thread.Mention { return nil }

func wireThreads(a server.Actions, cfg *config.Config) server.Actions {
	a.Threads = BuildThreadMention(cfg)
	return a
}
`,
			want: "a.Threads",
		},
		{
			name: "a pointer alias to a local",
			src: wrapActions(`p := &acts
				p.Threads = BuildThreadMention(cfg)`),
			want: "p.Threads",
		},
		{
			name: "inside a closure",
			src: wrapActions(`fn := func() {
					acts.Threads = BuildThreadMention(cfg)
				}
				fn()`),
			want: "acts.Threads",
		},
		{
			name: "inside a go statement",
			src: wrapActions(`go func() {
					acts.Threads = BuildThreadMention(cfg)
				}()`),
			want: "acts.Threads",
		},
		{
			name: "inside a defer statement",
			src: wrapActions(`defer func() {
					acts.Threads = BuildThreadMention(cfg)
				}()`),
			want: "acts.Threads",
		},
		{
			name: "inside a method",
			src: `package x

func BuildThreadMention(cfg *config.Config) *thread.Mention { return nil }

type wiring struct{ acts server.Actions }

func (w *wiring) wire(cfg *config.Config) {
	w.acts.Threads = BuildThreadMention(cfg)
}
`,
			want: "w.acts.Threads",
		},
		{
			name: "a selector receiver on a struct field",
			src: `package x

func BuildThreadMention(cfg *config.Config) *thread.Mention { return nil }

type wiring struct{ acts *server.Actions }

func wire(w *wiring, cfg *config.Config) {
	w.acts.Threads = BuildThreadMention(cfg)
}
`,
			want: "w.acts.Threads",
		},
		{
			name: "multi-value assignment from a same-package function",
			src: `package x

func buildBoth(cfg *config.Config) (*thread.Mention, error) { return nil, nil }

func RunServe() {
	acts := server.Actions{}
	var err error
	acts.Threads, err = buildBoth(cfg)
	_, _ = acts, err
}
`,
			want: "acts.Threads",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, sites := typedNilViolations("server.Actions", fields, parseSource(t, tc.src))
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

// TestActionsInterfaceFieldsResolvesQualifiedTypes pins the promise the guard
// above makes about itself: that adding an interface field to server.Actions
// extends the guard automatically.
//
// An earlier version classified field types by SYNTAX alone and handled only a
// bare identifier or an inline `interface{…}`. Every interface named from
// ANOTHER package — the overwhelmingly likely spelling, e.g.
// `Curator curate.Pass` — was silently skipped, and nothing went red, because
// the only assertion was that Threads survived. A field wired from one of this
// package's many builders that already return nil for "disabled" would then have
// reintroduced the exact bug this file exists to close, with the guard green.
//
// The synthetic module is built on disk rather than mocked because the reader's
// whole job is to follow an import path to real sources: go.mod, package
// directories and all. It also pins the two answers that must NOT be yes — a
// qualified STRUCT is not an interface, and an unresolvable type is reported
// rather than silently guessed at.
func TestActionsInterfaceFieldsResolvesQualifiedTypes(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	write("go.mod", "module example.com/m\n\ngo 1.25\n")
	write("curate/curate.go", `package curate

// Pass is an interface, so a field of this type must be guarded.
type Pass interface{ Run() error }

// Alias chains must resolve too.
type Sink = Pass

// Settings is a struct: a field of this type cannot hold a typed nil.
type Settings struct{ Name string }
`)
	write("server/server.go", `package server

import (
	"example.com/m/curate"
	ext "golang.org/x/exp/nothing"
)

type Pauser interface{ Pause() }

type Actions struct {
	Local     Pauser
	Qualified curate.Pass
	Aliased   curate.Sink
	Inline    interface{ Do() }
	Anything  any
	Struct    curate.Settings
	Concrete  *curate.Settings
	Name      string
	IDs       []string
	External  ext.Thing
}
`)

	fields, unresolved := actionsInterfaceFields(t, filepath.Join(root, "server"), "Actions")

	for _, want := range []string{"Local", "Qualified", "Aliased", "Inline", "Anything"} {
		if !fields[want] {
			t.Errorf("field %q is an interface but was not detected; got %v — a field wired from a "+
				"nil-returning builder would slip past the guard", want, sortedKeys(fields))
		}
	}
	for _, notWant := range []string{"Struct", "Concrete", "Name", "IDs"} {
		if fields[notWant] {
			t.Errorf("field %q cannot hold a typed nil but was reported as an interface; got %v — "+
				"the guard will cry wolf on correct code", notWant, sortedKeys(fields))
		}
	}
	// Outside the module: no sources to read, so it must fail CLOSED and say so.
	if !fields["External"] {
		t.Error("an unresolvable field type must be treated as an interface (fail closed)")
	}
	if len(unresolved) != 1 || !strings.Contains(unresolved[0], "External") {
		t.Errorf("an unresolvable field type must be reported, got %v", unresolved)
	}
}

// wrapActions puts a statement body inside a RunServe that binds a
// server.Actions, for the mutation cases about the statement rather than the
// indirection.
func wrapActions(body string) string {
	return fmt.Sprintf(`package x

func BuildThreadMention(cfg *config.Config) *thread.Mention { return nil }
func BuildSafeHandler(cfg *config.Config) server.ThreadHandler { return nil }

func RunServe() {
	acts := server.Actions{}
	%s
	_ = acts
}
`, body)
}

// typedNilViolations reports every assignment, anywhere in files, of a value
// that may be a typed-nil concrete pointer into one of the named interface
// fields of structName.
//
// An assignment is accepted only when the value cannot smuggle a typed nil:
//
//   - the untyped nil literal, or a composite literal (`&T{}`), which is never nil;
//   - the result of a SAME-PACKAGE function whose declared result at that
//     position is not a pointer — i.e. it returns the interface itself, so its
//     own `return nil` is a true nil interface;
//   - a variable only ever assigned from such values;
//   - an expression an enclosing `if` condition proves non-nil, which is how this
//     package already writes it (`if auto != nil`, `if ledger.Enabled()`), or one
//     an earlier `if x == nil { return }` has already filtered out.
//
// Everything else is reported, including a call to a function this checker
// cannot resolve. That is a KNOWN, ACCEPTED FALSE POSITIVE and the same trade
// TestRunServeComposesTheChatLayer makes: the fix is to assign through a
// nil-checked variable, which is what the codebase does everywhere else and is
// never wrong. A guard that let unresolvable calls through would have let the
// original bug through too, since BuildThreadMention lives in another file.
//
// The struct is matched BY TYPE, not by field name: internal/notify's Deps also
// has a Threads field, and it is an interface assigned an interface, so a
// name-only match reports correct code and teaches the next author to delete the
// test. Receivers are found through local variables, POINTER ALIASES, function
// PARAMETERS and struct FIELDS of the guarded type, and bodies are walked
// through closures and methods as well as plain functions, because every one of
// those indirections is a refactor away from reintroducing the bug.
//
// sites reports how many values of that struct the checker actually found, so
// the caller can fail loudly if the answer is zero rather than pass on nothing.
func typedNilViolations(structName string, ifaceFields map[string]bool, files []*ast.File) (violations []string, sites int) {
	m := &typedNilModel{structName: structName, fields: ifaceFields}
	m.collect(files)

	var out []string
	for _, fn := range m.funcs {
		c := &typedNilChecker{model: m, fn: fn.name, bound: map[string]bool{}, safe: map[string]bool{}}
		c.seedParams(fn.decl)
		c.bindFixpoint(fn.decl.Body)
		c.block(fn.decl.Body.List, nil)
		out = append(out, c.found...)
		sites += c.sites
	}
	sort.Strings(out)
	return out, sites
}

// namedFunc is a function or method body to walk, with a display name that keeps
// methods distinct from same-named functions.
type namedFunc struct {
	name string
	decl *ast.FuncDecl
}

// typedNilModel is the package-wide knowledge the per-function checker needs.
type typedNilModel struct {
	structName string
	fields     map[string]bool
	funcs      []namedFunc
	// results maps a same-package function name to its declared result types, so
	// a call can be classified per result position (multi-value assignment
	// included). Methods are excluded: they are not callable by bare name, and
	// indexing them here would let a method shadow a function.
	results map[string][]ast.Expr
	// actsFields are struct field names in THIS package declared as the guarded
	// struct, so `w.acts.Threads = …` resolves through the field.
	actsFields map[string]bool
}

func (m *typedNilModel) collect(files []*ast.File) {
	m.results = map[string][]ast.Expr{}
	m.actsFields = map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				name = "(" + types.ExprString(fn.Recv.List[0].Type) + ")." + name
			} else if fn.Type.Results != nil {
				m.results[fn.Name.Name] = flattenFields(fn.Type.Results)
			}
			m.funcs = append(m.funcs, namedFunc{name: name, decl: fn})
		}
		// Struct fields of the guarded type, for selector receivers.
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if !m.isStruct(field.Type) {
					continue
				}
				for _, id := range field.Names {
					m.actsFields[id.Name] = true
				}
			}
			return true
		})
	}
	sort.Slice(m.funcs, func(i, j int) bool { return m.funcs[i].name < m.funcs[j].name })
}

// flattenFields expands a result list into one entry per returned value, so
// `(a, b *T, err error)` indexes the way an assignment does.
func flattenFields(l *ast.FieldList) []ast.Expr {
	var out []ast.Expr
	for _, field := range l.List {
		n := len(field.Names)
		if n == 0 {
			n = 1
		}
		for range n {
			out = append(out, field.Type)
		}
	}
	return out
}

// isStruct reports whether a type expression names the struct being guarded,
// through any number of pointers. Both the qualified and the bare spelling
// count, so the guard keeps working if the wiring ever moves into
// internal/server itself.
func (m *typedNilModel) isStruct(e ast.Expr) bool {
	for {
		star, ok := e.(*ast.StarExpr)
		if !ok {
			break
		}
		e = star.X
	}
	s := types.ExprString(e)
	if s == m.structName {
		return true
	}
	if i := strings.Index(m.structName, "."); i >= 0 {
		return s == m.structName[i+1:]
	}
	return false
}

type typedNilChecker struct {
	model *typedNilModel
	fn    string
	bound map[string]bool // expressions holding a value of the guarded struct
	safe  map[string]bool // expressions that cannot be a typed nil
	sites int
	found []string
}

// seedParams binds parameters and the receiver when they are declared as the
// guarded struct, which is what makes a `func wire(acts *server.Actions)` helper
// visible to the checker.
func (c *typedNilChecker) seedParams(fn *ast.FuncDecl) {
	for _, l := range []*ast.FieldList{fn.Type.Params, fn.Recv} {
		if l == nil {
			continue
		}
		for _, f := range l.List {
			if !c.model.isStruct(f.Type) {
				continue
			}
			for _, id := range f.Names {
				c.bindExpr(id.Name)
			}
		}
	}
}

// bindExpr records an expression as holding the guarded struct, counting it as a
// site only the first time so the fixpoint below cannot inflate the total.
func (c *typedNilChecker) bindExpr(expr string) {
	if c.bound[expr] {
		return
	}
	c.bound[expr] = true
	c.sites++
}

// bindFixpoint repeats the binding pass until it stops learning, so a chain like
// `acts := server.Actions{}` then `p := &acts` is followed regardless of the
// order the passes happen to see the statements in.
func (c *typedNilChecker) bindFixpoint(body *ast.BlockStmt) {
	for {
		before := len(c.bound) + len(c.safe)
		c.bind(body)
		if len(c.bound)+len(c.safe) == before {
			return
		}
	}
}

// bind finds the expressions that hold a value of the guarded struct — locals,
// pointer aliases and struct fields — plus the locals that provably cannot hold
// a typed nil.
func (c *typedNilChecker) bind(body *ast.BlockStmt) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if i >= len(s.Lhs) {
					break
				}
				lhs := types.ExprString(s.Lhs[i])
				if c.holdsStruct(rhs) {
					c.bindExpr(lhs)
				}
				// A local counts as safe only if EVERY assignment to it is safe.
				if c.unsafe(rhs, nil) == "" && !c.everUnsafe(body, lhs) {
					c.safe[lhs] = true
				}
			}
		case *ast.ValueSpec:
			for _, id := range s.Names {
				if s.Type != nil && c.model.isStruct(s.Type) {
					c.bindExpr(id.Name)
				}
				// `var h server.ThreadHandler` with no initialiser is a true nil
				// interface; a pointer type is not.
				if len(s.Values) == 0 && s.Type != nil {
					if _, isPtr := s.Type.(*ast.StarExpr); !isPtr && !c.everUnsafe(body, id.Name) {
						c.safe[id.Name] = true
					}
				}
			}
		case *ast.SelectorExpr:
			// w.acts / w.acts.Threads — resolve through a struct field of the type.
			if c.model.actsFields[s.Sel.Name] {
				c.bindExpr(types.ExprString(s))
			}
		}
		return true
	})
}

// everUnsafe reports whether name is assigned an unsafe value anywhere in body,
// which disqualifies it from the safe set however it was first bound.
func (c *typedNilChecker) everUnsafe(body *ast.BlockStmt, name string) bool {
	bad := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			if types.ExprString(lhs) != name || i >= len(as.Rhs) {
				continue
			}
			if c.unsafe(as.Rhs[i], nil) != "" {
				bad = true
			}
		}
		return true
	})
	return bad
}

// holdsStruct reports whether e evaluates to the guarded struct: a composite
// literal of it, or the address of something already bound.
func (c *typedNilChecker) holdsStruct(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.CompositeLit:
		return c.model.isStruct(v.Type)
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			return c.bound[types.ExprString(v.X)]
		}
	case *ast.Ident, *ast.SelectorExpr:
		return c.bound[types.ExprString(e)]
	}
	return false
}

// block walks statements carrying guarded: the set of expressions proven non-nil
// at this point, either by an enclosing condition or by an earlier
// `if x == nil { return }`.
func (c *typedNilChecker) block(stmts []ast.Stmt, guarded map[string]bool) {
	for _, st := range stmts {
		c.stmt(st, guarded)
		// The idiomatic Go early return: `if x == nil { return }` filters x out of
		// everything that follows it in this block.
		if ifs, ok := st.(*ast.IfStmt); ok && ifs.Else == nil && terminates(ifs.Body) {
			guarded = union(guarded, guardRefs(ifs.Cond, false))
		}
	}
}

func (c *typedNilChecker) stmt(st ast.Stmt, guarded map[string]bool) {
	// A function literal can appear in any expression position (`go func(){}()`,
	// `defer func(){}()`, `fn := func(){}`), and its body is a fresh scope that
	// the enclosing conditions do not dominate.
	ast.Inspect(st, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			c.block(lit.Body.List, nil)
		}
		return true
	})

	switch s := st.(type) {
	case *ast.IfStmt:
		if s.Init != nil {
			c.stmt(s.Init, guarded)
		}
		// The THEN branch may rely on the condition; the ELSE branch may not —
		// inside `else` a `!= nil` check has established the opposite.
		c.block(s.Body.List, union(guarded, guardRefs(s.Cond, true)))
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
		ast.Inspect(s, func(n ast.Node) bool { c.literalNode(n, guarded); return true })
	}
}

// assign handles `acts.Field = v`, its multi-value form, and any composite
// literal appearing on either side.
func (c *typedNilChecker) assign(s *ast.AssignStmt, guarded map[string]bool) {
	for _, rhs := range s.Rhs {
		c.literals(rhs, guarded)
	}
	for i, lhs := range s.Lhs {
		sel, ok := lhs.(*ast.SelectorExpr)
		if !ok || !c.model.fields[sel.Sel.Name] || !c.bound[types.ExprString(sel.X)] {
			continue // not a field of a value of the guarded struct
		}
		value, reason := c.rhsFor(s, i, guarded)
		if reason == "" {
			continue
		}
		c.found = append(c.found, fmt.Sprintf(
			"%s: %s = %s: %s. Assign through a nil-checked variable "+
				"(`if v := Build(...); v != nil { %s = v }`): %s is an interface, so a nil concrete "+
				"pointer stored in it is NOT nil and every downstream nil check silently stops working",
			c.fn, types.ExprString(lhs), value, reason, types.ExprString(lhs), sel.Sel.Name))
	}
}

// rhsFor resolves the value flowing into LHS position i, including the
// multi-value form `a.Field, err = f()` where one call feeds several targets.
func (c *typedNilChecker) rhsFor(s *ast.AssignStmt, i int, guarded map[string]bool) (value, reason string) {
	if len(s.Lhs) == len(s.Rhs) {
		return types.ExprString(s.Rhs[i]), c.unsafe(s.Rhs[i], guarded)
	}
	if len(s.Rhs) != 1 {
		return "", ""
	}
	call, ok := s.Rhs[0].(*ast.CallExpr)
	if !ok {
		return "", ""
	}
	value = types.ExprString(call)
	callee := types.ExprString(call.Fun)
	res, known := c.model.results[callee]
	if !known {
		return value, fmt.Sprintf("%s is called inline and its result types cannot be resolved here", callee)
	}
	if i >= len(res) {
		return "", ""
	}
	if _, isPtr := res[i].(*ast.StarExpr); !isPtr {
		return "", ""
	}
	return value, fmt.Sprintf("%s returns a concrete pointer in result %d and is assigned raw", callee, i)
}

// literals finds composite literals nested anywhere in e and checks their keyed
// fields — `server.Actions{Threads: Build(...)}` is the same bug written a
// different way.
func (c *typedNilChecker) literals(e ast.Expr, guarded map[string]bool) {
	if e == nil {
		return
	}
	ast.Inspect(e, func(n ast.Node) bool { c.literalNode(n, guarded); return true })
}

func (c *typedNilChecker) literalNode(n ast.Node, guarded map[string]bool) {
	lit, ok := n.(*ast.CompositeLit)
	if !ok || !c.model.isStruct(lit.Type) {
		return
	}
	c.sites++
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || !c.model.fields[key.Name] {
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
		res, known := c.model.results[callee]
		if !known {
			return fmt.Sprintf("%s is called inline and its result type cannot be resolved here", callee)
		}
		if len(res) == 0 {
			return ""
		}
		if _, isPtr := res[0].(*ast.StarExpr); !isPtr {
			return "" // returns the interface itself, so its `return nil` is a true nil
		}
		return fmt.Sprintf("%s returns a concrete pointer and is assigned raw", callee)
	}
	expr := types.ExprString(v)
	if guarded[expr] || c.safe[expr] {
		return ""
	}
	return fmt.Sprintf("%s is not nil-checked by any enclosing condition", expr)
}

// guardRefs extracts the expressions a condition proves NON-NIL.
//
// positive is for the THEN branch of `if cond { … }`: `x != nil` conjuncts prove
// x non-nil, and a predicate call proves whatever it inspects (the codebase's
// `if ledger.Enabled()` idiom). !positive is for the code AFTER a terminating
// `if cond { return }`: there it is the `x == nil` disjuncts that have been
// filtered out. An `x == nil` condition whose body does NOT terminate proves
// nothing, which is why the operator is read rather than every identifier in the
// condition being collected.
func guardRefs(cond ast.Expr, positive bool) map[string]bool {
	out := map[string]bool{}
	join, want := token.LAND, token.NEQ
	if !positive {
		join, want = token.LOR, token.EQL
	}
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch v := e.(type) {
		case *ast.ParenExpr:
			walk(v.X)
		case *ast.BinaryExpr:
			if v.Op == join {
				walk(v.X)
				walk(v.Y)
				return
			}
			if v.Op != want {
				return
			}
			if isNilIdent(v.Y) {
				out[types.ExprString(v.X)] = true
			}
			if isNilIdent(v.X) {
				out[types.ExprString(v.Y)] = true
			}
		case *ast.CallExpr:
			if !positive {
				return
			}
			// `ledger.Enabled()` / `Enabled(ledger)`: a nil-safe predicate stands in
			// for the nil check, which is how this package already writes it.
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				out[types.ExprString(sel.X)] = true
			}
			for _, a := range v.Args {
				out[types.ExprString(a)] = true
			}
		}
	}
	walk(cond)
	return out
}

func isNilIdent(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "nil"
}

// terminates reports whether a block always leaves the enclosing block, so an
// `if x == nil { … }` before it filters x out of everything that follows.
func terminates(b *ast.BlockStmt) bool {
	if len(b.List) == 0 {
		return false
	}
	switch last := b.List[len(b.List)-1].(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BranchStmt:
		return last.Tok != token.FALLTHROUGH
	case *ast.ExprStmt:
		call, ok := last.X.(*ast.CallExpr)
		if !ok {
			return false
		}
		fn := types.ExprString(call.Fun)
		return fn == "panic" || strings.HasSuffix(fn, ".Fatal") || strings.HasSuffix(fn, ".Fatalf")
	}
	return false
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
// type is an interface, read from the SHIPPED sources of the package at dir.
//
// Interface-ness is resolved for real, not guessed from the syntax: a name
// declared in the same package is looked up there (following alias chains), and
// a QUALIFIED name is resolved through that file's imports to the referenced
// package's own sources. That matters because the whole promise of this guard is
// that it extends itself — a field added as `Curator curate.ConfirmationSink`,
// wired from a builder that already returns nil for "disabled", is exactly the
// bug this guard exists to catch, and a syntax-only reader would skip it in
// silence while still claiming to cover every interface field.
//
// Anything still unresolvable (a type from outside this module) is treated as an
// interface — fail closed, consistent with how unresolvable CALLS are treated —
// and returned in unresolved so the caller can say so out loud rather than let
// the guess pass for knowledge.
func actionsInterfaceFields(t *testing.T, dir, structName string) (fields map[string]bool, unresolved []string) {
	t.Helper()
	pkg := parsePackage(t, dir)

	var target *ast.StructType
	var targetFile *ast.File
	for _, f := range pkg {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*ast.StructType); ok && ts.Name.Name == structName {
				target, targetFile = st, f
			}
			return true
		})
	}
	if target == nil {
		t.Fatalf("no struct %q found under %s — this guard has gone inert", structName, dir)
	}

	local := interfaceNames(pkg)
	imports := importPaths(targetFile)
	root := moduleRoot(dir)

	fields = map[string]bool{}
	for _, f := range target.Fields.List {
		isIface, why := false, ""
		switch typ := f.Type.(type) {
		case *ast.InterfaceType:
			isIface = true
		case *ast.Ident:
			isIface = local.iface[typ.Name] || typ.Name == "any"
			if !isIface && !local.declared[typ.Name] && !isBuiltinType(typ.Name) {
				isIface, why = true, typ.Name
			}
		case *ast.SelectorExpr:
			pkgName, ok := typ.X.(*ast.Ident)
			if !ok {
				isIface, why = true, types.ExprString(typ)
				break
			}
			var resolved bool
			isIface, resolved = crossPackageIsInterface(t, root, imports[pkgName.Name], typ.Sel.Name)
			if !resolved {
				isIface, why = true, types.ExprString(typ)
			}
		}
		if !isIface {
			continue
		}
		if len(f.Names) == 0 { // embedded interface: the type name IS the field name
			if id := embeddedName(f.Type); id != "" {
				fields[id] = true
			}
			continue
		}
		for _, n := range f.Names {
			fields[n.Name] = true
			if why != "" {
				unresolved = append(unresolved, fmt.Sprintf("%s.%s has type %s, which could not be resolved",
					structName, n.Name, why))
			}
		}
	}
	sort.Strings(unresolved)
	return fields, unresolved
}

type pkgTypes struct {
	iface    map[string]bool // named types whose underlying type is an interface
	declared map[string]bool // every named type declared here
}

// interfaceNames finds the interface types a package declares, following alias
// and named-type chains to a fixpoint so `type A = B` with B an interface counts.
func interfaceNames(pkg []*ast.File) pkgTypes {
	out := pkgTypes{iface: map[string]bool{}, declared: map[string]bool{}}
	aliases := map[string]string{}
	for _, f := range pkg {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			out.declared[ts.Name.Name] = true
			switch typ := ts.Type.(type) {
			case *ast.InterfaceType:
				out.iface[ts.Name.Name] = true
			case *ast.Ident:
				aliases[ts.Name.Name] = typ.Name
			}
			return true
		})
	}
	for {
		grew := false
		for name, target := range aliases {
			if out.iface[target] && !out.iface[name] {
				out.iface[name] = true
				grew = true
			}
		}
		if !grew {
			return out
		}
	}
}

// crossPackageIsInterface resolves pkgPath to its sources under the module root
// and reports whether it declares name as an interface. resolved is false when
// the package is outside this module, so there are no sources to read.
func crossPackageIsInterface(t *testing.T, root, pkgPath, name string) (isIface, resolved bool) {
	t.Helper()
	if root == "" || pkgPath == "" {
		return false, false
	}
	mod := modulePath(root)
	if mod == "" || !strings.HasPrefix(pkgPath, mod+"/") {
		return false, false // a dependency, not our source
	}
	dir := filepath.Join(root, strings.TrimPrefix(pkgPath, mod+"/"))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false, false
	}
	known := interfaceNames(parsePackage(t, dir))
	if !known.declared[name] {
		return false, false
	}
	return known.iface[name], true
}

// parsePackage parses the shipped (non-test) Go files of one directory. A guard
// that read test files could be satisfied by a test.
func parsePackage(t *testing.T, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatalf("no shipped Go files under %s — this guard is inert", dir)
	}
	return out
}

// parseSource is parsePackage over an in-memory file, for the guard's own
// mutation test.
func parseSource(t *testing.T, src string) []*ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	return []*ast.File{f}
}

func importPaths(f *ast.File) map[string]string {
	out := map[string]string{}
	if f == nil {
		return out
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

// moduleRoot walks up from dir to the directory holding go.mod.
func moduleRoot(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

func modulePath(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func embeddedName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	case *ast.StarExpr:
		return embeddedName(v.X)
	}
	return ""
}

func isBuiltinType(name string) bool {
	switch name {
	case "bool", "byte", "complex64", "complex128", "error", "float32", "float64",
		"int", "int8", "int16", "int32", "int64", "rune", "string",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return true
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
