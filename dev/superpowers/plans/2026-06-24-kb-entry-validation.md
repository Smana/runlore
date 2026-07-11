# KB-Entry Validation — Implementation Plan

> **For agentic workers:** execute task-by-task with TDD (test → fail → impl →
> pass → commit). Spec: `dev/superpowers/specs/2026-06-24-kb-entry-validation-design.md`.

**Goal:** A shared `internal/kbvalidate` package (deterministic structural gate +
LLM semantic advisory), a `lore validate-kb` CLI, and a `catalog.Load` strict-warn,
so KB entries are validated before merge (CI) and loudly at index time.

**Architecture:** `kbvalidate.ValidateStructural(catalog.Entry) []Issue` is pure
and deterministic (the merge gate); `ReviewSemantic(ctx, Entry, ModelProvider)`
mirrors `verify.go`'s tool-call structured-output pattern (advisory only). The
CLI and `catalog.Reload` both consume the package.

**Tech Stack:** Go, `gopkg.in/yaml.v3` (already used), `go.opentelemetry.io/otel` metrics.

## Global Constraints
- Type vocabulary: **`{Incident, Playbook, Concept}`** (confirmed against runlore-kb).
- `catalog.Entry{Type, Title, Description, Resource, Tags, Body, Path}` — no `Fingerprint`.
- Incident body requires `## Symptom`, `## Cause`, `## Resolution` (matches `curator.draftKBEntry`).
- Semantic review is ADVISORY (never a gate); degrades to `Skipped` when model is nil.
- `golangci-lint v2.12.2` clean; every task ends green (`go test ./...`).

---

### Task 1: kbvalidate core — types + ValidateStructural (frontmatter)
**Files:** Create `internal/kbvalidate/kbvalidate.go`, `internal/kbvalidate/kbvalidate_test.go`
**Produces:** `Severity` (Error|Warning), `Issue{Severity,Field,Message}`,
`ValidateStructural(catalog.Entry) []Issue`, `HasErrors([]Issue) bool`.
- [ ] Test: a valid Incident → no errors; missing/!vocab `type`, empty `title`,
  >120-char/multiline `title`, empty `description`, empty/space `resource`,
  empty `tags` (Warning) → expected Issues.
- [ ] Impl frontmatter rules; run `go test ./internal/kbvalidate/`; commit.

### Task 2: ValidateStructural — body/section checks (type-aware)
**Files:** Modify `internal/kbvalidate/kbvalidate.go`, add cases to the test.
- [ ] Test: Incident missing `## Cause` → Error; empty `## Resolution` → Error;
  missing `## Investigate` → Warning; Playbook/Concept with non-empty body → ok;
  any type with empty body → Error.
- [ ] Impl heading scan (regex `(?m)^##\s+(\w+)` + section-content check); test; commit.

### Task 3: ReviewSemantic — LLM advisory
**Files:** Create `internal/kbvalidate/semantic.go`, `semantic_test.go`
**Consumes:** `providers.ModelProvider`, `providers.CompletionRequest/ToolSpec`.
**Produces:** `Verdict{OK bool, Rationale string}`,
`Advisory{CauseExplainsSymptom, Durable Verdict, Skipped bool}`,
`ReviewSemantic(ctx, catalog.Entry, providers.ModelProvider) (Advisory, error)`.
- [ ] Test (fake ModelProvider returning a `submit_review` tool-call): transient
  entry → `Durable.OK=false`; cause-mismatch → `CauseExplainsSymptom.OK=false`;
  nil model → `Skipped=true`, no error; model error → `Skipped`, no gate failure.
- [ ] Impl: `submitReviewSpec()` ToolSpec (JSON schema), `Complete` with System +
  rendered entry + the tool, parse `resp.ToolCalls[0].Args`; test; commit.

### Task 4: `lore validate-kb` CLI
**Files:** Modify `cmd/lore/main.go` (add `case "validate-kb"`); create
`cmd/lore/validate.go`, `cmd/lore/validate_test.go`.
**Consumes:** `kbvalidate`, `catalog.Load`.
- [ ] Test: a dir of fixtures (good + each bad variant) → exit 0 on warnings-only,
  exit 1 on any Error; `--format=github` emits `::error file=…::` annotations.
- [ ] Impl `runValidateKB(args)`: flags `--semantic`, `--format`, `--config`;
  walk paths, `catalog.Load` (or parse single files), `ValidateStructural`, optional
  `ReviewSemantic` (build model from config like serve); print + exit; wire the
  `case`; test; commit.

### Task 5: catalog.Load strict-warn + metric
**Files:** Modify `internal/telemetry/metrics.go` (+`metrics_test.go`),
`internal/catalog/catalog.go` (`Reload`), `internal/catalog/catalog_test.go`.
**Produces:** `Metrics.CatalogInvalidEntries metric.Int64Counter`
(`runlore_catalog_invalid_entries_total`).
- [ ] Test: `Reload` over a dir with one structurally-invalid (but parseable)
  entry → still loads the valid ones, logs WARN, and (when a `*telemetry.Metrics`
  is wired, nil-safe) increments the counter.
- [ ] Impl: add the instrument; in `Reload`, run `ValidateStructural` per entry,
  `log.Warn` + `metrics.CatalogInvalidEntries.Add` on Error (keep current
  skip/index behaviour); test; commit.

### Task 6: GitHub Action + process doc (runlore-kb)
**Files:** (runlore-kb repo) `.github/workflows/validate-kb.yml`; (runlore)
`docs/observability.md`/README note that `validate-kb` is the KB CI gate.
- [ ] Action: on `pull_request` `**.md`, run `ghcr.io/smana/runlore` →
  `lore validate-kb --changed-only --semantic --format=github`; structural Error
  fails the check; advisory posted as a comment. (No Go test; a follow-up PR on the
  KB repo.) Commit the runlore-side doc note.

## Success criteria
SC-1 malformed entry fails the gate · SC-2 #48-standard entry passes · SC-3
advisory appears / skips cleanly w/o a key · SC-4 bad merged entry → metric+WARN ·
SC-6 `go test ./...` + lint green, `meetsBar` unchanged.
