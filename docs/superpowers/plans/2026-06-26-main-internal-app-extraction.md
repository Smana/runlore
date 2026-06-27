# `cmd/lore/main.go` → `internal/app` Extraction Plan

> **For agentic workers:** behaviour-preserving refactor. The safety net is the existing
> `go test ./...` staying green at every step; new tests are characterization tests that lock in
> the moved functions' current behaviour. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Shrink the 1615-line `cmd/lore/main.go` god-file by moving its dependency-injection
builders and helpers into a testable `internal/app` package, so the assembly that decides what
ships is unit-tested instead of sitting at ~9% coverage behind `main()`.

**Architecture:** A single `internal/app` package holds the constructors/predicates as exported
functions; `cmd/lore` keeps `main()` + the `run*` subcommand handlers (CLI orchestration is a
`cmd` concern) and calls into `app`. Done in cohesive, independently-green phases so each is a
reviewable PR. No behaviour changes — only relocation + new tests.

**Tech Stack:** Go 1.26, standard library, existing internal packages. White-box tests
(`package app`).

## Global Constraints

- Behaviour-preserving: every phase keeps `go build ./... && go vet ./... && go test ./... && gofmt -l .` (empty) `&& golangci-lint run ./...` (`0 issues`) green.
- Module path `github.com/Smana/runlore`; exported `app` symbols carry doc comments (revive).
- No `Co-Authored-By` / AI-attribution lines in commits or PRs (user policy).
- Function bodies move **verbatim** (only the package clause, exported name, and call sites change) so diffs read as relocations.

---

## Phase 1 — Foundation: model builders + config predicates (THIS PR)

Establish `internal/app` and move the leaf, low-dependency functions — the easiest, mostly-untested
units — proving the package + test pattern before the heavier builders.

**Files:**
- Create `internal/app/model.go` — `NewModelClient`, `BuildModel`, `BuildVerifyModel`, `BuildJudgeModel` (moved from `main.go:505-541,752-761`). Drops the `anthropic`/`gemini`/`openai` imports from `main.go` (used nowhere else).
- Create `internal/app/config.go` — `ModelProvider`, `ModelConfigured`, `CatalogConfigured`, `GitopsEngine`, `RequireWebhookAuth`, `OutcomeKind` (moved from `main.go:459-502,1370-1374,1559-1564`).
- Create `internal/app/runtime.go` — `PodName`, `PodNamespace`, `ReadyFunc` (moved from `main.go:437-456,1390-1400`).
- Create `internal/app/{model,config,runtime}_test.go` — characterization tests; **relocate** the existing `RequireWebhookAuth`/`ReadyFunc` cases out of `cmd/lore/main_test.go`.
- Modify `cmd/lore/main.go` — delete the moved bodies; add `internal/app` import; rewrite ~36 call sites to `app.X`.
- Modify `cmd/lore/validate.go:34-35` — `modelConfigured`→`app.ModelConfigured`, `buildModel`→`app.BuildModel`.

**Interfaces produced (signatures unchanged, now exported):**
- `app.NewModelClient(provider, baseURL, model, apiKey string) providers.ModelProvider`
- `app.BuildModel(cfg *config.Config, apiKey string) providers.ModelProvider`
- `app.BuildVerifyModel(cfg *config.Config) providers.ModelProvider`
- `app.BuildJudgeModel(cfg *config.Config, provider, baseURL, model, apiKeyEnv string) providers.ModelProvider`
- `app.ModelProvider(cfg) string` · `app.ModelConfigured(cfg) bool` · `app.CatalogConfigured(cfg) bool` · `app.GitopsEngine(cfg) string` · `app.RequireWebhookAuth(cfg, token string) error` · `app.OutcomeKind(recalled bool) string`
- `app.PodName() string` · `app.PodNamespace() string` · `app.ReadyFunc(leader func() bool, cat *catalog.Catalog, configured bool) func() bool`

- [ ] **Step 1** — Create `internal/app/{model,config,runtime}.go` with the moved bodies (verbatim, exported, doc comments preserved).
- [ ] **Step 2** — `go build ./internal/app/` to confirm the new package compiles in isolation.
- [ ] **Step 3** — Delete the moved bodies from `main.go`; add the `app` import; rewrite call sites to `app.X`; drop the now-unused `anthropic`/`gemini`/`openai` imports. Update `validate.go`.
- [ ] **Step 4** — `go build ./... && go vet ./...` — confirm the whole module still compiles.
- [ ] **Step 5** — Relocate the `RequireWebhookAuth` + `ReadyFunc` test cases from `cmd/lore/main_test.go` into `internal/app/{config,runtime}_test.go` (white-box); add characterization tests for `ModelProvider`/`ModelConfigured`/`CatalogConfigured`/`GitopsEngine`/`OutcomeKind`/`NewModelClient` provider-selection (asserts the concrete client type per provider).
- [ ] **Step 6** — `go test ./... && test -z "$(gofmt -l .)" && golangci-lint run ./...` — all green.
- [ ] **Step 7** — Commit `refactor(cmd): extract model builders + config predicates to internal/app`; push; open PR.

## Phase 2 — Builder family (follow-up PR)

Move the heavier constructors + their private helpers into `internal/app`, cohesively:
`buildCatalog`, `buildNotifier`, `buildForgeTokenSource` (+ the `forgeToken` type), `buildAuditor`,
`buildApprovals`, `buildAuto`, `buildCurator`, `buildReinvestigator`, `buildModelAndTools`,
`buildInvestigator` (+ its `OnComplete` closure), `buildFailureDebouncer`, `buildGitOps`,
`gitOpsFromKube`, `kubeClientset`, `restConfig`, `startGitOpsFailureWatch`, `newHTTPServer`,
`runLeaderElection`, `setMemoryLimitFromCgroup`. Files: `internal/app/{catalog,forge,action,curator,investigate,gitops,server,leader}.go`. Add wiring tests asserting the assembled graph (e.g. `BuildInvestigator` returns `LogInvestigator` with no model, `LoopInvestigator` with one; `BuildAuto` nil unless `auto` mode).

## Phase 3 — Subcommand orchestration (follow-up PR)

Move `runServe`/`runEval`/`runEvalLive`/`runInvestigate`/`runCurate`/`runCatalog`/`runCatalogSync`/`runMCP`
into `internal/app` as `App` methods (or `Run*` funcs) so `runServe`'s 242-line wiring becomes
testable; `cmd/lore/main.go` collapses to `main()` + flag dispatch (~60 lines). `runValidateKB`
stays in `cmd/lore` (already small, in `validate.go`).

## Self-review

- **Coverage:** Phase 1 moves every targeted function; the `anthropic`/`gemini`/`openai`-only-in-`newModelClient` fact is verified (`grep`), so the import drop is safe. `validate.go` + `main_test.go` callers are accounted for.
- **Type consistency:** exported names are the capitalized originals; signatures unchanged.
- **No behaviour change:** the existing suite is the regression guard; new tests are characterization-only.
