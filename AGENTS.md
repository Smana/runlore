# RunLore — contributor & agent guide

RunLore is a self-improving, GitOps-native SRE agent written in Go. Start with
[`docs/design.md`](docs/design.md) for the architecture and `dev/plans/` for the implementation plans.

## Quality gate — run before every commit

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
```

- `gofmt -l .` must print nothing.
- `golangci-lint run ./...` must report **`0 issues`**.
- Linter config: [`.golangci.yml`](.golangci.yml) (golangci-lint **v2**). CI runs the same gate
  ([`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)).

## Try it — mock alert, end to end

```bash
hack/demo.sh
```

Builds `lore`, runs `lore serve`, and fires the mocked Alertmanager batch in
[`examples/alertmanager-webhook.json`](examples/alertmanager-webhook.json) through the trigger
policy — printing the investigate/skip decision per alert (covers match, dedup, severity/environment
filters, ignore-list, and resolved-drop). That JSON shape is what Alertmanager/VMAlert POST.

## Conventions

- **TDD.** Write the failing test first, then the minimal implementation. Prefer table-driven tests.
  Tests must verify behaviour, not mocks.
- **Errors.** Wrap with `%w`; compare with `errors.Is` / `errors.As` (enforced by `errorlint`).
- **Context.** `context.Context` is the first parameter of any function that does I/O.
- **Exported symbols carry doc comments** (enforced by `revive`).
- **Small, focused files** — one clear responsibility each. Backends are pluggable interfaces in
  `internal/providers`, with concrete impls in sub-packages (`gitops/flux`, `metrics`, …).
- **Autonomy ladder.** Cluster-mutating code lives behind `actions.mode` (`approve`/`auto`) — both
  off by default, both fail-closed (approval token + audit log required). See `docs/design.md` §9.
- Module path `github.com/Smana/runlore`; CLI binary `lore`.

## Layout

`cmd/lore` (entrypoint) · `internal/{config,trigger,investigate,whatchanged,catalog,curator,audit,model,notify}`
· `internal/providers` (the contracts) · `deploy/helm/runlore` · `examples/runbooks` (seed OKF catalog).
