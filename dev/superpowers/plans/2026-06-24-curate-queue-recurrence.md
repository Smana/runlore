# Wire Queue + Recurrence curation passes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Light up the two dormant Phase-2 curation passes — `Queue` (promote a solved PR to ready-to-merge when its incident resolved) and `Recurrence` (open one knowledge-gap issue when an unresolved pattern recurs) — both ledger-backed and idempotent.

**Architecture:** Both read the outcome ledger via `Episodes()` (no source-specific API). `LedgerResolutionChecker` joins a PR to its incident by exact title (`"KB: "+inv.Title` ↔ `Episode.Title`). `Recurrence.Run` recomputes unresolved-pattern counts from the ledger each run and is idempotent because the forge's existing knowledge-gap issues are the "already-opened" record. `runCurate` wires both when `outcome.ledger_path` is configured.

**Tech Stack:** Go 1.26, standard library (`strings`, `fmt`), existing `internal/outcome` + `internal/curate` packages.

## Global Constraints

- Go 1.26, no new third-party dependencies.
- Both passes touch only the KB forge (relabel a PR / open an issue) — never the cluster. They run only under the opt-in curate CronJob.
- The resolution/recurrence signal is the outcome ledger's events — **source-neutral** (no Alertmanager-specific code).
- Knowledge-gap artifacts are *issues*; the `Lifecycle` stale-sweep lists *PRs* only, so they are never auto-closed. Do not change that.
- `Recurrence` default threshold is **3** when unset/zero.
- The `Episodes()` dependency is taken as a small interface `interface{ Episodes() ([]outcome.Episode, error) }` for test-fakeability; the concrete `*outcome.Ledger` satisfies it. (`curate` may import `outcome`; there is no cycle.)
- After each task: `go build ./... && go vet ./... && go test ./...` green and `gofmt -l .` empty.

---

### Task 1: `LedgerResolutionChecker` + Queue-through-ledger

**Files:**
- Modify: `internal/curate/resolution.go`
- Test: `internal/curate/resolution_test.go`

**Interfaces:**
- Consumes: `outcome.Episode` (fields `Title string`, `Resource string`, `Resolved bool`), `providers.CuratedIssue` (`Number int`, `Title string`, `Labels []string`), existing `Queue`.
- Produces: `type LedgerResolutionChecker struct { Ledger interface{ Episodes() ([]outcome.Episode, error) } }` with `func (c LedgerResolutionChecker) IsResolved(ctx context.Context, pr providers.CuratedIssue) (bool, error)`. A shared test fake `type fakeLedger struct{ eps []outcome.Episode }` with `Episodes()`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/curate/resolution_test.go` (it already imports `context`, `io`, `log/slog`, `testing`, `providers`; add `"github.com/Smana/runlore/internal/outcome"`):

```go
// fakeLedger is a fixed Episodes() source for resolution/recurrence tests.
type fakeLedger struct{ eps []outcome.Episode }

func (f fakeLedger) Episodes() ([]outcome.Episode, error) { return f.eps, nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLedgerResolutionCheckerResolvedTitle(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", Resolved: true},
		{Title: "Other", Resolved: false},
	}}}
	got, err := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: HarborRegistryDown"})
	if err != nil || !got {
		t.Fatalf("a resolved episode with the PR's title must be resolved=true; got %v err=%v", got, err)
	}
}

func TestLedgerResolutionCheckerUnresolved(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", Resolved: false}, // opened, never resolved
	}}}
	got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: HarborRegistryDown"})
	if got {
		t.Fatal("an unresolved episode must yield resolved=false")
	}
	// absent entirely → false
	if got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: Unknown"}); got {
		t.Fatal("a title with no episode must yield resolved=false")
	}
}

func TestLedgerResolutionCheckerEmptyTitle(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{{Title: "", Resolved: true}}}}
	if got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: "}); got {
		t.Fatal("an empty title must never match (avoids resolving on a blank episode)")
	}
}

func TestQueuePromotesResolvedViaLedger(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: HarborRegistryDown", Labels: []string{"runlore", "solved"}},
		{Number: 50, Title: "KB: StillBroken", Labels: []string{"runlore", "solved"}},
	}}
	led := fakeLedger{eps: []outcome.Episode{{Title: "HarborRegistryDown", Resolved: true}}}
	q := Queue{Forge: f, Checker: LedgerResolutionChecker{Ledger: led}, Log: discardLog()}
	if err := q.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("the resolved PR #48 should be queued ready-to-merge, got %v", f.relabel)
	}
	if _, ok := f.relabel[50]; ok {
		t.Fatalf("the unresolved PR #50 must not be queued, got %v", f.relabel)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/curate/ -run 'LedgerResolutionChecker|QueuePromotesResolvedViaLedger' -v`
Expected: FAIL — `LedgerResolutionChecker` undefined.

- [ ] **Step 3: Implement `LedgerResolutionChecker`**

In `internal/curate/resolution.go`, add `"strings"` and `"github.com/Smana/runlore/internal/outcome"` to the imports, and append:

```go
// LedgerResolutionChecker reports a curated PR's incident has resolved when the
// outcome ledger holds a resolved episode whose title matches the PR's. A curated
// PR's title is "KB: " + the incident title, and the ledger records each open with
// that same incident title — so the join is an exact title match. Source-agnostic:
// it reads the ledger's resolve events, never a trigger-specific API.
type LedgerResolutionChecker struct {
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
}

// IsResolved implements ResolutionChecker.
func (c LedgerResolutionChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
	title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
	if title == "" {
		return false, nil
	}
	eps, err := c.Ledger.Episodes()
	if err != nil {
		return false, err
	}
	for _, e := range eps {
		if e.Resolved && e.Title == title {
			return true, nil
		}
	}
	return false, nil
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/curate/ -run 'LedgerResolutionChecker|QueuePromotesResolvedViaLedger' -v`
Expected: PASS.

- [ ] **Step 5: Full package + gofmt**

Run: `go test ./internal/curate/ && gofmt -l internal/curate/`
Expected: PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/curate/resolution.go internal/curate/resolution_test.go
git commit -m "feat(curate): LedgerResolutionChecker — resolve a PR's incident via the outcome ledger"
```

---

### Task 2: ledger-driven `Recurrence.Run` (replaces `Store`/`Observe`)

**Files:**
- Modify: `internal/curate/recurrence.go`
- Modify: `internal/curate/resolution_test.go` (add an `issues` field to `recordingForge`)
- Test: `internal/curate/recurrence_test.go` (rewrite)

**Interfaces:**
- Consumes: `outcome.Episode`, `fakeLedger` (Task 1), `Forge` (`ListIssuesByLabel`, `OpenIssue`).
- Produces: `type Recurrence struct { Forge Forge; Ledger interface{ Episodes() ([]outcome.Episode, error) }; Threshold int; Log *slog.Logger }` with `func (r Recurrence) Run(ctx context.Context) error`; `const gapTitlePrefix = "knowledge-gap: "`; `func recurrencePattern(e outcome.Episode) string`. The old `RecurrenceStore`/`Observe` are removed.

- [ ] **Step 1: Give `recordingForge` an issues list**

In `internal/curate/resolution_test.go`, change the `recordingForge` struct and its `ListIssuesByLabel` so existing knowledge-gap issues can be returned (the recurrence idempotency guard needs this):

```go
type recordingForge struct {
	prs     []providers.CuratedIssue
	issues  []providers.CuratedIssue // returned by ListIssuesByLabel
	relabel map[int]string           // number -> added label
}
```

and:

```go
func (f *recordingForge) ListIssuesByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.issues, nil
}
```

(Leave the other `recordingForge` methods unchanged.)

- [ ] **Step 2: Rewrite the recurrence tests**

Replace the entire body of `internal/curate/recurrence_test.go` with (note `memStore` is gone; `gapForge` stays):

```go
package curate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// gapForge records the issue titles it was asked to open. It embeds recordingForge
// (which supplies ListIssuesByLabel from its `issues` field) and overrides OpenIssue.
type gapForge struct {
	*recordingForge
	openedTitles []string
}

func (g *gapForge) OpenIssue(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	g.openedTitles = append(g.openedTitles, inv.Title)
	return providers.Ref{URL: "https://github.com/x/y/issues/1"}, nil
}

func unresolved(pattern string, n int) []outcome.Episode {
	eps := make([]outcome.Episode, n)
	for i := range eps {
		eps[i] = outcome.Episode{Resource: pattern, Resolved: false}
	}
	return eps
}

func TestRecurrenceOpensGapIssueAtThreshold(t *testing.T) {
	eps := append(unresolved("apps/web", 3), outcome.Episode{Resource: "apps/worker", Resolved: false}) // worker: only 1
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: apps/web" {
		t.Fatalf("want one gap issue for apps/web, got %v", gf.openedTitles)
	}
}

func TestRecurrenceBelowThresholdNoIssue(t *testing.T) {
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: unresolved("apps/web", 2)}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("below threshold must open nothing, got %v", gf.openedTitles)
	}
}

func TestRecurrenceIdempotentWhenIssueExists(t *testing.T) {
	gf := &gapForge{recordingForge: &recordingForge{
		issues: []providers.CuratedIssue{{Title: "knowledge-gap: apps/web"}}, // already open
	}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: unresolved("apps/web", 5)}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("an existing gap issue must prevent a duplicate, got %v", gf.openedTitles)
	}
}

func TestRecurrenceResolvedEpisodesDoNotCount(t *testing.T) {
	eps := []outcome.Episode{
		{Resource: "apps/web", Resolved: true}, {Resource: "apps/web", Resolved: true},
		{Resource: "apps/web", Resolved: false}, // only 1 unresolved
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("resolved episodes must not count, got %v", gf.openedTitles)
	}
}

func TestRecurrencePatternFallsBackToTitle(t *testing.T) {
	eps := []outcome.Episode{
		{Title: "DNSFailure", Resolved: false}, {Title: "DNSFailure", Resolved: false}, {Title: "DNSFailure", Resolved: false},
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: DNSFailure" {
		t.Fatalf("a resource-less episode should group by title, got %v", gf.openedTitles)
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./internal/curate/ -run TestRecurrence -v`
Expected: FAIL to compile — `Recurrence` has no `Ledger`/`Run` (still the old `Store`/`Observe`).

- [ ] **Step 4: Rewrite `recurrence.go`**

Replace the entire contents of `internal/curate/recurrence.go` with:

```go
package curate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// gapTitlePrefix titles every knowledge-gap issue; it is also the existence-check
// key (the pass opens at most one issue per pattern).
const gapTitlePrefix = "knowledge-gap: "

// Recurrence opens ONE knowledge-gap issue when an unresolved incident pattern recurs
// at least Threshold times. It is ledger-driven and idempotent: it recomputes counts
// from the outcome ledger every run and opens an issue only when no knowledge-gap
// issue for that pattern already exists — so re-running never double-opens (no mutable
// store or watermark).
type Recurrence struct {
	Forge  Forge
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
	Threshold int // default 3 when 0
	Log       *slog.Logger
}

// Run opens a knowledge-gap issue for each pattern that crosses the threshold and has
// none open yet.
func (r Recurrence) Run(ctx context.Context) error {
	thr := r.Threshold
	if thr == 0 {
		thr = 3
	}
	eps, err := r.Ledger.Episodes()
	if err != nil {
		return fmt.Errorf("recurrence: load episodes: %w", err)
	}
	// Count UNRESOLVED occurrences per pattern (affected resource; title fallback).
	counts := map[string]int{}
	for _, e := range eps {
		if e.Resolved {
			continue
		}
		counts[recurrencePattern(e)]++
	}
	// Existing knowledge-gap issues are the idempotency guard. OpenIssue labels issues
	// "runlore"/"triggered" and titles them gapTitlePrefix+pattern, so match by title.
	existing, err := r.Forge.ListIssuesByLabel(ctx, "runlore")
	if err != nil {
		return fmt.Errorf("recurrence: list issues: %w", err)
	}
	open := map[string]bool{}
	for _, iss := range existing {
		if p, ok := strings.CutPrefix(iss.Title, gapTitlePrefix); ok {
			open[p] = true
		}
	}
	for pattern, n := range counts {
		if n < thr || open[pattern] {
			continue
		}
		inv := providers.Investigation{
			Title: gapTitlePrefix + pattern,
			RootCauses: []providers.Hypothesis{{
				Summary: fmt.Sprintf("RunLore could not resolve incidents on %q across %d occurrences — needs seeded knowledge or a RunLore fix.", pattern, n),
			}},
		}
		if _, err := r.Forge.OpenIssue(ctx, inv); err != nil {
			r.Log.Warn("recurrence: open knowledge-gap issue failed", "pattern", pattern, "err", err)
			continue // best-effort; other patterns still processed
		}
		r.Log.Info("opened knowledge-gap issue", "pattern", pattern, "count", n)
	}
	return nil
}

// recurrencePattern groups unresolved incidents by the affected resource, falling
// back to the incident title when no resource was identified.
func recurrencePattern(e outcome.Episode) string {
	if e.Resource != "" {
		return e.Resource
	}
	return e.Title
}
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./internal/curate/ -run TestRecurrence -v`
Expected: PASS.

- [ ] **Step 6: Full package + build + gofmt**

Run: `go build ./... && go test ./internal/curate/ && gofmt -l internal/curate/`
Expected: build PASS (no leftover references to the removed `RecurrenceStore`/`Observe`); tests PASS; gofmt prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/curate/recurrence.go internal/curate/recurrence_test.go internal/curate/resolution_test.go
git commit -m "feat(curate): ledger-driven Recurrence.Run (idempotent via existing-issue check)"
```

---

### Task 3: `curate.recurrence_threshold` config

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `Curate.RecurrenceThreshold int` (`yaml:"recurrence_threshold"`).

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go` (it already imports `time`, `yaml`):

```go
func TestCurateRecurrenceThresholdParse(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  recurrence_threshold: 5\n"), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Curate.RecurrenceThreshold != 5 {
		t.Fatalf("recurrence_threshold: want 5, got %d", c.Curate.RecurrenceThreshold)
	}
	// Absent ⇒ zero ⇒ the pass applies its own default (3).
	var z Config
	if err := yaml.Unmarshal([]byte("curate:\n  stale_after: 240h\n"), &z); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if z.Curate.RecurrenceThreshold != 0 {
		t.Fatalf("absent recurrence_threshold must be 0, got %d", z.Curate.RecurrenceThreshold)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run TestCurateRecurrenceThresholdParse -v`
Expected: FAIL — `Curate` has no `RecurrenceThreshold` field.

- [ ] **Step 3: Add the field**

In `internal/config/config.go`, extend the `Curate` struct:

```go
type Curate struct {
	StaleAfter          Duration `yaml:"stale_after"`          // close unprotected KB PRs idle longer than this; 0 disables (default 720h)
	RecurrenceThreshold int      `yaml:"recurrence_threshold"` // open a knowledge-gap issue after this many unresolved occurrences of a pattern; 0 ⇒ default 3
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/config/ -run TestCurateRecurrenceThresholdParse -v`
Expected: PASS.

- [ ] **Step 5: Build + gofmt**

Run: `go build ./... && gofmt -l internal/config/`
Expected: build PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): curate.recurrence_threshold knob"
```

---

### Task 4: wire Queue + Recurrence into `runCurate` + chart note

**Files:**
- Modify: `cmd/lore/main.go` (the `runCurate` agent construction)
- Modify: `deploy/helm/runlore/values.yaml` (curate comment)

**Interfaces:**
- Consumes: `curate.Queue`, `curate.LedgerResolutionChecker`, `curate.Recurrence` (Tasks 1-2), `cfg.Curate.RecurrenceThreshold` (Task 3), `cfg.Outcome.LedgerPath`, `outcome.New` (already imported in `main.go`).

- [ ] **Step 1: Wire the passes**

In `cmd/lore/main.go`'s `runCurate`, replace the agent construction (currently `curate.Dedup` + `curate.Lifecycle`) and its `agent.Run` call with:

```go
	agent := curate.Agent{Log: log, Passes: []curate.Pass{
		curate.Dedup{Forge: forge, Log: log},
		curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
	}}
	// Queue + Recurrence read the outcome ledger; wire them only when it is configured.
	if cfg.Outcome.LedgerPath != "" {
		ledger, lerr := outcome.New(cfg.Outcome.LedgerPath)
		if lerr != nil {
			return fmt.Errorf("open outcome ledger %q: %w", cfg.Outcome.LedgerPath, lerr)
		}
		agent.Passes = append(agent.Passes,
			curate.Queue{Forge: forge, Checker: curate.LedgerResolutionChecker{Ledger: ledger}, Log: log},
			curate.Recurrence{Forge: forge, Ledger: ledger, Threshold: cfg.Curate.RecurrenceThreshold, Log: log},
		)
		log.Info("curate: Queue + Recurrence enabled", "ledger", cfg.Outcome.LedgerPath)
	} else {
		log.Info("curate: outcome ledger not configured; Queue + Recurrence passes skipped")
	}
	log.Info("curate: grooming KB backlog", "repo", cfg.Forge.KBRepo)
	agent.Run(context.Background())
```

Confirm `cmd/lore/main.go` imports `outcome` (`github.com/Smana/runlore/internal/outcome`) — it does (the serve path opens the ledger). `fmt` is imported.

- [ ] **Step 2: Build + vet + full suite + gofmt**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt prints nothing.

- [ ] **Step 3: Manual smoke — curate still parses + runs with a ledger configured**

```bash
printf 'forge:\n  kb_repo: o/r\noutcome:\n  ledger_path: /tmp/curate-ledger.jsonl\ncurate:\n  recurrence_threshold: 3\n' > /tmp/curate-smoke.yaml
go run ./cmd/lore curate -config /tmp/curate-smoke.yaml 2>&1 | head -3 || true
```
Expected: it parses the config and then fails on the missing GitHub App (`curate requires a configured GitHub App`) — NOT a yaml/ledger/flag error. (The ledger path need not exist; `outcome.New` tolerates an absent file.)

- [ ] **Step 4: Update the chart comment**

In `deploy/helm/runlore/values.yaml`, update the `config.curate` comment to note the ledger requirement (find the block added in #12):

```yaml
  # Phase-2 backlog groomer (lore curate). `stale_after` drives the lifecycle sweep;
  # the Queue (promote solved→ready-to-merge on resolution) and Recurrence
  # (open a knowledge-gap issue for repeatedly-unresolved patterns) passes also run
  # when config.outcome.ledger_path is set (they read the outcome ledger).
  curate:
    stale_after: 720h          # close unprotected KB PRs idle longer than this; 0 disables
    recurrence_threshold: 3    # open a knowledge-gap issue after this many unresolved occurrences; 0 ⇒ 3
```

(Keep `stale_after`'s existing value; add the `recurrence_threshold` line and the comment update.)

- [ ] **Step 5: Render smoke**

Run: `helm template t deploy/helm/runlore | python3 -c "import sys,yaml; list(yaml.safe_load_all(sys.stdin)); print('helm yaml ok')"`
Expected: `helm yaml ok`.

- [ ] **Step 6: Commit**

```bash
git add cmd/lore/main.go deploy/helm/runlore/values.yaml
git commit -m "feat(curate): wire Queue + Recurrence in lore curate when the ledger is configured"
```

---

## Self-Review

**Spec coverage:**
- `LedgerResolutionChecker` (exact title join, reuse `Episodes()`) → Task 1. ✅
- Ledger-driven `Recurrence.Run`, idempotent existence-check, pattern = resource/title → Task 2. ✅
- Remove `Store`/`Observe` → Task 2. ✅
- `cfg.Curate.RecurrenceThreshold` (default 3) → Task 3. ✅
- `runCurate` wires both only when `outcome.ledger_path` set → Task 4. ✅
- Source-neutral (ledger only), forge-only writes, gap issues immune to stale-sweep → Tasks 1-2 (no Lifecycle change). ✅
- Chart comment (no behavioral change) → Task 4. ✅

**Placeholder scan:** every step shows complete code; no TBD/TODO. ✅

**Type consistency:** `LedgerResolutionChecker.Ledger` and `Recurrence.Ledger` use the same `interface{ Episodes() ([]outcome.Episode, error) }`; `fakeLedger`/`discardLog`/`recordingForge.issues`/`gapForge` defined in Task 1/2 and reused consistently; `gapTitlePrefix`/`recurrencePattern` defined in Task 2; `Queue` is the existing type (unchanged). The `outcome.Episode` fields used (`Title`, `Resource`, `Resolved`) match `internal/outcome/ledger.go`. ✅
