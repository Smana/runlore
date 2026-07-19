# Recall Runner-Up Fallback (N3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A decayed/poisoned recall winner no longer disables instant recall while a healthy
corrected entry sits right behind it — and an outcome-rejected entry can never resurface as the
near-miss lead.

**Architecture:** Gate 3 (outcome decay, `internal/investigate/recall.go`) today evaluates ONLY the
single winner picked by the fire gate; a `low_outcome` rejection abandons recall entirely, and the
near-miss lookup can then re-surface the very entry that was just rejected (only the
verify-rejection path excludes anything). This plan (1) extracts the outcome gate into a helper fed
by one `OpenCounts()` snapshot, (2) adds a bounded fallback — magnitude mode walks up to
`maxOutcomeFallback` further structurally-agreeing candidates, each held to the CONSERVATIVE solo
bar (the margin-over-runner-up argument no longer holds once the winner is skipped); reranker mode
makes ONE bounded re-rank call over the remaining candidates (the rank verdict names a single
entry, so there is no free list to reuse) — every fallback candidate subject to the SAME outcome
gate, and (3) threads the outcome-rejected paths out of `lookupWithUsage` so `tryRecall` excludes
them from the near-miss lead. Fail-safe invariant unchanged: every rejection still falls through to
a full investigation.

**Tech Stack:** Go (toolchain go1.26.5), stdlib testing, existing package fakes
(`fakeOutcome` in `internal/investigate` tests).

## Global Constraints

- Quality gate before EVERY commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — golangci-lint must report `0 issues`, `gofmt -l .` must print nothing.
- Additionally run `go test -race ./internal/investigate/` before the final commit.
- New `.go` files start with `// SPDX-License-Identifier: Apache-2.0` on line 1.
- Conventional Commits; NO co-author trailer, no AI attribution.
- Fail-safe bias: a recall rejection ALWAYS falls through to a full investigation; no fallback may fire an entry that fails the outcome gate.
- Metrics: extend the existing `recall_rejections_total` reason vocabulary only (one `low_outcome` add per rejected candidate); NO new instruments.
- **Conflicts:** this plan and `2026-07-19-downvote-recovery.md` (N5) both touch the outcome gate in `internal/investigate/recall.go` (N5 changes `outcomeFactor`'s signature). Execute sequentially — whichever lands second rebases; the join point is the single `outcomeFactor(...)` call inside `outcomeGate`.

## File Structure

- Modify: `internal/investigate/recall.go` — `outcomeGate` helper, `maxOutcomeFallback`, `outcomeFallback` method, 3-value `lookupWithUsage`, variadic `nearMissExcluding`.
- Modify: `internal/investigate/loop.go:602-688` — `tryRecall` threads the rejected paths.
- Create: `internal/investigate/recall_fallback_test.go` — all new tests.
- Modify: `docs/learning-loop.md` — one paragraph on the fallback in the outcome-decay section.

---

### Task 1: Extract the outcome gate into a helper (pure refactor)

**Files:**
- Modify: `internal/investigate/recall.go:236-252` (the Gate-3 block inside `lookupWithUsage`)
- Test: existing suite only — behavior must be byte-for-byte identical

**Interfaces:**
- Produces: `func (r *Recall) outcomeGate(counts map[string]outcome.Aggregate, path string) (float64, bool)` — factor to multiply into confidence, ok=false ⇒ reject. Task 2 and 3 call it per candidate.

- [ ] **Step 1: Add the helper** in `internal/investigate/recall.go`, directly above `outcomeFactor`:

```go
// outcomeGate applies outcome decay (Gate 3) to ONE candidate entry, given the
// OpenCounts snapshot the caller fetched once per lookup. It returns the decay
// factor to multiply into the recall confidence, and ok=false when the entry's
// track record falls below OutcomeFloor (the caller must not fire this entry).
// Fail-safe: an entry with no recall/feedback history returns (1, true) —
// absence of evidence never blocks a recall.
func (r *Recall) outcomeGate(counts map[string]outcome.Aggregate, path string) (float64, bool) {
	agg, ok := counts[path]
	if !ok {
		return 1, true
	}
	f := outcomeFactor(agg.Recalls, agg.Resolved, agg.FeedbackUp, agg.FeedbackDown, r.OutcomePrior)
	return f, f >= r.OutcomeFloor
}
```

- [ ] **Step 2: Rewrite the Gate-3 block** in `lookupWithUsage` (currently lines 239-252) to use it:

```go
	// Outcome decay: bias confidence by the entry's resolution track record, and
	// reject (re-investigate) an entry that recalls-but-never-resolves. Fail-safe —
	// a rejected recall just falls through to a full investigation.
	if r.Outcome != nil {
		if counts, err := r.Outcome.OpenCounts(); err == nil {
			f, ok := r.outcomeGate(counts, e.Path)
			if !ok {
				r.reject(ctx, "low_outcome")
				return nil, 0
			}
			conf = clampF(conf*f, 0, 0.90)
		} else if r.Log != nil {
			r.Log.Warn("recall: outcome stats unavailable; skipping decay", "err", err)
		}
	}
```

(Behavior note: an entry with no history now multiplies by 1 — `clampF(conf*1, 0, 0.90)` is
identical to the previous skip because `conf` is already ≤ 0.90 on both gate paths.)

- [ ] **Step 3: Run the package tests**

Run: `go test ./internal/investigate/ -run 'Recall|Rerank' -v`
Expected: PASS, no failures (pure refactor).

- [ ] **Step 4: Full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build OK, all tests pass, no gofmt output, `0 issues`.

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/recall.go
git commit -m "refactor(recall): extract the outcome-decay gate into outcomeGate"
```

---

### Task 2: Magnitude-mode runner-up fallback

**Files:**
- Modify: `internal/investigate/recall.go` — add `maxOutcomeFallback`, `outcomeFallback`; restructure the tail of `lookupWithUsage`
- Create: `internal/investigate/recall_fallback_test.go`

**Interfaces:**
- Consumes: `outcomeGate` from Task 1.
- Produces: `lookupWithUsage(ctx, req, totals) (*catalog.Entry, float64, []string)` — third return is the outcome-rejected entry paths (nil when none); `lookup` stays 2-valued. `func (r *Recall) outcomeFallback(ctx context.Context, req Request, agreeing []catalog.ScoredEntry, counts map[string]outcome.Aggregate, minScore, soloFloor float64, totals *providers.UsageTotals, rejected []string) (*catalog.Entry, float64, []string)`. Task 3 extends `outcomeFallback` with the reranker branch; Task 4 threads the third return.

- [ ] **Step 1: Write the failing tests** — create `internal/investigate/recall_fallback_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// twoEntryCatalog seeds two structurally-agreeing entries for apps/worker. The
// "stale" entry repeats the query's symptom tokens so it outranks "fixed" on BM25
// and is deterministically the winner; both clear the (low) fire thresholds.
func twoEntryCatalog(t *testing.T) (*catalog.Catalog, string, string) {
	t.Helper()
	dir := t.TempDir()
	stale := `---
type: Incident
title: worker OOMKilled memory limit drop OOMKilled memory
description: apps/worker pods OOMKilled after memory limit drop
resource: apps/worker
---

# Symptom
apps/worker pods OOMKilled after a memory limit drop. OOMKilled memory limit drop.
`
	fixed := `---
type: Incident
title: worker OOMKilled corrected runbook
description: raise the worker memory request and limit together
resource: apps/worker
---

# Symptom
apps/worker OOMKilled.
`
	for name, body := range map[string]string{"stale.md": stale, "fixed.md": fixed} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	var stalePath, fixedPath string
	for _, e := range cat.Entries() {
		if e.Title == "worker OOMKilled corrected runbook" {
			fixedPath = e.Path
		} else {
			stalePath = e.Path
		}
	}
	return cat, stalePath, fixedPath
}

func fallbackReq() Request {
	return Request{
		Title:    "WorkerOOM",
		Message:  "apps/worker pods OOMKilled after memory limit drop",
		Workload: providers.Workload{Namespace: "apps", Name: "worker"},
	}
}

func TestRunnerUpFiresWhenWinnerDecayed(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	r := &Recall{
		Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0}, // factor 1/6 < 0.5 → rejected
			fixedPath: {Recalls: 3, Resolved: 3}, // healthy
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	// Premise: the stale entry IS the winner (rejected first).
	e, conf, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e == nil {
		t.Fatalf("runner-up must fire when the winner is outcome-rejected; got nil (rejected=%v)", rejected)
	}
	if e.Path != fixedPath {
		t.Fatalf("fired %q, want the healthy runner-up %q", e.Path, fixedPath)
	}
	if conf <= 0 || conf > 0.90 {
		t.Fatalf("runner-up confidence %v out of (0, 0.90]", conf)
	}
	if len(rejected) != 1 || rejected[0] != stalePath {
		t.Fatalf("rejected paths = %v, want exactly the decayed winner %q", rejected, stalePath)
	}
}

func TestRunnerUpMustClearConservativeSoloBar(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	// Solo floor set between the two BM25 scores: the winner clears it, the
	// runner-up does not → the fallback must NOT fire the runner-up.
	winnerScore, runnerScore := twoScores(t, cat, stalePath, fixedPath)
	r := &Recall{
		Catalog: cat, MinScore: 0.001, MarginGap: 0.001,
		SoloFloor: (winnerScore + runnerScore) / 2,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e != nil {
		t.Fatalf("runner-up below the solo bar must not fire, got %q", e.Path)
	}
	if len(rejected) != 1 || rejected[0] != stalePath {
		t.Fatalf("rejected = %v, want [%q]", rejected, stalePath)
	}
}

func TestAllCandidatesDecayedReturnsAllRejected(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	r := &Recall{
		Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
			fixedPath: {Recalls: 4, Resolved: 0},
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e != nil {
		t.Fatalf("no candidate is healthy; recall must fall through, got %q", e.Path)
	}
	if len(rejected) != 2 {
		t.Fatalf("both decayed candidates must be reported, got %v", rejected)
	}
}

// twoScores returns the BM25 scores of the two seeded entries for the fixture
// query, so thresholds can be pinned between them without hardcoding corpus-
// dependent magnitudes.
func twoScores(t *testing.T, cat *catalog.Catalog, stalePath, fixedPath string) (winner, runner float64) {
	t.Helper()
	hits, err := cat.SearchScored(buildRecallQuery(fallbackReq()), recallCandidateK)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		switch h.Entry.Path {
		case stalePath:
			winner = h.Score
		case fixedPath:
			runner = h.Score
		}
	}
	if winner <= runner {
		t.Fatalf("fixture premise broken: stale (winner) score %v must exceed fixed (runner-up) %v", winner, runner)
	}
	return winner, runner
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/investigate/ -run 'RunnerUp|AllCandidatesDecayed' -v`
Expected: FAIL — compile error (`lookupWithUsage` returns 2 values, tests expect 3).

- [ ] **Step 3: Implement.** In `internal/investigate/recall.go`:

3a. Add the constant next to `recallCandidateK`:

```go
// maxOutcomeFallback bounds how many ADDITIONAL structurally-agreeing candidates
// the outcome-decay gate may consider after rejecting the winner (magnitude mode).
// Small and fixed: each fallback candidate is held to the conservative solo bar,
// so depth buys little beyond the first couple of runners-up.
const maxOutcomeFallback = 2
```

3b. Change `lookup` to drop the third return:

```go
func (r *Recall) lookup(ctx context.Context, req Request) (*catalog.Entry, float64) {
	e, conf, _ := r.lookupWithUsage(ctx, req, nil)
	return e, conf
}
```

3c. Change `lookupWithUsage`'s signature to
`(*catalog.Entry, float64, []string)` and update every `return` inside it: all existing
`return nil, 0` become `return nil, 0, nil`; the final success return becomes
`return &e, conf, nil`. Then REPLACE the Task-1 Gate-3 block with:

```go
	// Outcome decay (Gate 3), with a bounded runner-up fallback: a decayed winner
	// must not disable instant recall while a healthy corrected entry sits right
	// behind it. The winner is gated exactly as before; on a low_outcome rejection
	// the fallback (outcomeFallback) considers a few further candidates under the
	// SAME gate. The third return lists every outcome-rejected path so the caller's
	// near-miss lead can exclude them — an entry the gate just rejected must not
	// resurface as a "possibly-related lead".
	if r.Outcome != nil {
		counts, cerr := r.Outcome.OpenCounts()
		if cerr != nil {
			if r.Log != nil {
				r.Log.Warn("recall: outcome stats unavailable; skipping decay", "err", cerr)
			}
		} else {
			f, ok := r.outcomeGate(counts, e.Path)
			if !ok {
				r.reject(ctx, "low_outcome")
				return r.outcomeFallback(ctx, req, agreeing, counts, minScore, soloFloor, totals, []string{e.Path})
			}
			conf = clampF(conf*f, 0, 0.90)
		}
	}
```

3d. Add `outcomeFallback` below `lookupWithUsage` (magnitude branch now; Task 3 adds the
reranker branch in the marked spot):

```go
// outcomeFallback runs after the fire-gate winner was rejected by outcome decay
// (Gate 3). It considers a BOUNDED number of further structurally-agreeing
// candidates, each subject to the same outcome gate, and returns the first healthy
// one — or (nil, 0, rejected) so the caller falls through to a full investigation
// with the rejected paths reported. Magnitude mode holds every fallback candidate
// to the CONSERVATIVE solo bar (soloFloor AND minScore): the margin-over-runner-up
// argument justified only the original winner, so a skipped-winner candidate gets
// the same bar as a lone hit. Candidates arrive in lexical order, so the first one
// below the bar ends the walk.
func (r *Recall) outcomeFallback(ctx context.Context, req Request, agreeing []catalog.ScoredEntry, counts map[string]outcome.Aggregate, minScore, soloFloor float64, totals *providers.UsageTotals, rejected []string) (*catalog.Entry, float64, []string) {
	if r.Rerank != nil {
		// Task 3 (reranker fallback) replaces this: without it, reranker mode keeps
		// today's behavior — rejection falls through with no fallback.
		return nil, 0, rejected
	}
	limit := min(len(agreeing), 1+maxOutcomeFallback)
	for _, cand := range agreeing[1:limit] {
		if cand.Score < soloFloor || cand.Score < minScore {
			break // lexical order: nothing further clears the conservative bar
		}
		f, ok := r.outcomeGate(counts, cand.Entry.Path)
		if !ok {
			r.reject(ctx, "low_outcome")
			rejected = append(rejected, cand.Entry.Path)
			continue
		}
		strength := entryAgrees(req.Workload, cand.Entry, r.RequireWorkloadMatch)
		// margin 0: a fallback candidate gets no decisive-winner bonus.
		conf := clampF(deriveRecallConfidence(cand.Score, 0, strength)*f, 0, 0.90)
		if r.Log != nil {
			r.Log.Info("instant recall runner-up fired after outcome rejection",
				"alert", req.Title, "entry_id", cand.Entry.Path, "rejected", rejected, "confidence", conf)
		}
		e := cand.Entry
		return &e, conf, rejected
	}
	return nil, 0, rejected
}
```

3e. Fix the ONE existing caller with the old arity: `internal/investigate/loop.go:608`
`entry, conf := li.Recall.lookupWithUsage(ctx, req, verifyTotals)` →
`entry, conf, _ := li.Recall.lookupWithUsage(ctx, req, verifyTotals)` (Task 4 uses the value;
the blank keeps this task compiling).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/investigate/ -v`
Expected: the three new tests PASS; every pre-existing test PASSES unchanged (the fallback
only runs after a rejection that previously returned nil).

- [ ] **Step 5: Full gate + commit**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: clean, `0 issues`.

```bash
git add internal/investigate/recall.go internal/investigate/loop.go internal/investigate/recall_fallback_test.go
git commit -m "feat(recall): bounded runner-up fallback after outcome-decay rejection"
```

---

### Task 3: Reranker-mode fallback (one bounded re-rank call)

**Files:**
- Modify: `internal/investigate/recall.go` — fill the reranker branch of `outcomeFallback`
- Test: `internal/investigate/recall_fallback_test.go`

**Interfaces:**
- Consumes: `outcomeFallback` skeleton from Task 2; `Reranker.rank` (`rerank.go:132`) unchanged.

- [ ] **Step 1: Write the failing test.** `rank()` names ONE candidate per call, so the
fallback is a second, final `rank` call over the remaining candidates. Append to
`recall_fallback_test.go` (the fake model mirrors the scripted-response style of
`rerank_test.go` — check that file's fake first and reuse it if one already supports
scripted multi-call responses; otherwise add):

```go
// scriptedRerankModel returns each queued rerank_match verdict in order and
// counts calls — the fallback contract is "at most ONE extra rank call".
type scriptedRerankModel struct {
	calls    int
	verdicts []string // raw rerank_match JSON args, consumed in order
}

func (m *scriptedRerankModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	i := m.calls
	m.calls++
	if i >= len(m.verdicts) {
		return providers.CompletionResponse{}, nil // no tool call ⇒ no match
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: rerankToolName, Args: m.verdicts[i]}}}, nil
}

func TestRerankFallbackReranksOnceWithoutRejected(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	model := &scriptedRerankModel{verdicts: []string{
		`{"match":true,"entry_id":"` + stalePath + `","confidence":0.9}`,
		`{"match":true,"entry_id":"` + fixedPath + `","confidence":0.85}`,
	}}
	r := &Recall{
		Catalog: cat,
		Rerank:  &Reranker{Model: model, Threshold: 0.7, K: 5, MinScore: 0.001},
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
			fixedPath: {Recalls: 3, Resolved: 3},
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, conf, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e == nil || e.Path != fixedPath {
		t.Fatalf("re-rank fallback must fire the healthy candidate %q, got %+v (rejected=%v)", fixedPath, e, rejected)
	}
	if model.calls != 2 {
		t.Fatalf("exactly one extra rank call allowed, model saw %d calls", model.calls)
	}
	if conf <= 0 || conf > 0.90 {
		t.Fatalf("confidence %v out of (0, 0.90]", conf)
	}
	if len(rejected) != 1 || rejected[0] != stalePath {
		t.Fatalf("rejected = %v, want [%q]", rejected, stalePath)
	}
}

func TestRerankFallbackStopsAfterSecondRejection(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	model := &scriptedRerankModel{verdicts: []string{
		`{"match":true,"entry_id":"` + stalePath + `","confidence":0.9}`,
		`{"match":true,"entry_id":"` + fixedPath + `","confidence":0.85}`,
	}}
	r := &Recall{
		Catalog: cat,
		Rerank:  &Reranker{Model: model, Threshold: 0.7, K: 5, MinScore: 0.001},
		Outcome: fakeOutcome{counts: map[string]outcome.Aggregate{
			stalePath: {Recalls: 4, Resolved: 0},
			fixedPath: {Recalls: 4, Resolved: 0}, // second match also decayed
		}},
		OutcomePrior: 2.0, OutcomeFloor: 0.5,
	}
	e, _, rejected := r.lookupWithUsage(context.Background(), fallbackReq(), nil)
	if e != nil {
		t.Fatalf("second outcome rejection must end the fallback (no third call), got %q", e.Path)
	}
	if model.calls != 2 {
		t.Fatalf("bounded at 2 rank calls total, model saw %d", model.calls)
	}
	if len(rejected) != 2 {
		t.Fatalf("both rejected paths must be reported, got %v", rejected)
	}
}
```

(If `providers.CompletionResponse`/`ToolCall` field names differ, mirror whatever
`rerank_test.go`'s existing fake uses — that file is the source of truth for the fake shape.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/investigate/ -run 'RerankFallback' -v`
Expected: FAIL — first test gets nil (reranker branch currently returns `nil, 0, rejected`).

- [ ] **Step 3: Implement** — replace the `if r.Rerank != nil { ... }` stub inside
`outcomeFallback` with:

```go
	if r.Rerank != nil {
		// The rank verdict names ONE candidate, so a fallback needs a second — and
		// FINAL — rank call over the remaining candidates. Bounded by construction:
		// low_outcome rejections are rare and each call is ~1-2k tokens on the
		// verify tier. The same cost guard as the first call applies.
		remaining := make([]catalog.ScoredEntry, 0, len(agreeing))
		for _, cand := range agreeing {
			if !slices.Contains(rejected, cand.Entry.Path) {
				remaining = append(remaining, cand)
			}
		}
		if len(remaining) == 0 || remaining[0].Score < r.Rerank.MinScore {
			return nil, 0, rejected
		}
		k := r.Rerank.K
		if k <= 0 || k > len(remaining) {
			k = len(remaining)
		}
		matched, mconf, ok := r.Rerank.rank(ctx, req, remaining[:k], totals)
		if !ok || mconf < r.Rerank.Threshold {
			r.reject(ctx, "rerank_low_confidence")
			return nil, 0, rejected
		}
		f, ok := r.outcomeGate(counts, matched.Path)
		if !ok {
			r.reject(ctx, "low_outcome")
			return nil, 0, append(rejected, matched.Path)
		}
		conf := clampF(clampF(mconf, 0, 0.90)*f, 0, 0.90)
		if r.Log != nil {
			r.Log.Info("instant recall runner-up fired after outcome rejection (re-rank)",
				"alert", req.Title, "entry_id", matched.Path, "rejected", rejected, "confidence", conf)
		}
		return &matched, conf, rejected
	}
```

Add `"slices"` to the imports.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/investigate/ -v`
Expected: all PASS, including every pre-existing rerank test (they never hit the fallback:
their single candidate either passes the gate or the ledger is absent).

- [ ] **Step 5: Full gate + commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_fallback_test.go
git commit -m "feat(recall): one bounded re-rank fallback call in reranker mode"
```

---

### Task 4: Exclude outcome-rejected paths from the near-miss lead

**Files:**
- Modify: `internal/investigate/recall.go:273-306` — variadic `nearMissExcluding`
- Modify: `internal/investigate/loop.go:602-688` — `tryRecall` threads the rejected list
- Test: `internal/investigate/recall_fallback_test.go`

**Interfaces:**
- Consumes: the `[]string` third return from Task 2.
- Produces: `func (r *Recall) nearMissExcluding(ctx context.Context, req Request, exclude ...string) *catalog.Entry` (variadic — the existing single-string call sites compile unchanged).

- [ ] **Step 1: Write the failing test** (append to `recall_fallback_test.go`):

```go
func TestNearMissExcludesOutcomeRejectedPaths(t *testing.T) {
	cat, stalePath, fixedPath := twoEntryCatalog(t)
	r := &Recall{Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001}

	// Excluding the top candidate surfaces the next agreeing one…
	if nm := r.nearMissExcluding(context.Background(), fallbackReq(), stalePath); nm == nil || nm.Path != fixedPath {
		t.Fatalf("near-miss with winner excluded = %+v, want %q", nm, fixedPath)
	}
	// …and excluding both surfaces nothing.
	if nm := r.nearMissExcluding(context.Background(), fallbackReq(), stalePath, fixedPath); nm != nil {
		t.Fatalf("near-miss with all candidates excluded must be nil, got %q", nm.Path)
	}
	// Zero exclusions (the nearMiss() path) still returns the top candidate.
	if nm := r.nearMissExcluding(context.Background(), fallbackReq()); nm == nil {
		t.Fatal("near-miss with no exclusions must return the top agreeing candidate")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/investigate/ -run NearMissExcludes -v`
Expected: FAIL — compile error (`nearMissExcluding` takes one string, not variadic).

- [ ] **Step 3: Implement.**

3a. In `recall.go`, make the parameter variadic and skip by set (update the doc comment to
say the verify-rejection path passes the refuted entry AND the outcome-decay path passes
every path Gate 3 rejected):

```go
func (r *Recall) nearMissExcluding(ctx context.Context, req Request, exclude ...string) *catalog.Entry {
	if r == nil || r.Catalog == nil {
		return nil
	}
	query := buildRecallQuery(req)
	var hits []catalog.ScoredEntry
	var err error
	if r.Hybrid != nil && r.Hybrid.HasVectors() {
		hits, err = r.Hybrid.SearchHybrid(ctx, query, recallCandidateK)
	} else {
		hits, err = r.Catalog.SearchScored(query, recallCandidateK)
	}
	if err != nil || len(hits) == 0 {
		return nil
	}
	skip := make(map[string]bool, len(exclude))
	for _, p := range exclude {
		if p != "" {
			skip[p] = true
		}
	}
	for _, h := range hits {
		if skip[h.Entry.Path] {
			continue
		}
		if nearMissEntryAgrees(req.Workload, h.Entry, r.RequireWorkloadMatch) != matchNone {
			e := h.Entry
			return &e
		}
	}
	return nil
}
```

`nearMiss` (`recall.go:273-275`) becomes `return r.nearMissExcluding(ctx, req)`.

3b. In `loop.go` `tryRecall`: use the third return and thread it into BOTH near-miss sites:

```go
	entry, conf, outcomeRejected := li.Recall.lookupWithUsage(ctx, req, verifyTotals)
	if entry == nil {
		li.emitRecall(RecallDecision{})
		// C2 near-miss — excluding every path the outcome gate just rejected: a
		// decayed entry must not resurface as a "possibly-related lead".
		nearMiss = li.Recall.nearMissExcluding(ctx, req, outcomeRejected...)
```

and the verify-rejection site (`loop.go:682`):

```go
	nearMiss = li.Recall.nearMissExcluding(ctx, req, append(outcomeRejected, entry.Path)...)
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/investigate/ -v && go test -race ./internal/investigate/`
Expected: all PASS (including `TestTryRecallNearMissOnNonFire` — no exclusions in its
fixture, behavior unchanged).

- [ ] **Step 5: Full gate + commit**

```bash
git add internal/investigate/recall.go internal/investigate/loop.go internal/investigate/recall_fallback_test.go
git commit -m "feat(recall): near-miss lead excludes outcome-rejected entries"
```

---

### Task 5: Documentation + final verification

**Files:**
- Modify: `docs/learning-loop.md` — outcome-decay section (search for "low_outcome" or "outcome decay")

- [ ] **Step 1: Document the fallback.** In the outcome-decay section of
`docs/learning-loop.md`, after the paragraph describing rejection, add:

```markdown
A `low_outcome` rejection does not abandon recall outright: the gate walks a small,
bounded set of further structurally-agreeing candidates (the runner-up fallback) —
each held to the conservative solo bar (or, with the reranker, chosen by one final
re-rank call over the remaining candidates) and to the same outcome gate. Only when
every candidate is decayed does recall fall through to a full investigation, and the
rejected entries are also excluded from the near-miss lead, so a decayed entry can
neither answer nor steer the fresh investigation that replaces it.
```

- [ ] **Step 2: Self-review the diff against the fail-safe invariant**

Run: `git diff main --stat && go test ./internal/investigate/ -count=1`
Check: no fallback path fires an entry that failed `outcomeGate`; every terminal
rejection returns `nil, 0, rejected`.

- [ ] **Step 3: Full gate one last time**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./... && go test -race ./internal/investigate/`
Expected: clean, `0 issues`.

- [ ] **Step 4: Commit**

```bash
git add docs/learning-loop.md
git commit -m "docs(learning-loop): describe the recall runner-up fallback"
```
