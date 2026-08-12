---
title: Security
weight: 50
---

You are considering pointing an LLM-driven agent at a production cluster. The question that
deserves an answer first is not *what can it do* — it is **what can it do that you did not
approve**.

The short answer: **the model proposes, the server decides.** The investigation loop hands the
model read-only tools and exactly one structured exit, `submit_findings`. A finding may contain
*proposed* actions, but a proposal is inert text until a server-side gate lets it through — and
that gate ignores anything the model wrote about its own authorization.
[How that invariant is enforced →]({{< relref "security-architecture.md#1-the-core-invariant-the-model-proposes-the-server-decides" >}})

## What bounds the blast radius

Four controls, each independent of the model's cooperation:

**It is read-only by default.** RunLore reads your cluster, metrics, logs and network flows. Its
only writes go to Git, as pull requests a human merges. A default install executes nothing.
[Read-only by default →]({{< relref "security-model.md#read-only-by-default" >}})

**The action gate fails closed.** Teams that climb `suggest` → `approve` get execution only for
*reversible*, allowlisted operations, after an explicit human approval. Unobserved targets never
auto-execute, and the operation set is a closed registry — not something a model can extend by
asking.
[The action gate →]({{< relref "security-model.md#the-action-gate-climbing-the-autonomy-ladder" >}})

**Secrets are redacted at one chokepoint** before data reaches the model or leaves over a
notifier, rather than at each call site where a new path would silently miss it.
[Redaction boundaries →]({{< relref "security-model.md#secret-redaction-at-the-llm-and-egress-boundaries" >}})

**Tool output is data, never instructions.** Cluster output, KB entries and external MCP results
are treated as untrusted and stay inert in every renderer — no live markup reaches your chat.
[Untrusted output →]({{< relref "security-architecture.md#3-untrusted-output-stays-inert-in-every-renderer" >}})

## What it does not claim

This is the part worth reading before the feature list, because it is where most agent security
pages stop being useful.

- **A prompt injection can still bias an answer.** The controls above bound *consequences* — no
  write without the gate, no secret past the redactor's coverage, no live markup in chat. A
  poisoned log line can still steer the model toward a wrong root cause. Human review of findings
  and KB pull requests is the load-bearing quality gate, by design.
- **Redaction is best-effort.** The model provider sees redacted cluster data. If that is
  unacceptable for your environment, self-host the model in-cluster — RunLore runs against any
  OpenAI-compatible endpoint.
- **RCA can be wrong.** Frontier root-cause analysis is sub-50% on real incidents. `unresolved` is
  a first-class output, and the adversarial verify pass can only ever *lower* a finding's
  confidence, never raise it.
- **Configured endpoints are trusted.** The network guards defend against redirects and response
  content, not against a hostile operator-supplied hostname.

[Honest limitations →]({{< relref "security-model.md#honest-limitations" >}}) ·
[What the architecture does not claim →]({{< relref "security-architecture.md#7-what-this-architecture-does-not-claim" >}})

## Find the answer to your question

| If you are asking | Go here |
|---|---|
| What permissions does it need in my cluster? | [Least-privilege RBAC]({{< relref "security-model.md#least-privilege-rbac" >}}) |
| What credentials does it hold, and where? | [Credentials & the GitHub App]({{< relref "security-model.md#credentials--the-github-app" >}}) |
| Can I prove what it did? | [Tamper-evident audit log]({{< relref "security-model.md#tamper-evident-audit-log" >}}) |
| What happens if I connect a third-party MCP server? | [External MCP tools]({{< relref "security-model.md#external-mcp-tools" >}}) |
| Who can click the 👍/👎 buttons, and what do they change? | [Feedback channels]({{< relref "security-model.md#the-feedback-channels---exposure--trust-model" >}}) |
| What is the actual threat model? | [Threat model at a glance]({{< relref "security-architecture.md#6-threat-model-at-a-glance" >}}) |
| How is the autonomy ladder enforced in code? | [LLM security architecture]({{< relref "security-architecture.md" >}}) |

## The receipts

- **[OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/Smana/runlore)** — supply-chain
  posture, scored continuously and published, not asserted here.
- **[Nightly eval](https://runlore.io/eval)** — every run published per scenario, red or green,
  including the poisoned-entry case that proves a bad KB entry is rejected at recall time.
- **[Reporting a vulnerability](https://github.com/Smana/runlore/blob/main/SECURITY.md)** — how to
  report privately, and what to expect back.

> [!NOTE]
> RunLore is maintained by one person and is pre-1.0. That is a real part of your risk assessment
> and is stated plainly in [Who maintains this](https://github.com/Smana/runlore#who-maintains-this),
> alongside what you keep if maintenance stops.
