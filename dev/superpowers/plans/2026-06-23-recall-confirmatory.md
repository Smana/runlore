# Confirmatory evidence on the recall short-circuit Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On an instant-recall hit, confront the recalled finding with current cluster state (the existing `pod_status`/`kube_events` tools, scoped to the workload) and feed that real evidence to the verify pass; when state can't be gathered, cap the recalled confidence lower.

**Architecture:** A new `confirmRecall` method reuses the loop's own tools to append current-state evidence to the recalled finding before verify runs; `capRecallConfidence` lowers confidence when confirmation was unavailable. Both wired into the recall block in `loop.go`.

**Tech Stack:** Go 1.26, standard library (`encoding/json`, `fmt`, `strings`), existing `internal/investigate` tool interface.

## Global Constraints

- Go 1.26, standard library only, no new dependencies, no new wiring in `cmd/lore/main.go` (reuse `li.Tools`).
- Best-effort/fail-safe: a missing namespace, absent tools, or a tool error must degrade to `gathered=false` (then the lower cap) — never crash, never drop the finding.
- `capRecallConfidence` only ever LOWERS confidence; apply it BEFORE verify so verify's max-of-survivors recomputation stays consistent, and BEFORE `initialConfidence` is captured (so the cap is not mis-counted as a verify "downgrade" in metrics).
- Do not change `recalledInvestigation`'s existing recall-label evidence line; the confirmatory evidence is appended alongside it.
- After each task: `go build ./... && go vet ./... && go test ./...` green and `gofmt -l .` empty.

---

### Task 1: `confirmRecall` + `capRecallConfidence` helpers

**Files:**
- Create: `internal/investigate/confirm.go`
- Test: `internal/investigate/confirm_test.go`

**Interfaces:**
- Consumes: `LoopInvestigator.Tools []Tool` (each `Tool` has `Name()`/`Call(ctx,args)`), `Request.Workload providers.Workload` (`Namespace`,`Name`), `providers.Investigation`/`Hypothesis`.
- Produces:
  - `const recallUnconfirmedCap = 0.70`
  - `var recallConfirmTools = []string{"pod_status", "kube_events"}`
  - `func (li *LoopInvestigator) confirmRecall(ctx context.Context, req Request, inv providers.Investigation) (providers.Investigation, bool)`
  - `func capRecallConfidence(inv providers.Investigation, ceiling float64) providers.Investigation`

- [ ] **Step 1: Write the failing tests**

Create `internal/investigate/confirm_test.go` (package `investigate`, matching the other test files):

```go
package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeConfirmTool is a confirmatory Tool that records the args it was called with.
type fakeConfirmTool struct {
	name    string
	out     string
	err     error
	gotArgs string
}

func (f *fakeConfirmTool) Name() string        { return f.name }
func (f *fakeConfirmTool) Description() string  { return "" }
func (f *fakeConfirmTool) Schema() string       { return "{}" }
func (f *fakeConfirmTool) Call(_ context.Context, args string) (string, error) {
	f.gotArgs = args
	return f.out, f.err
}

func recalledInv() providers.Investigation {
	return providers.Investigation{
		Title:      "web down",
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout", Confidence: 0.9,
			Evidence: []string{"instant recall: matched knowledge-base entry \"x\""}}},
		Resource: providers.Workload{Namespace: "apps", Name: "web"},
	}
}

func TestConfirmRecallAppendsCurrentState(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "web CrashLoopBackOff"}
	li := &LoopInvestigator{Tools: []Tool{ps}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	inv, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("expected gathered=true when a confirm tool returns output")
	}
	joined := strings.Join(inv.RootCauses[0].Evidence, "\n")
	if !strings.Contains(joined, "CrashLoopBackOff") || !strings.Contains(joined, "pod_status") {
		t.Fatalf("current-state evidence not appended: %q", joined)
	}
}

func TestConfirmRecallScopesToWorkload(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "ok"}
	ev := &fakeConfirmTool{name: "kube_events", out: "Warning"}
	li := &LoopInvestigator{Tools: []Tool{ps, ev}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if _, gathered := li.confirmRecall(context.Background(), req, recalledInv()); !gathered {
		t.Fatal("expected gathered=true")
	}
	if !strings.Contains(ps.gotArgs, `"namespace":"apps"`) {
		t.Fatalf("pod_status not scoped to namespace: %q", ps.gotArgs)
	}
	if !strings.Contains(ev.gotArgs, `"namespace":"apps"`) || !strings.Contains(ev.gotArgs, `"object":"web"`) {
		t.Fatalf("kube_events not scoped to namespace+object: %q", ev.gotArgs)
	}
}

func TestConfirmRecallNoNamespaceSkips(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "x"}
	li := &LoopInvestigator{Tools: []Tool{ps}}
	req := Request{Workload: providers.Workload{}} // no namespace
	inv, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if gathered {
		t.Fatal("no namespace must skip confirmation")
	}
	if ps.gotArgs != "" {
		t.Fatalf("tool must not be called without a namespace, got args %q", ps.gotArgs)
	}
	if len(inv.RootCauses[0].Evidence) != 1 {
		t.Fatalf("evidence must be unchanged, got %v", inv.RootCauses[0].Evidence)
	}
}

func TestConfirmRecallToolsAbsentSkips(t *testing.T) {
	li := &LoopInvestigator{Tools: []Tool{&fakeConfirmTool{name: "what_changed", out: "x"}}}
	req := Request{Workload: providers.Workload{Namespace: "apps"}}
	if _, gathered := li.confirmRecall(context.Background(), req, recalledInv()); gathered {
		t.Fatal("no confirm tools present must yield gathered=false")
	}
}

func TestConfirmRecallToolErrorTolerated(t *testing.T) {
	bad := &fakeConfirmTool{name: "pod_status", err: context.DeadlineExceeded}
	good := &fakeConfirmTool{name: "kube_events", out: "Warning FailedMount"}
	li := &LoopInvestigator{Tools: []Tool{bad, good}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	inv, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("one tool erroring must not prevent the other from confirming")
	}
	if !strings.Contains(strings.Join(inv.RootCauses[0].Evidence, "\n"), "FailedMount") {
		t.Fatal("the surviving tool's output should be appended")
	}
}

func TestCapRecallConfidenceOnlyLowers(t *testing.T) {
	inv := providers.Investigation{Confidence: 0.9, RootCauses: []providers.Hypothesis{{Confidence: 0.9}, {Confidence: 0.5}}}
	out := capRecallConfidence(inv, 0.70)
	if out.Confidence != 0.70 || out.RootCauses[0].Confidence != 0.70 {
		t.Fatalf("values above the ceiling must be lowered: %+v", out)
	}
	if out.RootCauses[1].Confidence != 0.5 {
		t.Fatalf("a value already below the ceiling must be untouched, got %v", out.RootCauses[1].Confidence)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/investigate/ -run 'TestConfirmRecall|TestCapRecallConfidence' -v`
Expected: FAIL — `confirmRecall` / `capRecallConfidence` undefined.

- [ ] **Step 3: Implement `internal/investigate/confirm.go`**

```go
package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// recallUnconfirmedCap is the recall-confidence ceiling applied when current cluster
// state could not be gathered to confront the recalled entry.
const recallUnconfirmedCap = 0.70

// recallConfirmTools are the read-only, namespace-scoped checks used to confront a
// recalled finding with current cluster state, in priority order. They are the same
// tools the agent uses, resolved from the loop's tool set.
var recallConfirmTools = []string{"pod_status", "kube_events"}

// confirmRecall gathers current cluster state for the recalled workload and appends
// it to the top hypothesis's evidence, so the verify pass can judge the recalled
// cause against reality rather than a tautology. Best-effort: a missing namespace,
// absent tools, or a tool error yields gathered=false. gathered is true when at
// least one confirm tool returned non-empty output (including "no pods"/"no events"
// — still real current state).
func (li *LoopInvestigator) confirmRecall(ctx context.Context, req Request, inv providers.Investigation) (providers.Investigation, bool) {
	if req.Workload.Namespace == "" || len(inv.RootCauses) == 0 {
		return inv, false
	}
	byName := make(map[string]Tool, len(li.Tools))
	for _, t := range li.Tools {
		byName[t.Name()] = t
	}
	gathered := false
	for _, name := range recallConfirmTools {
		t, ok := byName[name]
		if !ok {
			continue
		}
		out, err := t.Call(ctx, confirmArgs(name, req.Workload))
		if err != nil {
			if li.Log != nil {
				li.Log.Debug("recall confirm tool failed", "tool", name, "err", err)
			}
			continue
		}
		if out = strings.TrimSpace(out); out == "" {
			continue
		}
		inv.RootCauses[0].Evidence = append(inv.RootCauses[0].Evidence,
			fmt.Sprintf("current state — %s:\n%s", name, out))
		gathered = true
	}
	return inv, gathered
}

// confirmArgs builds the JSON args scoping a confirmatory tool to the workload.
func confirmArgs(name string, w providers.Workload) string {
	m := map[string]string{"namespace": w.Namespace}
	if name == "kube_events" && w.Name != "" {
		m["object"] = w.Name
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// capRecallConfidence lowers the investigation's overall and per-hypothesis
// confidence to at most ceiling (it never raises any value).
func capRecallConfidence(inv providers.Investigation, ceiling float64) providers.Investigation {
	if inv.Confidence > ceiling {
		inv.Confidence = ceiling
	}
	for i := range inv.RootCauses {
		if inv.RootCauses[i].Confidence > ceiling {
			inv.RootCauses[i].Confidence = ceiling
		}
	}
	return inv
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/investigate/ -run 'TestConfirmRecall|TestCapRecallConfidence' -v`
Expected: PASS.

- [ ] **Step 5: Full package + gofmt**

Run: `go test ./internal/investigate/ && gofmt -l internal/investigate/`
Expected: PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/investigate/confirm.go internal/investigate/confirm_test.go
git commit -m "feat(investigate): confirmRecall gathers current state; capRecallConfidence"
```

---

### Task 2: Wire the confirmatory step into the recall block

**Files:**
- Modify: `internal/investigate/loop.go`
- Test: `internal/investigate/loop_test.go`

**Interfaces:**
- Consumes: `confirmRecall`, `capRecallConfidence`, `recallUnconfirmedCap` (Task 1).

- [ ] **Step 1: Write the failing tests**

Read `internal/investigate/loop_test.go` first to mirror its existing recall-test construction (how it builds `LoopInvestigator` with a `Recall`, the `scriptModel`/fake catalog, and how it captures the delivered investigation via `OnComplete`). Then add, using those same patterns:

```go
func TestInstantRecallUnconfirmedLowersConfidence(t *testing.T) {
	// A recall hit with NO confirm tools available → confidence is capped at
	// recallUnconfirmedCap before delivery.
	var got providers.Investigation
	li := &LoopInvestigator{
		// Recall configured to return a high-confidence hit; mirror the existing
		// recall-hit test's setup (fake catalog/recall thresholds).
		// Tools intentionally omit pod_status/kube_events.
		Tools:      nil,
		Verify:     false,
		OnComplete: func(inv providers.Investigation) { got = inv },
		Log:        testLoggerLoop(),
		Recall:     <recall returning a hit, per the existing TestInstantRecallHit setup>,
	}
	req := Request{Title: "web down", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got.Confidence > recallUnconfirmedCap {
		t.Fatalf("unconfirmed recall must be capped at %.2f, got %.2f", recallUnconfirmedCap, got.Confidence)
	}
}

func TestInstantRecallConfirmedEvidenceReachesVerify(t *testing.T) {
	// A recall hit + a confirm tool whose output contradicts the entry + a verify
	// model that rejects the (now evidence-bearing) root cause → the delivered
	// finding is rejected (root causes emptied), proving the confirmatory evidence
	// reached verify rather than the tautological string.
	var got providers.Investigation
	ps := &fakeConfirmTool{name: "pod_status", out: "web Running ready=1/1 (healthy — contradicts the recalled crash)"}
	li := &LoopInvestigator{
		Tools:  []Tool{ps},
		Verify: true,
		// Model used only by verify here (recall short-circuits the loop): script it
		// to submit a single "reject" verdict for index 0. Mirror verify_test.go's
		// scriptModel verdict-response shape.
		Model:      <scriptModel returning one submit_verdicts call: reject index 0>,
		OnComplete: func(inv providers.Investigation) { got = inv },
		Log:        testLoggerLoop(),
		Recall:     <recall returning a hit, per TestInstantRecallHit setup>,
	}
	req := Request{Title: "web down", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(got.RootCauses) != 0 {
		t.Fatalf("verify should have rejected the recalled cause using current-state evidence, got %+v", got.RootCauses)
	}
}
```

IMPORTANT: the `<...>` placeholders above MUST be filled from the actual existing
`loop_test.go` patterns — reuse its recall-hit fixture (the fake `Recall`/catalog
that makes `lookup` return an entry) and `verify_test.go`'s `scriptModel` verdict
shape. `fakeConfirmTool` is defined in `confirm_test.go` (same package) and is reusable
here. If `loop_test.go` already has a logger helper, use it; otherwise use
`slog.New(slog.NewTextHandler(io.Discard, nil))`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/investigate/ -run 'TestInstantRecallUnconfirmed|TestInstantRecallConfirmed' -v`
Expected: FAIL — confidence not yet capped / evidence not yet reaching verify.

- [ ] **Step 3: Wire the recall block in `loop.go`**

In the recall short-circuit (after `rec := recalledInvestigation(req, *entry, conf)`), insert the confirmatory step and cap, so the block reads:

```go
			rec := recalledInvestigation(req, *entry, conf)
			rec, confirmed := li.confirmRecall(ctx, req, rec)
			if !confirmed {
				// Could not confront the entry with current state — be less assertive
				// so an unverifiable recall does not present at full recall confidence.
				rec = capRecallConfidence(rec, recallUnconfirmedCap)
			}
			initialConfidence := rec.Confidence
			if li.Verify {
				// Catalog content is untrusted: verify a recalled finding too, so a
				// crafted high-recall entry can't bypass the adversarial review.
				rec = li.verifyFindings(ctx, req, rec)
			}
```

The rest of the block (metrics, `li.deliver(req, rec)`, `return nil`) is unchanged.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/investigate/ -run 'TestInstantRecallUnconfirmed|TestInstantRecallConfirmed' -v`
Expected: PASS.

- [ ] **Step 5: Full suite + vet + gofmt**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt prints nothing. In particular the existing `TestInstantRecallHit` must still pass (it has no namespace-scoped confirm tools, so it now takes the unconfirmed-cap path — confirm its assertions still hold; if it asserted an exact confidence above 0.70, update it to reflect the cap and note why).

- [ ] **Step 6: Commit**

```bash
git add internal/investigate/loop.go internal/investigate/loop_test.go
git commit -m "feat(investigate): confront recalled findings with current state before verify"
```

---

## Self-Review

**Spec coverage:**
- Confirmatory step reusing pod_status/kube_events scoped to the workload → Task 1 (`confirmRecall`). ✅
- Append real evidence to the recalled finding before verify → Tasks 1, 2. ✅
- Lower confidence cap when unconfirmed → Task 1 (`capRecallConfidence`) + Task 2 wiring. ✅
- Best-effort/fail-safe (no namespace / absent tools / tool error) → Task 1 tests. ✅
- Apply cap before verify + before `initialConfidence` → Task 2 Step 3. ✅
- No `main.go` wiring, reuse `li.Tools` → Task 1. ✅

**Placeholder scan:** Task 2's `<...>` are explicitly flagged to fill from existing `loop_test.go`/`verify_test.go` fixtures (test wiring that depends on patterns the implementer must read), not production-code placeholders. All production code (confirm.go, the loop.go block) is complete. ✅

**Type consistency:** `confirmRecall(ctx, req, inv) (Investigation, bool)`, `capRecallConfidence(inv, ceiling)`, `recallUnconfirmedCap`, `recallConfirmTools` defined in Task 1 and consumed in Task 2 with matching signatures. `fakeConfirmTool` defined in Task 1's test file, reused in Task 2 (same package). ✅
