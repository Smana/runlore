---
title: RunLore vs Aurora
description: "Aurora executes commands in sandboxed pods, analyzes Terraform, and opens fix PRs — the fastest-moving open-source threat in this space. An honest, sourced comparison."
last_verified: 2026-08-02
---

*Claims about Aurora below were checked against the project's own repository — including
its Python and TypeScript source, not just its README — and its public website on
2026-08-02. If Aurora has shipped something new since, please
[open an issue](https://github.com/Smana/runlore/issues).*

Aurora, built by [Arvo AI](https://github.com/Arvo-AI), is the fastest-moving open-source
project in this space: Apache-2.0, commercially backed, and shipping features RunLore
doesn't have at all — sandboxed command execution, Terraform tooling, auto-generated
postmortems, fix-PR drafts. RunLore doesn't try to out-execute it. The difference is
where the resulting knowledge ends up, and how much scrutiny it gets before it's trusted
again.

## What Aurora does better

- **Commercial backing.** Aurora is [backed by Panache Ventures and Front Row
  Ventures](https://www.aurorasre.ai/), with a managed SaaS/VPC tier funding ongoing
  development on top of the open-source core. RunLore, today, has no company and no
  funding behind it — one person, maintained in spare time.
- **Sandboxed execution of real commands.** Aurora's agents run `kubectl`, `aws`, `az`,
  and `gcloud` autonomously in [isolated Kubernetes pods with
  NetworkPolicy](https://github.com/Arvo-AI/aurora#security), not against your control
  plane directly. RunLore is read-only by default; its `auto` mode only executes
  suspend/resume/reconcile against a GitOps controller. Aurora's autonomy ceiling for
  investigation-time commands is materially higher.
- **Terraform / IaC tooling, which RunLore has none of.** Aurora's agents can write,
  plan, and apply Terraform, and separately inspect existing state — outputs, `state
  list`, `state show`, `state pull` — as read-only queries
  ([`iac_state_commands.py`](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/tools/iac/iac_state_commands.py),
  [tool-selection skill](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/skills/core/tool_selection.md)).
  RunLore has no Terraform or infrastructure-as-code capability at all; it reads
  GitOps-reconciled Kubernetes manifests, not the IaC that provisioned the underlying
  cloud resources.
- **Auto-generated postmortems, and fix-PR drafts.** "Generate Postmortem" is a built-in
  Aurora Action that [fires automatically when an incident is
  resolved](https://github.com/Arvo-AI/aurora/blob/main/server/services/actions/system_actions.py)
  and exports to Confluence, Notion, Jira, or SharePoint. Separately, Aurora's agent can
  propose a code fix as an anchored diff during an investigation; a human [reviews it and
  creates the pull request from the
  UI](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/tools/github_fix_tool.py)
  — not a system RunLore has at all.

## What RunLore does that Aurora does not

- **Aurora's knowledge base has no review gate; RunLore's does.** Documents uploaded to
  Aurora's knowledge base go straight from `uploading` to `processing` to indexed in
  Weaviate for RAG retrieval — there is no approval step in between
  ([`document_processor.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/knowledge_base/document_processor.py),
  [`knowledge_base/routes.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/knowledge_base/routes.py)).
  RunLore's catalog is human-reviewed markdown: every entry is a pull request a person
  merges.
- **Aurora does have a feedback signal ("Aurora Learn") — but it isn't an outcome
  signal.** A user can click "helpful" on a completed RCA, which immediately stores the
  full investigation transcript in a separate Weaviate collection for future retrieval,
  enabled by default
  ([`incident_feedback/weaviate_client.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/incident_feedback/weaviate_client.py),
  [`incident_feedback/routes.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/incident_feedback/routes.py)).
  That store also has no review gate, and the "helpful" click is a single, immediate,
  subjective judgment — the code has no mechanism to re-check it against whether the
  incident actually stayed fixed. RunLore's trust in a knowledge entry decays
  specifically on **real-world resolve-rate** over time, not a one-time rating at
  generation. See the [learning loop]({{< relref "/docs/concepts/learning-loop.md" >}}).
- **No Git export, either way.** Aurora's knowledge lives in Postgres, Weaviate, and
  Memgraph. The only export path in the product is postmortems, to Confluence / Notion /
  Jira / SharePoint — there is no mechanism to export the knowledge base or
  Aurora-Learn-stored RCAs to a Git repository. RunLore's catalog *is* a Git repo you
  own, portable by construction.

## At a glance

| | RunLore | Aurora |
|---|---|---|
| What it is | Multi-signal investigation agent, knowledge-first | LangGraph agents that investigate, execute, remediate, and document |
| License | Apache-2.0 | [Apache-2.0](https://github.com/Arvo-AI/aurora/blob/main/LICENSE) |
| Backing | Solo-maintained, unfunded | [Arvo AI](https://github.com/Arvo-AI), backed by [Panache Ventures and Front Row Ventures](https://www.aurorasre.ai/) |
| Command execution | Read-only by default; `auto` executes only suspend/resume/reconcile | [Sandboxed `kubectl`/`aws`/`az`/`gcloud`](https://github.com/Arvo-AI/aurora#security) in isolated pods, full read/write |
| Terraform / IaC | None | [Write, plan, apply, and read-only state inspection](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/skills/core/tool_selection.md) |
| Knowledge ingestion | Human-authored markdown, PR-merged, before it's trusted | Auto-indexed on document upload; auto-stored on a single "helpful" click ([Aurora Learn](https://github.com/Arvo-AI/aurora/blob/main/server/routes/incident_feedback/weaviate_client.py)) — no review gate either way |
| Knowledge portability | Plain markdown in your Git repo, OKF-compatible | Locked in Postgres + Weaviate + Memgraph; no export path for the knowledge base itself |
| Outcome signal | Trust decays on real-world resolve-rate ([learning loop]({{< relref "/docs/concepts/learning-loop.md" >}})) | One-time "helpful / not helpful" click at generation time; never re-checked against whether the fix held |
| Postmortems | Not a RunLore feature | [Auto-generated on incident resolution](https://github.com/Arvo-AI/aurora/blob/main/server/services/actions/system_actions.py), exports to Confluence/Notion/Jira/SharePoint |
| Fix PRs | Not a RunLore feature | Agent drafts the edit; [human reviews and creates the PR](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/tools/github_fix_tool.py) |

## Pick Aurora instead if…

- You want an agent that takes real, sandboxed actions during investigation — running
  cloud CLI commands, writing and applying Terraform — not just reading signals.
- You want Terraform/IaC-aware tooling, which RunLore does not have in any form.
- You want commercial backing, a managed/SaaS deployment option, or postmortems and
  fix-PR drafts generated as part of the workflow.

## Sources

- [Aurora GitHub repository](https://github.com/Arvo-AI/aurora) — license, description, stars, checked 2026-08-02.
- [Aurora README](https://github.com/Arvo-AI/aurora) — features, security model, architecture table, checked 2026-08-02.
- [Aurora website (aurorasre.ai)](https://www.aurorasre.ai/) — investor backing (Panache Ventures, Front Row Ventures), open-source/managed pricing model, checked 2026-08-02.
- [`document_processor.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/knowledge_base/document_processor.py) and [`knowledge_base/routes.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/knowledge_base/routes.py) — document upload/processing flow, no review step, checked 2026-08-02.
- [`incident_feedback/routes.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/incident_feedback/routes.py) and [`incident_feedback/weaviate_client.py`](https://github.com/Arvo-AI/aurora/blob/main/server/routes/incident_feedback/weaviate_client.py) — "Aurora Learn" feedback-to-knowledge pipeline, checked 2026-08-02.
- [`server/chat/backend/agent/skills/core/tool_selection.md`](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/skills/core/tool_selection.md) — Terraform/`iac_tool` vs `cloud_exec` decision logic, checked 2026-08-02.
- [`iac_state_commands.py`](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/tools/iac/iac_state_commands.py) — read-only Terraform state inspection, checked 2026-08-02.
- [`system_actions.py`](https://github.com/Arvo-AI/aurora/blob/main/server/services/actions/system_actions.py) — built-in "Generate Postmortem" action, checked 2026-08-02.
- [`github_fix_tool.py`](https://github.com/Arvo-AI/aurora/blob/main/server/chat/backend/agent/tools/github_fix_tool.py) — human-reviewed fix-PR flow, checked 2026-08-02.
