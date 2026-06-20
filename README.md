# RunLore

**The self-improving, GitOps-native SRE agent.** RunLore **reacts** to incidents, **investigates**
by correlating *what changed* across your GitOps engine and observability stack, and **learns** —
writing every resolved incident into an open, git-versioned knowledge catalog that makes the next
investigation faster.

> Status: **early design / scaffold.** Nothing here works yet. See [`docs/design.md`](docs/design.md)
> for the full design and [`docs/prior-art.md`](docs/prior-art.md) for how RunLore relates to
> k8sgpt, HolmesGPT, kagent, and the commercial AI-SRE market.

## Why

A lot of *interactive* GitOps incident investigation is already solved by today's tools. What isn't:

- **Unattended** operation — wake at 03:00 on an alert, investigate, hand you a root cause.
- **"What changed" correlation** — diff Git between the two deployed revisions and tie the actual
  delta to the metric/log/network impact.
- **Learning** — accumulate knowledge into an **open** catalog instead of relying on hand-curated
  runbooks (the closed tools that learn don't share).

RunLore is exactly those gaps — and only those.

## Three pillars

| | |
|---|---|
| **React** | incident/alert webhook gated by a **trigger policy** (only prod, only critical, by namespace/team/label) · GitOps failure events · proactive watch · on-demand CLI / chat |
| **Investigate** | *what-changed* spine (revision history + Git diff) · cross-signal correlation · runbook-grounded ReAct · confidence + explicit `unresolved` |
| **Learn** | reads a cached OKF knowledge catalog (instant recall) and writes new incidents back via reviewed PRs — knowledge is portable markdown in git, never vendor lock-in |

## Design principles

- **Read-only first.** v1 ships no cluster-mutating tools; "writes" mean markdown-to-git via PR.
- **GitOps-engine-agnostic** (Flux + ArgoCD) and **metrics-backend-agnostic** (VictoriaMetrics +
  Prometheus); logs/network/cloud pluggable behind the same provider pattern.
- **Single static Go binary.** Runs in your terminal (`lore investigate`) or in-cluster (`lore serve`).
- **Model-agnostic.** Claude, an in-cluster vLLM, or Ollama — your telemetry needn't leave the boundary.
- **Built-in core providers, MCP as the extension layer** — self-contained, but composable.

## Quickstart

> Not implemented yet — placeholder.

```bash
# build the scaffold
go build ./...

# (planned)
lore investigate "checkout p99 doubled since 14:10"
lore serve            # in-cluster: react to alerts/GitOps failures
lore catalog sync     # sync + index the knowledge catalog
lore eval             # replay past incidents, score root-cause identification
```

## License

[Apache-2.0](LICENSE).
