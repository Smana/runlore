// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunServeComposesTheChatLayer pins the COMPOSITION, not the components.
//
// Every part of the chat layer already has a test that pins it in isolation:
// TestBuildThreadChatWiresEveryField proves buildThreadChat fills every field of
// thread.Chat (the shared catalog included), and
// TestBuildThreadResponderCarriesTheChatLayer proves buildThreadResponder puts
// the Chat it is handed on the one shared *thread.Responder. Neither says
// anything about the two calls RunServe actually makes, and that is where the
// feature is switched off in production:
//
//	threadChat := buildThreadChat(cfg, nil /* was: cat */, metrics, log)
//	threadResponder := buildThreadResponder(cfg, threadRegistry, forge, nil /* was: threadChat */, metrics, log)
//
// Either substitution builds the entire paid layer and drops it on the floor —
// Responder.freeform nil-checks Chat before every use and Chat.catalogHits
// nil-checks Catalog, so both degrade silently back to the PR2 deterministic
// path — and the whole internal/app suite stayed green through both. This is the
// fourth time this series has shipped something green and inert, so the last hop
// gets its own guard.
//
// The chain does not end at buildThreadResponder, so neither does this guard: the
// responder it returns is what RunServe hands to BuildThreadMention (Slack) and
// BuildMatrixFeedback (Matrix), and BOTH must receive that ONE value. A second
// responder built for one of them would silently double the two global ceilings
// that live on the responder — ForgeWrites, the per-hour cap on every forge write
// thread capture makes, and Chat's per-hour token Budget — since "one responder,
// two transports" is exactly what makes them global. nil in either slot instead
// switches that transport's capture off: BuildThreadMention warns about a missing
// forge and returns nil, buildMatrixThreadMention does the same and the listener
// keeps running with feedback reactions alone, so startup still looks healthy.
// internal/app's notify_test.go builds its own responder by hand and so cannot
// see any of this; only the call sites in RunServe can.
//
// The same argument covers the two hops the announcement path adds. The sink a
// knowledge-base announcement is delivered to is the process's ONE *notify.Multi
// — the same fan-out that carries investigation findings — and RunServe is the
// only place it meets the responder; buildThreadResponder's own test proves the
// announcer is built from whatever notifier it is handed, and says nothing about
// which value production hands it. nil there is silently "announcements off"
// with the operator's announce_kb_updates: true still in the file. And the
// announcer must be DRAINED at shutdown, after everything that can still hand it
// work: an announcement is scheduled on a bounded dispatcher, so one in flight
// when SIGTERM arrives is dropped unless something waits for it — for a write
// that already reached the forge. Drain existing, tested and called from nowhere
// is precisely how it shipped.
//
// It is a static guard because RunServe is not callable from a test: it loads a
// config file, dials Kubernetes, elects a leader and blocks in ListenAndServe.
// What that costs is a KNOWN, ACCEPTED FALSE POSITIVE, the same trade
// TestRecallDecayWarningIsEmittedOnceAtStartup makes: extracting the two calls
// into a helper fails here even though it is behaviour-preserving. Update the
// guard deliberately in that case — a pin that costs an occasional refactor an
// explicit edit is worth more than one that green-lights a silent regression.
//
// Nothing here is hard-coded positionally: the argument slots are resolved by
// PARAMETER NAME from the builders' own declarations, and the catalog's slot by
// RESULT TYPE from BuildInvestigator's, so reordering a signature moves the
// guard with it instead of quietly pointing it at the wrong argument.
func TestRunServeComposesTheChatLayer(t *testing.T) {
	for _, v := range threadWiringViolations(funcDecls(t, ".")) {
		t.Error(v)
	}
}

// TestThreadWiringGuardCatchesADroppedLayer is the guard's own mutation test.
//
// threadWiringViolations reports problems rather than asserting them, precisely
// so the mutations it exists to catch can be fed to it here as source. A checker
// that silently matched nothing — a renamed builder, an argument shape it does
// not understand — would pass the test above forever, which is the failure mode
// this whole file is about.
//
// Each case states only what it mutates; every other slot keeps its intact
// value, so a case reads as the one substitution it is about.
func TestThreadWiringGuardCatchesADroppedLayer(t *testing.T) {
	// The intact shape, minus every dependency: enough for the checker, and it
	// must report nothing so the mutations below are known to be what fails.
	// The %s slots are, in order: the statement that binds the investigation
	// queue, the catalog handed to buildThreadChat, the chat layer and the
	// notifier handed to buildThreadResponder, an extra statement (empty unless a
	// case builds a second responder), the responder handed to each transport,
	// and the shutdown drain sequence.
	const intact = `package x

func BuildInvestigator() (investigate.Investigator, *catalog.Catalog, *notify.Multi, error) { return nil, nil, nil, nil }
func buildThreadChat(cfg *config.Config, cat *catalog.Catalog) *thread.Chat { return nil }
func buildThreadResponder(cfg *config.Config, chat *thread.Chat, notifier *notify.Multi) *thread.Responder { return nil }
func BuildThreadMention(cfg *config.Config, responder *thread.Responder) *thread.Mention { return nil }
func BuildMatrixFeedback(cfg *config.Config, responder *thread.Responder, dispatch, busyDispatch *thread.Dispatcher) *notify.MatrixFeedback { return nil }

func RunServe() {
	inv, cat, notifier, err := BuildInvestigator()
	_, _, _ = inv, notifier, err
	%s
	threadChat := buildThreadChat(cfg, %s)
	threadResponder := buildThreadResponder(cfg, %s, %s)
	%s
	mfb := BuildMatrixFeedback(cfg, %s, matrixDispatch, matrixBusyDispatch)
	mention := BuildThreadMention(cfg, %s)
	srv := server.New(acts)
	go func() {
		%s
	}()
	_, _, _, _ = threadChat, threadResponder, mfb, mention
}
`
	// How production binds the investigation queue — the drain the announcer's
	// must come BEFORE, and the one the guard resolves its name from.
	const queueDecl = `queue := investigate.NewQueue(inv, log)`

	// The shutdown order production uses: the mention pools first (each can
	// still schedule an announcement while it drains), then the announcer, then
	// the investigation queue, which cannot.
	const drains = `srv.Drain(dctx)
		matrixDispatch.Drain(dctx)
		matrixBusyDispatch.Drain(dctx)
		threadResponder.Announcer.Drain(dctx)
		queue.Drain(dctx)`

	for _, tc := range []struct {
		name, queue, cat, chat, notifier, extra, matrix, slack, drains, want string
	}{
		{
			name: "intact wiring reports nothing",
		},
		{
			name: "the catalog is dropped",
			cat:  "nil",
			want: "buildThreadChat",
		},
		{
			name: "the chat layer is dropped",
			chat: "nil",
			want: "buildThreadResponder",
		},
		{
			name: "a second catalog is passed instead of the shared one",
			cat:  "otherCat",
			want: "buildThreadChat",
		},
		{
			// The break the last hop exists for: the shared responder still
			// reaches Matrix, but Slack gets one built just for it — its own
			// ForgeWrites window and its own chat Budget, both global ceilings
			// doubled, with every unit test still green.
			name:  "a second responder is built and handed to Slack's thread capture",
			extra: "slackResponder := buildThreadResponder(cfg, threadChat)",
			slack: "slackResponder",
			want:  "BuildThreadMention",
		},
		{
			name:  "the responder never reaches Slack's thread capture",
			slack: "nil",
			want:  "BuildThreadMention",
		},
		{
			name:   "the responder never reaches Matrix's thread capture",
			matrix: "nil",
			want:   "BuildMatrixFeedback",
		},
		{
			// notify.thread.announce_kb_updates: true in the operator's file,
			// no sink on the responder, and NewKBAnnouncer's nil-sink contract
			// turns that into "announcements are off" without a word.
			name:     "the notifier never reaches the responder, so nothing is ever announced",
			notifier: "nil",
			want:     "buildThreadResponder",
		},
		{
			// The Task 4 shape exactly: Drain written, tested, called nowhere.
			name:   "the announcer is never drained at shutdown",
			drains: "srv.Drain(dctx)\n\t\tmatrixDispatch.Drain(dctx)\n\t\tmatrixBusyDispatch.Drain(dctx)\n\t\tqueue.Drain(dctx)",
			want:   "Announcer",
		},
		{
			// Present but useless: the pools it drains ahead of are the ones
			// still scheduling announcements, so every announcement produced
			// during their drain is dropped — by a Drain call that is right
			// there in the diff.
			name:   "the announcer is drained before the handlers that can still schedule one",
			drains: "threadResponder.Announcer.Drain(dctx)\n\t\tsrv.Drain(dctx)\n\t\tmatrixDispatch.Drain(dctx)\n\t\tmatrixBusyDispatch.Drain(dctx)",
			want:   "Announcer",
		},
		{
			// The other end of the same window, and the half serve.go's own
			// drain-order comment calls out: all five drains share ONE deadline,
			// the investigation queue schedules no announcements and is the slow
			// one, so waiting on it first spends the grace period and hands the
			// announcer a context that has already expired. Every announcement
			// still in flight is then dropped — by a Drain called in the right
			// place, in the wrong order.
			name:   "the announcer is drained after the investigation queue, on the shared deadline",
			drains: "srv.Drain(dctx)\n\t\tmatrixDispatch.Drain(dctx)\n\t\tmatrixBusyDispatch.Drain(dctx)\n\t\tqueue.Drain(dctx)\n\t\tthreadResponder.Announcer.Drain(dctx)",
			want:   "queue",
		},
		{
			// The guard's own inertness, which is the failure this whole file
			// exists to catch: with the queue bound from something this checker
			// does not recognise it cannot compare the two positions at all, and
			// must say so rather than pass.
			name:  "the queue is bound from a constructor the guard cannot resolve",
			queue: "queue := investigate.NewWorkQueue(inv, log)",
			want:  "inert",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fmt.Sprintf(intact,
				cmp.Or(tc.queue, queueDecl),
				cmp.Or(tc.cat, "cat"),
				cmp.Or(tc.chat, "threadChat"),
				cmp.Or(tc.notifier, "notifier"),
				tc.extra,
				cmp.Or(tc.matrix, "threadResponder"),
				cmp.Or(tc.slack, "threadResponder"),
				cmp.Or(tc.drains, drains),
			)
			got := threadWiringViolations(parseFuncDecls(t, src))
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("intact wiring reported %v — the guard cannot distinguish a real break from noise", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("the checker waved through this source — it would wave the real mutation through too:\n%s", src)
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Fatalf("violation must name %s so the reader knows which hop broke; got %v", tc.want, got)
			}
		})
	}
}

// threadWiringViolations reports every way RunServe's composition of the chat
// layer can be silently wrong, as operator-readable messages; nil when the
// wiring is intact. It returns problems instead of asserting them so
// TestThreadWiringGuardCatchesADroppedLayer can drive it with synthetic source
// and prove it still detects them.
func threadWiringViolations(fns map[string]*ast.FuncDecl) []string {
	var bad []string
	need := func(name string) *ast.FuncDecl {
		fn := fns[name]
		if fn == nil {
			bad = append(bad, fmt.Sprintf("%s is not declared in this package: the thread wiring moved "+
				"and this guard is inert, which is the exact failure it exists to catch", name))
		}
		return fn
	}
	serve := need("RunServe")
	chatBuilder := need("buildThreadChat")
	respBuilder := need("buildThreadResponder")
	invBuilder := need("BuildInvestigator")
	slackBuilder := need("BuildThreadMention")
	matrixBuilder := need("BuildMatrixFeedback")
	if len(bad) > 0 {
		return bad
	}

	// The catalog is the process's ONE catalog — a second one would mean a second
	// git-sync goroutine on the same checkout — so the identifier RunServe hands
	// the chat layer must be the very one BuildInvestigator returned.
	catIdx := resultIndex(invBuilder, "*catalog.Catalog")
	if catIdx < 0 {
		return append(bad, "BuildInvestigator no longer returns a *catalog.Catalog: this guard can no longer "+
			"tell which identifier is the shared catalog")
	}
	catNames := boundNames(serve, "BuildInvestigator")
	if len(catNames) <= catIdx || catNames[catIdx] == "_" {
		return append(bad, "RunServe does not bind BuildInvestigator's *catalog.Catalog result: the chat layer "+
			"cannot be given the shared catalog it is required to use")
	}

	bad = append(bad, checkArg(serve, chatBuilder, "buildThreadChat", "cat", catNames[catIdx],
		"conversational replies are then answered with no knowledge-base context at all — Chat.catalogHits "+
			"nil-checks Catalog, so every reply silently loses the excerpts that justify it, and no test fails")...)

	chatNames := boundNames(serve, "buildThreadChat")
	if len(chatNames) != 1 || chatNames[0] == "_" {
		return append(bad, "RunServe does not bind buildThreadChat's result to a variable: the layer is built "+
			"and discarded, and model.chat is paid-for dead config")
	}
	bad = append(bad, checkArg(serve, respBuilder, "buildThreadResponder", "chat", chatNames[0],
		"the whole chat layer is built and dropped on the floor — Responder.freeform nil-checks Chat, so every "+
			"addressed question falls back to the PR2 deterministic reply while every unit test of Chat stays green")...)

	// The responder is the process's ONE responder — the global ForgeWrites cap
	// and the chat token Budget both live on it — so it must be bound exactly
	// once and that same identifier must reach BOTH transports below.
	respNames := boundNames(serve, "buildThreadResponder")
	if len(respNames) != 1 || respNames[0] == "_" {
		return append(bad, fmt.Sprintf("RunServe binds %v from buildThreadResponder, want exactly one named "+
			"variable: BuildThreadMention and BuildMatrixFeedback must both be handed that single value, because "+
			"the per-hour ForgeWrites cap and the chat token Budget live ON the responder — a second instance "+
			"doubles both global ceilings, and none of it fails a test", respNames))
	}
	bad = append(bad, checkArg(serve, slackBuilder, "BuildThreadMention", "responder", respNames[0],
		"Slack's thread capture no longer runs on the shared responder — nil switches the Slack capture path off "+
			"entirely (BuildThreadMention warns about a missing forge and returns nil), and a second responder "+
			"gives Slack its own ForgeWrites window and its own chat Budget on top of Matrix's")...)
	bad = append(bad, checkArg(serve, matrixBuilder, "BuildMatrixFeedback", "responder", respNames[0],
		"Matrix's thread capture no longer runs on the shared responder — nil makes buildMatrixThreadMention warn "+
			"and drop capture from the listener while feedback reactions keep it looking healthy, and a second "+
			"responder doubles the same two global ceilings")...)

	// The announcement sink is that same single *notify.Multi: announcing to
	// each notifier's own channel is the entire feature, and the fan-out is
	// where the configured notifiers live.
	notifierIdx := resultIndex(invBuilder, "*notify.Multi")
	if notifierIdx < 0 {
		return append(bad, "BuildInvestigator no longer returns a *notify.Multi: this guard can no longer tell "+
			"which identifier is the shared notifier the knowledge-base announcement is delivered through")
	}
	if len(catNames) <= notifierIdx || catNames[notifierIdx] == "_" {
		return append(bad, "RunServe does not bind BuildInvestigator's *notify.Multi result: a knowledge-base "+
			"write can then be announced to nobody, whatever notify.thread.announce_kb_updates says")
	}
	bad = append(bad, checkArg(serve, respBuilder, "buildThreadResponder", "notifier", catNames[notifierIdx],
		"notify.thread.announce_kb_updates is then on in the operator's config and off in the process — "+
			"buildKBAnnouncer builds no announcer without a sink, thread.Responder.Announcer is nil-safe, and "+
			"every landed knowledge write is announced to nobody with nothing logged")...)
	bad = append(bad, announcerDrainViolations(serve, matrixBuilder, respNames[0])...)
	return bad
}

// announcerDrainViolations reports whether RunServe drains the announcer at
// shutdown, and whether it does so in the one window where draining it is worth
// anything — after everything that can still schedule an announcement, and
// before the one drain that can eat the shared deadline.
//
// Three distinct failures, one silent each. Never draining it means an
// announcement still in flight when SIGTERM arrives is dropped, for a write that
// is already on the forge — the KBAnnouncer's dispatcher runs its deliveries on
// a context stripped of cancellation, so nothing else waits for them. Draining
// it too early means the same drop with a Drain call sitting in the diff: the
// Slack server and the Matrix mention pools are what RUN the handlers that
// schedule announcements, so anything they deliver while THEY drain arrives
// after an announcer drain that has already returned. Draining it too LATE is
// the same drop again: all five drains share ONE dctx deadline (see serve.go's
// drain-order comment), and the investigation queue — which schedules no
// announcements at all — is the slow one, so waiting on it first hands the
// announcer an already-expired context.
//
// Both ends are bounded by drains RunServe itself names, and every one of them
// is resolved from the source rather than spelled out here — see
// announcementSchedulers and announcementDeadlineSpenders. Either resolving to
// nothing is reported as an inert guard, because a checker that quietly stopped
// comparing is the failure this file exists to catch.
func announcerDrainViolations(serve, matrixBuilder *ast.FuncDecl, respName string) []string {
	want := respName + ".Announcer"
	drains := drainCalls(serve)
	at, ok := drains[want]
	if !ok {
		return []string{fmt.Sprintf("RunServe never calls %s.Drain: an announcement in flight at shutdown is "+
			"dropped for a knowledge write that already reached the forge. thread.KBAnnouncer.Drain exists and "+
			"is tested; a Drain nobody calls is the same as no Drain at all", want)}
	}
	var bad []string
	schedulers := announcementSchedulers(serve, matrixBuilder)
	if len(schedulers) == 0 {
		bad = append(bad, "this guard is inert about draining the announcer too EARLY: it can no longer identify "+
			"the handlers that schedule announcements (neither server.New's result nor BuildMatrixFeedback's "+
			"dispatch arguments resolved)")
	}
	for _, recv := range schedulers {
		pos, drained := drains[recv]
		if !drained || pos < at {
			continue
		}
		bad = append(bad, fmt.Sprintf("RunServe drains %s BEFORE %s: %s is still running the mention handlers "+
			"that schedule announcements, so every announcement produced while it drains is dropped — by a "+
			"Drain call that looks correct in review. Drain the announcer after it", want, recv, recv))
	}
	spenders := announcementDeadlineSpenders(serve)
	if len(spenders) == 0 {
		bad = append(bad, "this guard is inert about draining the announcer too LATE: nothing in RunServe is bound "+
			"from investigate.NewQueue, so it can no longer identify the drain that must not run first")
	}
	for _, recv := range spenders {
		pos, drained := drains[recv]
		if !drained || pos > at {
			continue
		}
		bad = append(bad, fmt.Sprintf("RunServe drains %s BEFORE %s: they share one dctx deadline and %s "+
			"schedules no announcements, so a slow investigation spends the whole grace period and the announcer "+
			"is then drained on an expired context — the same dropped announcement, from the other side. Drain "+
			"the announcer before it", recv, want, recv))
	}
	return bad
}

// announcementDeadlineSpenders returns the receiver expressions of the shutdown
// drains that must happen AFTER the announcer's: the investigation queue, whose
// in-flight work is the slowest thing shutdown waits on and the only one of the
// five that can hand nothing to the announcer.
//
// Resolved from the source like the schedulers are — by the identifier RunServe
// binds from investigate.NewQueue — so a rename moves the guard with it, and a
// shape it cannot resolve is reported as inert by the caller rather than
// passing quietly. A queue that is bound but never drained is not this check's
// business: there is then no order to get wrong.
func announcementDeadlineSpenders(serve *ast.FuncDecl) []string {
	var out []string
	for _, name := range boundNames(serve, "investigate.NewQueue") {
		if name != "_" {
			out = append(out, name)
		}
	}
	return out
}

// announcementSchedulers returns the receiver expressions of the shutdown drains
// that must happen BEFORE the announcer's: the Slack server (its own Drain waits
// on the detached mention handlers) and the two Matrix mention dispatchers.
//
// Both are resolved from the source, not spelled out: the server by the
// identifier RunServe binds from server.New, the dispatchers by the arguments it
// passes in BuildMatrixFeedback's own dispatch/busyDispatch parameter slots. A
// rename moves the guard with it, and a shape it cannot resolve is reported as
// an inert guard by the caller rather than passing quietly.
func announcementSchedulers(serve, matrixBuilder *ast.FuncDecl) []string {
	var out []string
	if names := boundNames(serve, "server.New"); len(names) == 1 && names[0] != "_" {
		out = append(out, names[0])
	}
	if calls := callsTo(serve, "BuildMatrixFeedback"); len(calls) == 1 {
		for _, param := range []string{"dispatch", "busyDispatch"} {
			idx := paramIndex(matrixBuilder, param)
			if idx >= 0 && idx < len(calls[0].Args) {
				out = append(out, types.ExprString(calls[0].Args[idx]))
			}
		}
	}
	return out
}

// drainCalls maps the receiver expression of every Drain call in fn to its
// position, so the checker can compare shutdown ORDER as well as presence. A
// receiver drained twice keeps the last position — the later call is the one
// that decides whether the wait actually covered the work.
func drainCalls(fn *ast.FuncDecl) map[string]token.Pos {
	out := map[string]token.Pos{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Drain" {
			return true
		}
		out[types.ExprString(sel.X)] = call.Pos()
		return true
	})
	return out
}

// checkArg asserts RunServe calls builder exactly once, passing want in the
// argument slot the builder's own declaration names param. why states what the
// production behaviour becomes when it does not.
func checkArg(serve, builder *ast.FuncDecl, builderName, param, want, why string) []string {
	idx := paramIndex(builder, param)
	if idx < 0 {
		return []string{fmt.Sprintf("%s has no parameter named %q: this guard can no longer tell which argument "+
			"carries it", builderName, param)}
	}
	found := callsTo(serve, builderName)
	if len(found) != 1 {
		return []string{fmt.Sprintf("RunServe calls %s %d times, want exactly once: the shared instance is the "+
			"invariant (one responder, two transports), and this guard checks the single call site",
			builderName, len(found))}
	}
	call := found[0]
	if idx >= len(call.Args) {
		return []string{fmt.Sprintf("RunServe's %s call passes %d arguments, too few to carry %q",
			builderName, len(call.Args), param)}
	}
	got := types.ExprString(call.Args[idx])
	if got == want {
		return nil
	}
	return []string{fmt.Sprintf("RunServe passes %s as %s's %q argument, want %s — %s",
		got, builderName, param, want, why)}
}

// callsTo returns every call to callee inside fn's body. callee is matched as
// the source renders it, so both a same-package name ("BuildInvestigator") and a
// qualified one ("server.New") work — the announcer's drain order is checked
// against a value RunServe binds from another package's constructor.
func callsTo(fn *ast.FuncDecl, callee string) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if types.ExprString(call.Fun) == callee {
			out = append(out, call)
		}
		return true
	})
	return out
}

// boundNames returns the identifiers fn binds from its (single) call to callee,
// in result order — "_" for a discarded result. Empty when fn never calls it or
// never binds what it returns.
func boundNames(fn *ast.FuncDecl, callee string) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if types.ExprString(call.Fun) != callee {
			return true
		}
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok {
				out = append(out, id.Name)
			} else {
				out = append(out, types.ExprString(lhs))
			}
		}
		return true
	})
	return out
}

// paramIndex reports the flat argument position of the parameter named name
// (one entry per name, so `a, b int` counts as two), or -1.
func paramIndex(fn *ast.FuncDecl, name string) int {
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, id := range field.Names {
			if id.Name == name {
				return i
			}
			i++
		}
	}
	return -1
}

// resultIndex reports the position of the first result whose type renders as
// typ, or -1.
func resultIndex(fn *ast.FuncDecl, typ string) int {
	if fn.Type.Results == nil {
		return -1
	}
	i := 0
	for _, field := range fn.Type.Results.List {
		n := max(len(field.Names), 1)
		for range n {
			if types.ExprString(field.Type) == typ {
				return i
			}
			i++
		}
	}
	return -1
}

// funcDecls indexes every top-level function declared in the SHIPPED files of
// the package rooted at dir (test files excluded — a guard that read them could
// be satisfied by a test).
func funcDecls(t *testing.T, dir string) map[string]*ast.FuncDecl {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.FuncDecl{}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		collectFuncs(f, out)
	}
	if len(out) == 0 {
		t.Fatalf("no function declarations found under %s — this guard is inert", dir)
	}
	return out
}

// parseFuncDecls is funcDecls over an in-memory source file, for the guard's own
// mutation test.
func parseFuncDecls(t *testing.T, src string) map[string]*ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	out := map[string]*ast.FuncDecl{}
	collectFuncs(f, out)
	return out
}

func collectFuncs(f *ast.File, into map[string]*ast.FuncDecl) {
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Body != nil {
			into[fn.Name.Name] = fn
		}
	}
}
