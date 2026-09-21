# RunLore — contributor & agent guide

RunLore is a self-improving, GitOps-native SRE agent written in Go. Start with the design doc,
[`website/content/docs/concepts/design.md`](website/content/docs/concepts/design.md)
(published at <https://runlore.io/docs/concepts/design/>), for the architecture, and with
`dev/plans/` for the implementation plans.

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
  off by default, both fail-closed (approval token + audit log required). See the design doc,
  §9 "Safety & trust model".
- Module path `github.com/Smana/runlore`; CLI binary `lore`.

## Layout

One package per responsibility, grouped here by the stage of the loop it serves. Read a
package's `// Package …` doc comment for its contract before changing it.

- **Entry & wiring.** `cmd/lore` (entrypoint) · `internal/app` (dependency-injection
  builders and config predicates, one file per subcommand or concern) · `internal/config`.
- **Ingress.** `internal/server` (HTTP endpoints) ·
  `internal/source/{alertmanager,pagerduty,grafana,custom,gitops}` (webhook and watcher
  adapters) · `internal/trigger` (investigate-or-skip policy) · `internal/coalesce` (folds
  correlated alerts into one incident) · `internal/ratelimit`.
- **Investigation.** `internal/investigate` (the ReAct loop, its tools, recall, verify) ·
  `internal/whatchanged` + `internal/gitrev` + `internal/sourcerepo` (the GitOps "what
  changed" spine) · `internal/model/{anthropic,openai,gemini,replay}` over
  `internal/model/clientcore` · `internal/mcp` (stdio MCP server and streamable-HTTP MCP
  client) · `internal/redact` (secret masking at every boundary) · `internal/httpx`.
- **Providers — the contracts.** `internal/providers` (interfaces, resource identity) with
  `cloud/`, `cluster/`, `gitops/` implementations; further backends in
  `internal/{metrics,logs/{loki,victorialogs,elasticsearch},network,gcplog}`.
- **Knowledge.** `internal/okf` (OKF markdown serialisation) · `internal/catalog` (load and
  search) · `internal/embed` · `internal/kbvalidate` · `internal/kbimport` · `internal/kbmcp`
  · `internal/curator` (file-time learning gate) · `internal/curate` (backlog grooming) ·
  `internal/forge/{github,gitlab}` · `internal/outcome` (append-only JSONL ledger).
- **Delivery & chat.** `internal/notify` (Slack, Matrix, `webhook/`, `templated/`) ·
  `internal/slackcard` · `internal/thread` (a human reply in a thread becomes a KB note).
- **Safety & ops.** `internal/action` (autonomy-ladder gate) · `internal/executor`
  (reversible Argo CD operations) · `internal/audit` (tamper-evident log) ·
  `internal/telemetry` · `internal/logging`.
- **Guards & eval.** `internal/eval` (replays recorded cases) · `internal/docsguard` (tests
  that pin published docs to the code) · `internal/foldguard`.
- **Outside Go.** `deploy/helm/runlore` (chart) · `deploy/observability` (alerts, Grafana) ·
  `examples/runbooks` (seed OKF catalog) · `examples/{demo,eval,scenarios}` (recorded
  transcripts and cases) · `eval/` (scorecard config, rubric, scenarios) ·
  `plugins/kb-steward` · `hack/` (scripts) · `website/` (Hugo docs site) · `dev/plans/`
  and `docs/superpowers/` (working notes, not published).
