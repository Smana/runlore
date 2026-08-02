---
title: RunLore vs HolmesGPT
description: "HolmesGPT has the broader toolset; RunLore keeps what it learns in a Git repo you own. An honest, sourced comparison."
last_verified: 2026-08-02
---

*Claims about HolmesGPT below were checked against the project's own repository and
documentation on 2026-08-02. If HolmesGPT has shipped something new since — especially
Flux support, which was an open pull request at the time of writing — please
[open an issue](https://github.com/Smana/runlore/issues).*

HolmesGPT is the strongest open-source investigation agent and the one to beat on
breadth: [56 built-in toolsets](https://holmesgpt.dev/latest/data-sources/builtin-toolsets/),
a CNCF Sandbox home, and a real contributor base. RunLore does not try to out-toolset
it — the difference is what happens **after** the investigation.

## What HolmesGPT does better

- **A much broader native toolset.** 56 built-in toolsets as of this check, spanning
  Datadog, New Relic, Splunk, Sentry, Elasticsearch/OpenSearch, VictoriaMetrics, and
  more — see the
  [full list](https://holmesgpt.dev/latest/data-sources/builtin-toolsets/). RunLore's
  native set is a fraction of that (see [below](#toolset-breadth-honestly)).
- **A CNCF Sandbox project with a real contributor base.** HolmesGPT was
  [accepted into the CNCF Sandbox](https://github.com/cncf/sandbox/issues/392) in
  October 2025 and has
  [71 contributors on GitHub](https://github.com/HolmesGPT/holmesgpt/graphs/contributors)
  as of this check. RunLore, today, is maintained by one person. That is a materially
  different bus factor and support surface — if you need a project that survives its
  founder losing interest, that matters.
- **Approval-gated remediation, shipped and versioned.** Since
  [v0.34](https://github.com/HolmesGPT/holmesgpt/releases/tag/0.34.0) (June 2026),
  HolmesGPT's Kubernetes Remediation toolset issues signed (JWT) approval tickets
  before a write action executes — a real, shipped human-in-the-loop gate for actions,
  not a design sketch.
- **Backed by a company, with outside contributions.** Originally built by
  [Robusta.Dev](https://github.com/HolmesGPT/holmesgpt), with contributions from
  Microsoft; Apache-2.0 licensed, ~3,000 GitHub stars, and
  [28 pull requests merged in the 30 days before this check](https://github.com/HolmesGPT/holmesgpt/pulls?q=is%3Apr+is%3Amerged+merged%3A%3E2026-07-02).
  RunLore has no company behind it.

## What RunLore does that HolmesGPT does not

- **HolmesGPT doesn't learn across incidents; RunLore does.** HolmesGPT's
  [skills (formerly runbooks)](https://holmesgpt.dev/latest/reference/skills/) are
  human-authored, static troubleshooting guides — the docs describe no mechanism for
  the agent to generate or refine them from what it observes. Every incident starts
  from the same skill catalog. RunLore curates each **verified** finding into a pull
  request against a Git repo you own; once merged, the same incident is answered from
  the catalog in milliseconds next time, and a note's trust decays if it stops
  matching real-world outcomes. See the
  [learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}).
- **Flux support, and a revision-exact "what changed."** HolmesGPT has an
  [ArgoCD toolset](https://github.com/HolmesGPT/holmesgpt/blob/master/docs/data-sources/builtin-toolsets/argocd.md)
  that can list deployment history and diff an application's *live* state against its
  *desired* Git state — but as of this check there is no built-in Flux toolset (an
  [open PR](https://github.com/HolmesGPT/holmesgpt/pull/2304) adds one, unmerged), and
  no tool that renders the diff **between two already-reconciled revisions** — the
  "what changed in the last deploy" view. RunLore's `what_changed` tool does exactly
  that, for both Flux and Argo CD, scoped to the failing workload.
- **A published, continuous eval scorecard.** HolmesGPT runs a
  [weekly automated regression suite](https://holmesgpt.dev/latest/development/evaluations/)
  (150+ evals, Sundays) plus manual full benchmarks, with results
  [archived by date](https://holmesgpt.dev/latest/development/evaluations/history/).
  RunLore's [nightly eval workflow](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml)
  is designed to publish a per-scenario scorecard — pass/fail, recall outcomes,
  confidence calibration, model, and cost — on **every** run, red or green, not just
  on a schedule; see [Benchmarking]({{< relref "/docs/reference/benchmarking.md" >}}).

## At a glance

| | RunLore | HolmesGPT |
|---|---|---|
| Investigation breadth | 7 native providers ([data sources]({{< relref "/docs/concepts/data-sources.md" >}})) + any MCP server | [56 built-in toolsets](https://holmesgpt.dev/latest/data-sources/builtin-toolsets/) |
| Learning loop | Yes — [outcome-weighted recall]({{< relref "/docs/concepts/learning-loop.md" >}}), decays on real resolve-rate | No — [static, human-authored skills](https://holmesgpt.dev/latest/reference/skills/) |
| Knowledge portability | Plain markdown in your Git repo, OKF-compatible | Not applicable — no auto-generated knowledge to port |
| Human review gate | Every KB entry is a human-merged pull request | No knowledge to review; remediation *actions* are approval-gated ([since v0.34](https://github.com/HolmesGPT/holmesgpt/releases/tag/0.34.0)) |
| GitOps "what changed" | Revision-exact diff, Flux + Argo CD ([data sources]({{< relref "/docs/concepts/data-sources.md" >}})) | [Argo CD only](https://github.com/HolmesGPT/holmesgpt/blob/master/docs/data-sources/builtin-toolsets/argocd.md) (live-vs-desired diff + history, no Flux, no between-revisions diff) |
| Autonomy ceiling | Read-only by default; `auto` executes only suspend/resume/reconcile | Read-only investigation; [signed-ticket approval-gated remediation](https://github.com/HolmesGPT/holmesgpt/releases/tag/0.34.0) since v0.34 |
| Published eval numbers | [Nightly workflow](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml), every run, pass/fail + cost + calibration ([details]({{< relref "/docs/reference/benchmarking.md" >}})) | [Weekly regression + manual full benchmarks](https://holmesgpt.dev/latest/development/evaluations/), 150+ evals, [archived](https://holmesgpt.dev/latest/development/evaluations/history/) |
| Project maturity | Solo-maintained, pre-1.0 | [CNCF Sandbox](https://github.com/cncf/sandbox/issues/392) (Oct 2025), Apache-2.0, [71 contributors](https://github.com/HolmesGPT/holmesgpt/graphs/contributors), backed by Robusta.Dev |
| MCP client | Yes — extends the agent's toolbox ([MCP]({{< relref "/docs/configuration/mcp.md" >}})); also serves as an MCP server itself | Yes — [remote MCP servers](https://holmesgpt.dev/latest/data-sources/remote-mcp-servers/) plus MCP-based built-in toolsets |

## Toolset breadth, honestly

RunLore's native tool set is narrower **by design** — seven pluggable providers
(GitOps, metrics, logs, network, cloud, Kubernetes, source repos) rather than a
toolset per vendor. That is a real gap against HolmesGPT's 56. Any specific
observability vendor RunLore doesn't natively speak, an [MCP
server]({{< relref "/docs/configuration/mcp.md" >}}) closes — RunLore is an MCP
*client* as well as a server, so a Datadog or Splunk MCP server extends its
investigation toolbox with zero Go code. It is not the same as a maintained,
first-party integration, and we're not pretending otherwise.

## Pick HolmesGPT instead if…

- You need a specific vendor toolset **today** — Datadog, New Relic, Splunk, and
  dozens more ship built-in, with no MCP server to stand up first.
- You want a CNCF-governed project with a large, active contributor base and a
  company behind it.
- You don't want a knowledge-review workflow at all — you just want an agent that
  investigates and reports, every time, with no PRs to merge.

## Sources

- [HolmesGPT built-in toolsets](https://holmesgpt.dev/latest/data-sources/builtin-toolsets/) — toolset count and list, checked 2026-08-02.
- [HolmesGPT GitHub repository](https://github.com/HolmesGPT/holmesgpt) — license, stars, description, CNCF Sandbox status, checked 2026-08-02.
- [HolmesGPT merged pull requests, last 30 days](https://github.com/HolmesGPT/holmesgpt/pulls?q=is%3Apr+is%3Amerged+merged%3A%3E2026-07-02) — recent activity, checked 2026-08-02.
- [HolmesGPT contributors graph](https://github.com/HolmesGPT/holmesgpt/graphs/contributors) — contributor count (verified via the GitHub API), checked 2026-08-02.
- [CNCF Sandbox application — HolmesGPT](https://github.com/cncf/sandbox/issues/392) — Sandbox acceptance, checked 2026-08-02.
- [HolmesGPT v0.34.0 release notes](https://github.com/HolmesGPT/holmesgpt/releases/tag/0.34.0) — signed approval tickets, approval-gated remediation, checked 2026-08-02.
- [HolmesGPT skills reference](https://holmesgpt.dev/latest/reference/skills/) — human-authored, static skills/runbooks, checked 2026-08-02.
- [HolmesGPT ArgoCD toolset](https://github.com/HolmesGPT/holmesgpt/blob/master/docs/data-sources/builtin-toolsets/argocd.md) — capabilities (diff, history), checked 2026-08-02.
- [Add native FluxCD toolset (open PR #2304)](https://github.com/HolmesGPT/holmesgpt/pull/2304) — Flux support not yet merged, checked 2026-08-02.
- [HolmesGPT evaluations](https://holmesgpt.dev/latest/development/evaluations/) — eval count and cadence, checked 2026-08-02.
- [HolmesGPT evaluation history](https://holmesgpt.dev/latest/development/evaluations/history/) — published benchmark archive, checked 2026-08-02.
- [HolmesGPT remote MCP servers](https://holmesgpt.dev/latest/data-sources/remote-mcp-servers/) — MCP client support, checked 2026-08-02.
