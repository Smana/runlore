# R23 — Poisoned-entry scenario + readyz-with-no-KB — plan

Spec: `docs/superpowers/specs/2026-06-24-poison-scenario-ready-gate-design.md`.
Base: worktree fast-forwarded to `main` (`d06ad6e`, includes R2).

## Commit 1 — readyz: a configured-but-failed catalog stays 503 (Part 1)

Test-first (`cmd/lore/main_test.go`):
- Extend `TestReadyFunc`: `readyFunc` takes a `configured bool`.
  - configured + nil catalog (load failed) + leader=true ⇒ **not ready** (the bug).
  - configured + cold (`NewEmpty`) + leader=true ⇒ not ready.
  - configured + warm + leader=true ⇒ ready; + leader=false ⇒ not ready.
  - **unconfigured** + nil + leader=true ⇒ ready (passthrough preserved).

Impl (`cmd/lore/main.go`):
- Add `catalogConfigured(cfg) bool`.
- `readyFunc(leader, cat, configured)`: if `configured && (cat == nil || !cat.Ready())` ⇒ false; keep `cat != nil && !cat.Ready()` ⇒ false; else `leader()`.
- Call site: `readyFunc(leader.Load, cat, catalogConfigured(cfg))`.

Gate green → commit.

## Commit 2 — poisoned-entry live scenario + harness guard (Part 2)

Test-first (`internal/eval/live_test.go`):
- `TestRunScenarioPoisonedRecallReinvestigates`: an invasive scenario through `RunScenario` with a poisoned `Recall` (fake `ScoredSearcher`) + reject-then-investigate model; assert no run's investigation carries the poisoned cause and the fresh finding is delivered. Reuse existing fakes; `Steps` = a no-op recorder; `Judge` nil.

Add fixtures:
- `eval/scenarios/poisoned-recall-rejected.yaml` — invasive, `precheck: test -n "$RUNLORE_POISON_READY"`, bad-image-tag fault, ground truth = the real cause.
- `eval/scenarios/manifests/poisoned-recall-entry.md` — the poisoned OKF entry an operator seeds.

Verify the scenario parses (`LoadScenarios` via the existing loader test path) and SKIPs without the env var.

Gate green → commit.

## Gate (before EACH commit)

`go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./... && golangci-lint run --enable gosec ./...`
