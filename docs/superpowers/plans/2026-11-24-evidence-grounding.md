# Evidence Grounding + Causal Confidence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface a structured `EvidenceGrounding` signal (`traced / total` cited evidence bullets + exact unverified indices) alongside RunLore's existing scalar causal `Confidence`, computed inside the existing adversarial verify pass at zero extra LLM cost and surfaced across Slack/Matrix/CLI cards, KB PR drafts, Prometheus metrics, and `lore eval`.

**Architecture:** Add `providers.EvidenceGrounding` onto `Hypothesis` and `Investigation`. Number evidence bullets in `verify.go`'s `renderForReview` and extend `submit_verdicts` with optional `ungrounded_evidence: []int`; validate, deduplicate, and clamp indices server-side against `len(rc.Evidence)` (only when a non-empty tool transcript was present). Render a compact inline badge next to confidence and `(unverified)` tags on ungrounded bullets; record `runlore_investigation_grounding_ratio` and add informational grounding reporting + optional `min_grounding` to `internal/eval`.

**Tech Stack:** Go 1.26, OpenTelemetry metrics, `internal/notify`, `internal/eval`.

**Spec:** [`docs/superpowers/specs/2026-11-24-evidence-grounding-design.md`](../specs/2026-11-24-evidence-grounding-design.md)

## Global Constraints

- **Zero extra LLM calls.** Grounding rides on the existing `verifyFindings` completion.
- **No gate changes on `inv.Confidence`.** `curator.MeetsBar`, `action.Policy`, and existing `eval.Score` cases (`min_grounding == 0`) keep exact current pass/fail behavior.
- **Unmeasured stays `nil`.** When verify is skipped/fails, `len(rc.Evidence) == 0`, or `transcriptExcerpt == ""` (e.g. unconfirmed recall), leave `Grounding == nil` and render nothing.
- **Keep `internal/docsguard/metric_names_test.go` green.** Document `runlore_investigation_grounding_ratio` in `website/content/docs/operations/observability.md`.
- **Quality gate before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/providers.go` (modify) | Add `EvidenceGrounding` struct + `Ratio()` + `IsUngrounded(idx int) bool`; add `Grounding *EvidenceGrounding` to `Hypothesis` and `Investigation` |
| `internal/investigate/verify.go`, `verify_test.go` (modify) | Number evidence bullets in `renderForReview`; add `ungrounded_evidence` to `submitVerdictsSpec`/`verdict`; clamp/dedup/rollup `Grounding` in `applyVerdicts` gated on `hasTranscript` |
| `internal/investigate/loop.go` (modify) | Record `InvestigationGroundingRatio` telemetry once per completed investigation when `inv.Grounding != nil` |
| `internal/telemetry/{metrics.go,setup.go}` (modify) | Register `runlore_investigation_grounding_ratio` Float64Histogram with explicit `[0, 0.25, 0.5, 0.75, 0.9, 1.0]` buckets |
| `internal/notify/{format.go,slack.go,matrix.go}` + tests (modify) | Add `groundingBadge`; append inline badge to confidence line; tag ungrounded bullets `(unverified)` in CLI, Slack thread detail, and Matrix detail |
| `internal/curator/curator.go`, `curator_test.go` (modify) | Tag ungrounded evidence bullets `*(unverified)*` in drafted KB PR markdown |
| `internal/eval/{case.go,score.go,replay_report.go,compare_run.go,compare_report.go}` + tests (modify) | Record grounding on `eval.Result`, support optional `Expected.MinGrounding`, display median grounding in replay & `--compare` reports |
| `website/content/docs/operations/observability.md`, `deploy/observability/` (modify) | Document `runlore_investigation_grounding_ratio` and add Grafana panel |

---

## Task 1: `providers.EvidenceGrounding` and `verify.go` grounding extraction

**Files:**
- Modify: `internal/providers/providers.go`
- Modify: `internal/investigate/verify.go`
- Modify: `internal/investigate/verify_test.go`

- [ ] **Step 1: Write failing tests in `internal/investigate/verify_test.go`**

Add table-driven tests covering:
- `renderForReview` numbers evidence bullets `[0]`, `[1]`, `[2]`.
- `verifyFindings` with transcript + `ungrounded_evidence: []` → `rc.Grounding = {Traced: 2, Total: 2}`, `inv.Grounding = {Traced: 2, Total: 2}`.
- `verifyFindings` with transcript + `ungrounded_evidence: [1, 1, 99, -2]` on 3 bullets → deduplicated/clamped to `UngroundedIndices: [1]`, `Traced: 2, Total: 3`.
- `verifyFindings` with `transcript == nil` (no tool transcript) → `rc.Grounding == nil` and `inv.Grounding == nil` even if model returned `ungrounded_evidence`.
- Multi-hypothesis rollup: hypothesis 0 kept (`2/2`), hypothesis 1 downgraded (`1/2`), hypothesis 2 rejected → `inv.Grounding = {Traced: 3, Total: 4}`.
- Unrecognized verdict or missing verdict index → `rc.Grounding == nil` (not fabricated).

- [ ] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/investigate/... -run TestVerifyGrounding
```

Expected: FAIL (compile error — `EvidenceGrounding` doesn't exist yet).

- [ ] **Step 3: Implement `EvidenceGrounding` and `verify.go` updates**

- In `internal/providers/providers.go`, define `EvidenceGrounding` with `Ratio() float64` and `IsUngrounded(i int) bool` helpers, and add `Grounding *EvidenceGrounding` to `Hypothesis` and `Investigation`.
- In `internal/investigate/verify.go`:
  - Update `verifyPrompt` to explain `ungrounded_evidence` (0-based indices of bullets not traced to the transcript excerpt).
  - Add optional `ungrounded_evidence` array of integers to `submitVerdictsSpec()` and `UngroundedEvidence []int` (`json:"ungrounded_evidence"`) to `verdict`.
  - Number evidence lines `[%d] %s` in `renderForReview`.
  - Pass `hasTranscript := transcriptExcerpt(transcript) != ""` into `applyVerdicts`.
  - For each kept/downgraded root cause with `ok && hasTranscript && len(rc.Evidence) > 0`, compute `clampUngrounded(v.UngroundedEvidence, len(rc.Evidence))` and set `rc.Grounding`, then roll up across surviving `kept` hypotheses onto `inv.Grounding`.

- [ ] **Step 4: Run tests with race detector**

```bash
go test -race ./internal/investigate/... ./internal/providers/...
```

Expected: PASS.

---

## Task 2: Surface grounding in CLI, Slack, Matrix, and KB PR drafts

**Files:**
- Modify: `internal/notify/format.go`, `format_test.go`
- Modify: `internal/notify/slack.go`, `slack_test.go`, `card_golden_test.go`
- Modify: `internal/notify/matrix.go`, `matrix_test.go`
- Modify: `internal/curator/curator.go`, `curator_test.go`

- [ ] **Step 1: Write failing tests for rendering**

- `format_test.go`: `groundingBadge` returns `""` for nil/0-total, `"3/3 evidence traced"` for full, `"2/3 evidence traced — 1 unverified"` for partial; `Format(inv)` appends inline badge to confidence line and `(unverified)` to ungrounded evidence bullets.
- `slack_test.go` & `matrix_test.go`: main card confidence block appends ` · 2/3 evidence traced — 1 unverified` when `inv.Grounding != nil`, leaves existing cards with `Grounding == nil` byte-identical; detail thread marks ungrounded bullet `*(unverified)*`.
- `curator_test.go`: drafted KB PR body marks ungrounded bullets `*(unverified)*` and omits annotations when `Grounding == nil`.

- [ ] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/notify/... ./internal/curator/... -run 'Test.*Grounding'
```

Expected: FAIL.

- [ ] **Step 3: Implement rendering helpers and detail markers**

- Implement `groundingBadge(inv providers.Investigation) string` in `internal/notify/format.go` (shared by `Format`, `slack.go`, `matrix.go` so all three render identical copy).
- Append `(unverified)` / `*(unverified)*` wherever `rc.Grounding.IsUngrounded(j)` is true in `format.go`, `slack.go` thread reply, `matrix.go` detail, and `curator.go` entry markdown.

- [ ] **Step 4: Run notify & curator tests (including golden tests)**

```bash
go test -race ./internal/notify/... ./internal/curator/...
```

Expected: PASS.

---

## Task 3: Telemetry & `internal/eval` integration

**Files:**
- Modify: `internal/telemetry/metrics.go`, `internal/telemetry/setup.go`
- Modify: `internal/investigate/loop.go`
- Modify: `internal/eval/{case.go,score.go,replay_report.go,compare_run.go,compare_report.go}` + tests
- Modify: `website/content/docs/operations/observability.md`, `deploy/observability/`

- [ ] **Step 1: Write failing tests for metrics and eval**

- `internal/eval/score_test.go`:
  - `Score` populates `GroundingTraced`/`GroundingTotal` from `inv.Grounding`.
  - Default `MinGrounding: 0` leaves `Pass` unaffected even when `inv.Grounding` is `nil` or `0/2`.
  - `MinGrounding: 0.75` passes on `3/4` (0.75), fails on `2/4` (0.50) with a clear `Missing` message (`grounding 0.50 < 0.75`), and fails when unmeasured (`GroundingTotal == 0`).
- `internal/eval/replay_report_test.go` & `compare_report_test.go`: report includes median grounding column (`—` when unmeasured).
- `internal/investigate/loop_test.go`: completed verified investigation records `runlore_investigation_grounding_ratio` when `inv.Grounding != nil`.

- [ ] **Step 2: Run targeted tests to verify failure**

```bash
go test ./internal/eval/... ./internal/telemetry/... ./internal/investigate/... -run 'Test.*Grounding'
```

Expected: FAIL.

- [ ] **Step 3: Implement telemetry, eval, and observability docs**

- Add `InvestigationGroundingRatio` (`runlore_investigation_grounding_ratio`) in `internal/telemetry/metrics.go` + explicit histogram view buckets `[0, 0.25, 0.5, 0.75, 0.9, 1.0]` in `setup.go`.
- Record it in `internal/investigate/loop.go` on delivery when `inv.Grounding != nil && inv.Grounding.Total > 0`.
- Wire `GroundingTraced`, `GroundingTotal`, `MinGrounding`, and report columns across `internal/eval/`.
- Document `runlore_investigation_grounding_ratio` in `website/content/docs/operations/observability.md` and add a Grafana panel in `deploy/observability/`.

- [ ] **Step 4: Run full quality gate**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

Expected: all tests (including `internal/docsguard/metric_names_test.go`) PASS, `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.

- [ ] **Step 5: Commit**

Commit using Conventional Commits (English, no co-author line), e.g.:
`feat(investigate): surface evidence grounding alongside causal confidence`
