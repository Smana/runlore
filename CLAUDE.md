# RunLore — contributor & agent guide

RunLore is a self-improving, GitOps-native SRE agent written in Go. Start with
[`docs/design.md`](docs/design.md) for the architecture and `docs/plans/` for the implementation plans.

## Quality gate — run before every commit

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```

- `gofmt -l .` must print nothing.
- `golangci-lint run ./...` must report **`0 issues`**.
- Linter config: [`.golangci.yml`](.golangci.yml) (golangci-lint **v2**). CI runs the same gate
  ([`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)).

## Conventions

- **TDD.** Write the failing test first, then the minimal implementation. Prefer table-driven tests.
  Tests must verify behaviour, not mocks.
- **Errors.** Wrap with `%w`; compare with `errors.Is` / `errors.As` (enforced by `errorlint`).
- **Context.** `context.Context` is the first parameter of any function that does I/O.
- **Exported symbols carry doc comments** (enforced by `revive`).
- **Small, focused files** — one clear responsibility each. Backends are pluggable interfaces in
  `internal/providers`, with concrete impls in sub-packages (`gitops/flux`, `metrics`, …).
- **Read-only first.** No cluster-mutating code in v1 — see `docs/design.md` §9 (the autonomy ladder).
- Module path `github.com/Smana/runlore`; CLI binary `lore`.

## Layout

`cmd/lore` (entrypoint) · `internal/{config,trigger,investigate,whatchanged,catalog,curator,audit,model,notify}`
· `internal/providers` (the contracts) · `deploy/helm/runlore` · `examples/runbooks` (seed OKF catalog).
