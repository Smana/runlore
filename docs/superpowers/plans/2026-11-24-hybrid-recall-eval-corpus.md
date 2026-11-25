# Hybrid Recall Graduation (or Cut), Eval Corpus Expansion & `#394` Implementation Plan

> **Status:** Partially implemented (PR #633, merged 2026-11-24 — Tasks 1–3 merged; Task 4 live embedding benchmark pending)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close `#394` (vacuous `--compare` columns when `ground_truth` is absent), wire hybrid recall into `internal/eval`, expand `examples/eval/` from 7 to 17 hermetic replay cases (including paraphrase recall and a second held-out case), and execute the evidence-gated live benchmark to either graduate or deprecate/cut `catalog.instant_recall.hybrid`.

**Architecture:** Fix `compare_run.go`/`compare_report.go` to track measured denominators (`JudgedCount`, `CoverageCount`) and omit or render `—` for unmeasured columns. Wire `embed.Client` + `Catalog.SetEmbedder` inside `internal/eval/eval.go`. Add 10 compact hermetic YAML replay fixtures under `examples/eval/` and lexical-mismatch test cases in `hybrideval_test.go`. Run `TestHybridRecallEvalLive` against one self-hosted open-weights model and one hosted baseline, then execute the documented Graduate or Cut branch.

**Tech Stack:** Go 1.26, `internal/eval`, `internal/catalog`, `internal/embed`, YAML eval fixtures.

**Spec:** [`docs/superpowers/specs/2026-11-24-hybrid-recall-eval-corpus-design.md`](../specs/2026-11-24-hybrid-recall-eval-corpus-design.md)

## Global Constraints

- **Never tune `systemPrompt` against held-out cases.** Keep held-out cases strictly blind.
- **Zero live-cluster dependencies in `examples/eval/`.** All replay cases run offline via recorded tool outputs.
- **No graduating on vibes.** Graduation requires meeting all published criteria in `website/content/docs/configuration/configuration.md` with a recorded live report under `eval/reports/`.
- **Check nightly CI budget** when adding 10 replay cases (confirm `-n` in `.github/workflows/eval.yaml` stays within spend bounds).
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/eval/compare_run.go`, `compare_report.go` + tests (modify) | Track `JudgedCases` / `CoverageCases` denominators; omit vacuous columns when `0`, render `—` / `k/N` on mixed suites (closes `#394`) |
| `internal/eval/eval.go`, `eval_test.go` (modify) | Wire `model.embeddings` + `catalog.instant_recall.hybrid` into replay runner; account embedding tokens |
| `examples/eval/*.yaml` + `examples/eval/fixtures/` (create) | Add 10 new hermetic replay cases covering DNS, network drop, PVC, cert expiry, Argo CD, AWS CloudTrail, correlation-only inconclusive, 2 paraphrase recall cases, and 1 new held-out case |
| `internal/investigate/hybrideval_test.go` (modify) | Add lexical-mismatch / paraphrase positive pairs and hard-negative pairs; print BM25 vs Hybrid side-by-side summary in `TestHybridRecallEvalLive` |
| `eval/reports/YYYY-MM-DD-hybrid-recall-benchmark.md` (create) | Recorded live benchmark results across self-hosted open-weights + hosted embedding baselines |
| `internal/config/config.go`, `website/content/docs/configuration/configuration.md`, `ROADMAP.md` (modify) | Execute either Graduate (remove `EXPERIMENTAL`, set measured defaults, record model+date) or Deprecate/Cut branch; remove `#394` and hybrid graduation from `ROADMAP.md` |

---

## Task 1: Fix `#394` — stop `lore eval --compare` publishing vacuous columns

**Files:**
- Modify: `internal/eval/compare_run.go`
- Modify: `internal/eval/compare_report.go`
- Modify: `internal/eval/compare_report_test.go` (or create if absent)

- [x] **Step 1: Write failing tests in `internal/eval/compare_report_test.go`**

Test three suites:
1. All cases lack `ground_truth` → rendered markdown/table omits Judge and Coverage columns completely and prints a footnote explaining why; JSON includes `judged_cases: 0`, `coverage_cases: 0`.
2. Mixed suite (2 of 5 cases have `ground_truth`) → columns render with explicit denominator or `—` when a model had 0 judged cases (never `0.0%` for unmeasured).
3. Full `ground_truth` suite → renders identically to today.

- [x] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/eval/... -run TestCompareReport
```

Expected: FAIL.

- [x] **Step 3: Implement `JudgedCases` / `CoverageCases` tracking and conditional column rendering**

Update `compare_run.go` to count cases where judge/coverage actually ran, and update `compare_report.go` to hide vacuous columns and display `—` / explicit denominators.

- [x] **Step 4: Run `internal/eval` tests**

```bash
go test -race ./internal/eval/...
```

Expected: PASS.

---

## Task 2: Wire hybrid recall into `internal/eval` and add paraphrase retrieval fixtures

**Files:**
- Modify: `internal/eval/eval.go`, `internal/eval/eval_test.go`
- Modify: `internal/investigate/hybrideval_test.go`

- [x] **Step 1: Write failing test in `internal/eval/eval_test.go` for hybrid recall wiring**

Inject a deterministic fake `catalog.Embedder` (distinct vectors for matching vs non-matching entries) and assert `lore eval` runs `SearchHybrid`, fires instant recall on a lexical-mismatch alert when cosine ≥ threshold, and attributes embedding token usage.

- [x] **Step 2: Run targeted test to verify failure**

```bash
go test ./internal/eval/... -run TestEvalHybridRecall
```

Expected: FAIL.

- [x] **Step 3: Wire embedder & hybrid thresholds in `internal/eval/eval.go` and expand `hybrideval_test.go`**

- In `internal/eval/eval.go`, wire `embed.New(...)` when `cfg.Catalog.InstantRecall.Hybrid` and `cfg.Model.Embeddings` are configured, set `cat.SetEmbedder(...)`, and populate `recall.Hybrid`, `HybridMinScore`, `HybridMarginGap`.
- In `internal/investigate/hybrideval_test.go`, add 4–6 lexical-mismatch/paraphrase positive cases (e.g. synonym wording for OOM, DNS, TLS expiry, PVC pending) and 2 hard-negative cases (same workload, different failure mechanism); emit a side-by-side BM25 vs Hybrid Recall@1 + false-fire table from `TestHybridRecallEvalLive`.

- [x] **Step 4: Run hermetic tests**

```bash
go test -race ./internal/eval/... ./internal/investigate/... -run 'Test.*Hybrid|TestEval'
```

Expected: PASS.

---

## Task 3: Add 10 new hermetic replay cases to `examples/eval/`

**Files:**
- Create: `examples/eval/dns-coredns-servfail.yaml`
- Create: `examples/eval/network-cilium-policy-drop.yaml`
- Create: `examples/eval/pvc-storageclass-unbound.yaml`
- Create: `examples/eval/cert-manager-expiry.yaml`
- Create: `examples/eval/argocd-degraded-sync.yaml`
- Create: `examples/eval/aws-cloudtrail-sg-change.yaml`
- Create: `examples/eval/correlation-only-inconclusive.yaml`
- Create: `examples/eval/recall-paraphrase-oom.yaml`
- Create: `examples/eval/recall-paraphrase-dns.yaml`
- Create: `examples/eval/heldout-statefulset-wal-lock.yaml`
- Create: matching KB fixture entries under `examples/eval/fixtures/` for the two paraphrase recall cases

- [x] **Step 1: Author the 10 compact, realistic replay YAML cases**

Ensure each case:
- Uses realistic tool output shapes matching RunLore's actual tool formatting (never hands the model its own answer in the alert text — remember #585).
- Uses `must_contain`, `root_cause_entities`, `distractors`, and `min_confidence` (or `max_confidence` / `expect_inconclusive` for `correlation-only-inconclusive.yaml`; `expect_recall: true` for paraphrase recall cases).
- Marks `heldout-statefulset-wal-lock.yaml` clearly in comments as held-out.

- [x] **Step 2: Add/run hermetic loader & schema validation test across all `examples/eval/*.yaml`**

```bash
go test -race ./internal/eval/...
```

Expected: all 17 cases parse and validate cleanly.

---

## Task 4: Run live embedding benchmark and execute Graduate-or-Cut decision

**Files:**
- Create: `eval/reports/YYYY-MM-DD-hybrid-recall-benchmark.md`
- Modify: `internal/config/config.go`, `website/content/docs/configuration/configuration.md`, `ROADMAP.md` (and if Cut: deprecate/remove `hybrid.go`, `veccache.go`, `internal/embed`)

- [ ] **Step 1: Run `TestHybridRecallEvalLive` on self-hosted + hosted embedding models**

```bash
RUNLORE_LIVE_EMBED_URL=<self-hosted-vllm-or-ollama> RUNLORE_LIVE_EMBED_MODEL=<bge-m3|nomic-embed-text> \
  go test -v ./internal/investigate/... -run TestHybridRecallEvalLive
```

Repeat against the hosted baseline (`text-embedding-3-small` or `gemini-embedding-001` OpenAI-compatible endpoint). Record per-case cosine distributions, BM25 Recall@1, Hybrid Recall@1, negative fire count, and recommended thresholds in `eval/reports/YYYY-MM-DD-hybrid-recall-benchmark.md`.

- [ ] **Step 2: Execute Graduate or Cut branch strictly on the measured numbers**

- **If Criteria 1–3 pass:** remove `EXPERIMENTAL` markers from `internal/config/config.go` and `configuration.md`, update default thresholds if measured values differ, record model + date + report link in `configuration.md`, and remove hybrid graduation + `#394` from `ROADMAP.md`.
- **If Criteria fail (or BM25+reranker matches/beats hybrid even on paraphrase cases):** publish the negative result in `eval/reports/`, deprecate `catalog.instant_recall.hybrid` (or remove `hybrid.go`, `veccache.go`, `internal/embed` in a dedicated breaking minor PR), document why in `configuration.md`, and remove hybrid graduation + `#394` from `ROADMAP.md`.

- [ ] **Step 3: Review nightly CI eval budget (`.github/workflows/eval.yaml`) and run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests PASS, `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [ ] **Step 4: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(eval): expand replay corpus to 17 cases, close #394, and measure hybrid recall`
