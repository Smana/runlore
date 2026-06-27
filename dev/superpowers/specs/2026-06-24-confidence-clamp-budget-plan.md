# R18 plan — confidence clamp + budget estimate

Spec: `2026-06-24-confidence-clamp-budget-design.md`

## Steps (each gated + committed)

1. **Spec + plan** (this) — commit.
2. **clamp01 + parseFindings/applyVerdicts** (test-first)
   - Write failing tests: `tools_test.go` (overall + per-cause clamp inc. NaN), `verify_test.go` (verdict clamp keep/downgrade inc. NaN).
   - Add `clamp01` (new tiny file `clamp.go` or top of tools.go) using `math`.
   - Apply in `parseFindings` (tools.go) and `applyVerdicts` (verify.go).
   - Gate green, commit.
3. **estimateTokens tool-call args + tool-spec count** (test-first)
   - Update `budget_test.go` `TestEstimateTokens` to new signature; add a case asserting tool-call args + tool-spec bytes are counted (and exceed old content-only sum).
   - Change `estimateTokens` signature + body in `budget.go`; update both call sites in `loop.go` to pass `specs`.
   - Gate green, commit.

## Gate (before each commit)

```
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```

`golangci-lint` must report `0 issues` (gosec enabled).
