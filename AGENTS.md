# RunLore — contributor & agent guide

RunLore is a self-improving, GitOps-native SRE agent written in Go. Start with
[`docs/design.md`](docs/design.md) for the architecture and `dev/plans/` for the implementation plans.

## Quality gate — run before every commit

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
```

- `gofmt -l .` must print nothing.
- `hack/lint.sh` must report **`0 issues`**.
- Linter config: [`.golangci.yml`](.golangci.yml) (golangci-lint **v2**). CI runs the same gate
  ([`.github/workflows/ci.yaml`](.github/workflows/ci.yaml)).
- **Use `hack/lint.sh`, not a bare `golangci-lint run ./...`.** It is that command with
  `GOTOOLCHAIN` pinned to go.mod's `toolchain` line — the Go version CI resolves too. With a
  newer Go on PATH, the staticcheck bundled in golangci-lint panics building its IR
  (`unexpected expr: *ast.KeyValueExpr`) and aborts the whole run, so the failure has nothing
  to do with your code. Do **not** reach for `--disable=staticcheck` to get past it: that runs
  clean while silently dropping one of the five linters in the `standard` set.

## Try it

```bash
hack/demo.sh                 # a real investigation on recorded evidence (no cluster, no key)
hack/demo-trigger-policy.sh  # fires mocked Alertmanager alerts through the trigger policy
```

`hack/demo.sh` replays a recorded model transcript through the real investigation loop and
renders a real verdict card — no cluster, no API key, no network.

`hack/demo-trigger-policy.sh` builds `lore`, runs `lore serve`, and fires the mocked Alertmanager
batch in [`examples/alertmanager-webhook.json`](examples/alertmanager-webhook.json) through the
trigger policy — printing the investigate/skip decision per alert (covers match, dedup,
severity/environment filters, ignore-list, and resolved-drop). That JSON shape is what
Alertmanager/VMAlert POST.

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
