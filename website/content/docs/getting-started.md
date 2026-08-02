---
title: Getting Started
weight: 10
---

RunLore reacts to incidents, investigates them with an LLM (grounded in your knowledge catalog),
delivers findings to chat, and — optionally — curates what it learns back to a Git repo as pull
requests. This page has three stops, each independently worth doing on its own:

1. **[Try it](#try-it--no-cluster-no-keys)** — a real root cause, no cluster, no keys, ~60 seconds.
2. **[Investigate your own stack](#investigate-your-own-stack--kubeconfig--one-api-key)** — one CLI
   command against your cluster, no config file.
3. **[The full loop](#the-full-loop--production-install)** — `lore serve` in-cluster: continuous
   reaction, chat delivery, and knowledge curation.

## Try it — no cluster, no keys

Before wiring anything, watch RunLore reach a real root cause over recorded evidence — no
Kubernetes, no LLM key, no network. You only need Go and git:

```bash
git clone https://github.com/Smana/runlore && cd runlore
hack/demo.sh
```

`hack/demo.sh` builds the binary and runs `lore demo investigate --offline default` — replaying a
transcript recorded once against a live model through the **real investigation loop**: the same
ReAct tool calls, verify pass, and verdict-card renderer that run in `lore serve`, over fake (but
realistic) evidence for a Harbor chart-bump incident:

```text
== RunLore demo: investigating "harbor-chart-bump" (recorded model turns, fake providers, no cluster) ==
   model turns recorded 2026-08-02T07:48:52Z with openai/glm-4.5-air

→ what_changed()
  flux Kustomization/apps aaa111..bbb222 --- apps/harbor/values.yaml - tag: 1.14.2 + tag: 1.15.0 …

== submit_findings ==
*Investigation* — confidence 90%
🔥 Verdict: Action required
Resource: Deployment apps/harbor-core
1. *Database migration deadlock preventing harbor-db pod from starting, causing harbor-core
   connection failures* (90%)
   → suggested: Investigate and resolve the migration deadlock in harbor-db pod, then restart
     the database to clear the lock (reversible)
```

That `model turns recorded … with openai/glm-4.5-air` line is not decoration — it's the honesty
mechanism. Nothing here is scripted for the demo: the model's reasoning was captured once against a
live model and is replayed byte-for-byte, so the card discloses exactly **when** and **with which
model** it was produced, rather than letting you mistake a replay for a live run. Model behavior on
the same evidence can drift release to release, so that provenance line is what tells you how
current the transcript is. Re-record it yourself against your own model with `--record` — see the
[CLI reference]({{< relref "/docs/reference/cli.md" >}}).

Already have `lore` installed (see [below](#investigate-your-own-stack--kubeconfig--one-api-key))?
The command `hack/demo.sh` runs is just:

```bash
lore demo investigate --offline default
```

run from inside a clone — the demo fixtures under `examples/` are read from disk at startup, not
embedded in the released binary, so this one needs the repo checked out.

Curious about the **filter** that decides what gets investigated in the first place, rather than the
investigation itself? `hack/demo-trigger-policy.sh` fires mocked Alertmanager alerts through the
trigger policy — no LLM involved at all.

---

## Investigate your own stack — kubeconfig + one API key

Ready to point it at something real? `lore investigate` runs one on-demand investigation against
**your** cluster and prints the findings — no `runlore.yaml`, no Helm install. First, get the
binary:

```bash
curl -fsSL https://runlore.io/install.sh | sh
# or: go install github.com/Smana/runlore/cmd/lore@latest
```

Then, with a working `kubeconfig` and **one** model credential exported:

```bash
export ANTHROPIC_API_KEY=sk-ant-...          # or OPENAI_BASE_URL (+ OPENAI_API_KEY / OPENAI_MODEL)
lore investigate --alert "HarborProbeFailure" --namespace apps
```

With no `--config`, `investigate` synthesizes a **model-only** config straight from the
environment — every data source stays unset, so each one simply disables the tool it would have
unlocked rather than demanding a full stack:

| Env var | Effect |
|---|---|
| `OPENAI_BASE_URL` (+ optional `OPENAI_API_KEY`, `OPENAI_MODEL`) | any OpenAI-compatible endpoint — keyless if the endpoint itself needs no credential (e.g. a local vLLM/Ollama) |
| `ANTHROPIC_API_KEY` | native Anthropic, checked if no `OPENAI_BASE_URL` is set |

Four flags override whatever that resolves to, so you can point the CLI at your stack without
writing a file at all:

| Flag | Overrides |
|---|---|
| `--model` | the model name |
| `--base-url` | the OpenAI-compatible endpoint |
| `--metrics-url` | PromQL endpoint — enables the `query_metrics` tool |
| `--logs-url` | logs endpoint — enables the `query_logs` tool |

`investigate` never refuses to run just because a signal is missing — it runs on whatever's
configured and says so on stderr:

```text
note: running without metrics (query_metrics), logs (query_logs), knowledge catalog (kb_search, instant recall) — pass --metrics-url/--logs-url or a --config to enable them
```

**That notice means "under-configured," not "broken."** The investigation still runs to completion
and still reaches a verdict — just over whatever evidence is actually available. Treat it as a
checklist for widening the picture (another flag, a `--config` pointing at a fuller `runlore.yaml`),
not as an error to work around. Full flag/env reference:
[CLI]({{< relref "/docs/reference/cli.md" >}}).

---

## The full loop — production install

This is where RunLore stops being a one-off lookup and starts reacting to incidents continuously,
delivering to chat, and — once a knowledge-catalog repo is wired up — curating what it learns back
as pull requests (the Learn loop, RunLore's differentiator). **Running in-cluster (`lore serve`,
via Helm) is the recommended way to get there** — the CLI above is for one-off local runs against
the same engine.

RunLore is **read-only on your cluster**: it never mutates workloads. Its only writes go to the Git
forge (issues/PRs on a repo you designate).

> For local development / testing on k3d, see
> [CONTRIBUTING.md](https://github.com/Smana/runlore/blob/main/CONTRIBUTING.md) instead.

The walkthrough below wires **one golden path** — Prometheus/VictoriaMetrics metrics, GitHub for
curation, Slack for delivery, and **Anthropic** for the model. That is what the shipped values
profiles and the `Secret` in [Step 3](#step-3--credentials) use: one API key, no endpoint of your
own to run. Pointing it at any **OpenAI-compatible** endpoint instead is a two-line change
(`provider: openai` + `base_url`) — and that is the combination the nightly eval and the k3d e2e
suite exercise, so neither path is untested. Every other data source, LLM, notifier, or forge is
equally supported; each gets a one-line pointer to its own page under
[Integrations]({{< relref "/docs/integrations/_index.md" >}}) as it comes up below.

## Prerequisites

### Required

- A **Kubernetes cluster** — any conformant cluster works: EKS, GKE, AKS, or local
  [k3d](https://k3d.io/) / [kind](https://kind.sigs.k8s.io/) (follow each project's install docs).
- `kubectl` + `helm` (v3.12+).
- An **LLM** — the shipped values profiles use native
  [Anthropic]({{< relref "/docs/integrations/anthropic.md" >}}) (one API key, nothing to run); any
  **OpenAI-compatible** endpoint (in-cluster [vLLM](https://github.com/vllm-project/vllm),
  [Ollama](https://ollama.com/), OpenAI, OpenRouter) is a two-line swap. Keep the
  model in-cluster if you don't want telemetry to leave your boundary — see
  [Local / keyless]({{< relref "/docs/integrations/local-keyless.md" >}}).

### Recommended

- **[Flux](https://fluxcd.io/flux/installation/) or
  [Argo CD](https://argo-cd.readthedocs.io/en/stable/getting_started/) running on that cluster** —
  select with `config.gitops.engine` (`flux` default, or `argocd`). This unlocks the what-changed spine and the
  GitOps-failure trigger (Flux `Kustomization`/`HelmRelease`, or Argo CD `Application`, going
  `Ready=False`) — RunLore's sharpest signal. **Not required**: every data source is pluggable, and
  an unset one just disables the tool it would have unlocked — see
  [GitOps failures]({{< relref "/docs/integrations/gitops.md" >}}). Without it, RunLore still reacts
  to Alertmanager alerts and investigates with whatever other signals you've wired.
- A **GitHub App** for curation — [Step 2](#step-2--github-app-for-curation-optional) below.
  **Without it the Learn loop (curation) is disabled**: RunLore still reacts and investigates, it
  just can't write what it learns back to your KB repo. Since the learning loop is RunLore's
  differentiator, this is **strongly recommended**.

### Optional

Everything else is genuinely pluggable — an unset one just disables the tool or channel it would
have unlocked, nothing else changes. Full catalog:
[Integrations]({{< relref "/docs/integrations/_index.md" >}}).

- **Data sources** — [Prometheus/VictoriaMetrics]({{< relref "/docs/integrations/prometheus.md" >}})
  (used below), [VictoriaLogs]({{< relref "/docs/integrations/victorialogs.md" >}}) or
  [Loki]({{< relref "/docs/integrations/loki.md" >}}) for logs, a network-flow signal
  ([Hubble]({{< relref "/docs/integrations/hubble.md" >}}) — Cilium only,
  [AWS VPC Flow Logs]({{< relref "/docs/integrations/aws-vpc-flow-logs.md" >}}), or
  [GCP Firewall Logs]({{< relref "/docs/integrations/gcp-firewall-logs.md" >}}) — RunLore does
  **not** assume Cilium), [AWS cloud control plane]({{< relref "/docs/integrations/aws-cloud.md" >}})
  (also see [Step 4b](#step-4b--aws-cloud-provider-optional) below),
  [source repos]({{< relref "/docs/integrations/source-repos.md" >}}) for real Git diffs behind a
  version bump, and [MCP]({{< relref "/docs/integrations/mcp.md" >}}) for tools RunLore doesn't ship
  natively.
- **A notifier for delivery** — this walkthrough uses
  [Slack]({{< relref "/docs/integrations/slack.md" >}}); a
  [Matrix]({{< relref "/docs/integrations/matrix.md" >}}) account, a generic
  [webhook]({{< relref "/docs/integrations/webhook.md" >}}), or a
  [templated]({{< relref "/docs/integrations/templated.md" >}}) payload (Teams, Discord, ntfy…) all
  work the same way.
- [External Secrets Operator](https://external-secrets.io/) to sync credentials from a vault
  (recommended over raw `Secret`s in production).

---

## Step 1 — Create the knowledge-catalog repo

**This Git repo is where RunLore commits what it learns** — every resolved incident is curated back here
as a PR (the Learn loop), and the agent reads from it to ground future investigations. It's an
**[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog) knowledge catalog**: a Git repo of
markdown files, each with YAML frontmatter. This is *your* portable knowledge base — runbooks, past
incidents, platform constraints.

1. Create a new (private) Git repo, e.g. `your-org/runlore-kb`.
2. Add entries as markdown files — **one file per entry** (YAML frontmatter + a markdown body). Name
   each file with a short, descriptive kebab-case **slug** (e.g. `helmrelease-upgrade-failure.md`); the
   slug is just the entry's identity — not indexed, not a frontmatter field (RunLore names the entries
   it drafts `<title-slug>-<8 fingerprint chars>.md`, so two entries sharing a title can't collide).
   Put them at the repo root or in subfolders (`playbooks/`, `incidents/`, …); the whole tree is
   indexed recursively. Example:

   ```markdown
   ---
   type: Playbook
   title: HelmRelease upgrade failure for shop-api
   description: A Helm chart bump leaves the release Ready=False.
   resource: shop-prod/shop-api
   tags: [flux, helmrelease, upgrade, shop-prod]
   ---
   # Symptom
   Ready=False after a chart bump; often a DB migration that didn't complete.

   # Checks
   - `flux get hr -A | grep -i false`
   - the rendered diff between the two chart versions
   ```

   `resource` is the affected workload as `namespace/name`, with no whitespace — it's what recall's
   structural filter matches on, so a scoped entry beats a general one. It is **required for
   `Incident`** entries and optional elsewhere, but an entry with no `resource` can only be recalled by
   an incident that itself carries no workload, so leave it out only for genuinely platform-wide notes.
   `index.md`, `log.md` and any `readme.md` are reserved (a human listing + a changelog) and skipped by
   the indexer — as are dot-files and hidden directories. What search actually matches is one combined
   corpus per entry — `title` + `description` + `resource` + `tags` + body, **not** the filename — so
   write those well. Seed it with whatever runbooks you already have; the agent gets sharper at *your*
   platform as the catalog grows.

   > Writing entries by hand? The full field-by-field contract lives in
   > [`okf-format.md`](https://github.com/Smana/runlore/blob/main/plugins/kb-steward/skills/kb-steward/references/okf-format.md), and
   > `lore validate-kb <catalog-dir>` checks a catalog against it.

3. **Make it available in-cluster.** Two options:

   **Option A — Git-sync (recommended; closes the read/write loop).** RunLore clones the repo and
   re-pulls it on an interval, re-indexing automatically. When curation merges a PR into this repo, the
   new knowledge flows straight back into what the agent searches — no manual step. Configure it under
   `config.catalog.git` ([step 4](#step-4--configure-and-install)) and set `catalog.gitSync: true` (which
   mounts a writable mirror). A **private** repo authenticates with the **same curation GitHub App** by
   default ([step 2](#step-2--github-app-for-curation-optional)) — one credential for both reads and
   writes; set `git.token_env` only to use a different token.

   **Option B — ConfigMap (static).** Mount a snapshot; refresh it yourself when the repo changes:

   ```bash
   git clone https://github.com/your-org/runlore-kb /tmp/runlore-kb
   kubectl -n runlore create configmap runlore-catalog \
     --from-file=/tmp/runlore-kb/ \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

---

## Step 1b — Seed the catalog from your existing runbooks (optional)

You don't have to start empty. If your team already keeps markdown runbooks or
postmortems anywhere, import them and get recall value on day one:

```bash
# preview what would be written — nothing is touched
lore kb import ./our-runbooks --into ./kb --dry-run

# convert + validate + dedup, then write into your local KB checkout
lore kb import ./our-runbooks --into ./kb
cd kb && git add . && git commit -m "seed catalog from existing runbooks" && git push
```

What `import` does, deterministically (no model, no config needed beyond the
directory paths):

- **Adds/normalizes OKF frontmatter** — title from the existing frontmatter or
  the first heading (filename as last resort), description from the first
  paragraph, tags from the document's own tags **plus detected alert names**
  (`KubePodCrashLooping`-style tokens in headings and alert-mentioning lines —
  exactly the recall signal that lets a future alert find the runbook).
- **Classifies** — a document that already carries `Symptom`/`Cause`/`Resolution`
  sections *and* names a `resource` becomes an `Incident`; everything else is a
  `Playbook` (free-form runbooks validate relaxed, same as hand-written entries).
- **Validates** every entry with the same merge gate as `lore validate-kb` —
  nothing is written that the gate would later reject.
- **Dedups** against what the catalog already holds (exact and fuzzy title, same
  rule the curator uses for duplicate PRs) and skips it with a printed reason.

Nothing is committed for you: you review the diff and push, the same
human-in-the-loop bar as every RunLore KB PR. With `--model`, the LLM already
configured in your `runlore.yaml` refines titles/descriptions/tags (purely
optional — a model failure falls back to the deterministic result). Re-running
the same import is a no-op.

---

## Step 2 — GitHub App for curation (optional)

The **Curator** writes findings back to your KB repo: confident, *verified* root causes become a **PR**
drafting an OKF entry; less-confident ones are delivered to chat only — **no repo artifact** (a below-bar
guess must not enter the catalog). Auth is a **GitHub App** — the secure choice over a
personal access token: fine-grained permissions, per-repo installation, and short-lived (1-hour)
installation tokens minted on demand from the App's private key (no long-lived credential in the cluster).

### Create the App

1. **Settings → Developer settings → GitHub Apps → New GitHub App.**
2. Homepage URL: anything (e.g. your repo). Disable Webhooks (RunLore doesn't receive GitHub webhooks).
3. **Repository permissions** (least privilege — grant only these):

   | Permission | Access | Why |
   |---|---|---|
   | Contents | **Read & write** | push the drafted OKF entry to a branch on the KB repo |
   | Pull requests | **Read & write** | open the curation PR |
   | Issues | **Read & write** | open knowledge-gap issues for recurring unresolved patterns (Phase-2 grooming) |

   If your Flux source repos are **private** and you want real Git diffs from them, also grant
   **Contents: Read-only** and install the App on those repos. Public source repos need nothing.
4. **Create**, then **Generate a private key** — download the `.pem` (you only see it once).
5. Note the **App ID** (on the App's page).
6. **Install App** → install it on **only the specific repos** it needs (the KB repo, plus any private
   source repos) — *not* "All repositories". Open the installation and note the **Installation ID**
   (the number in the install settings URL: `.../installations/<id>`).

### Security best practices

- **Never commit the private key** or put it in `values.yaml`. Store it in a `Secret`
  ([step 3](#step-3--credentials)) — ideally synced from a vault via External Secrets.
- **Scope the installation** to specific repos, and grant only the three write permissions above.
- Installation tokens are already **short-lived (1h) and auto-refreshed** — there is no long-lived
  token to leak. **Rotate the App private key** periodically anyway.
- Restrict who can administer the App in your org.
- RunLore's writes are confined to the forge — it has **no cluster-mutating permissions**.

Full key reference and the source-repo App-access edge case:
[Integrations → GitHub]({{< relref "/docs/integrations/github.md" >}}).

---

## Step 3 — Credentials

Create a `Secret` with the credentials your config references by env-var name. In production, prefer an
`ExternalSecret` that pulls these from your vault instead of `kubectl create secret`.

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=ANTHROPIC_API_KEY='<model-api-key>' \
  --from-literal=RUNLORE_WEBHOOK_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=SLACK_WEBHOOK_URL='https://hooks.slack.com/services/...' \
  --from-file=GITHUB_APP_PRIVATE_KEY=/path/to/app-private-key.pem
```

On an OpenAI-compatible endpoint the key is `OPENAI_API_KEY` instead (omit it entirely when the
endpoint is keyless), and `values-full.yaml` adds one more: `APPROVAL_TOKEN`, which gates the action
approval and kill-switch endpoints.

> **`RUNLORE_WEBHOOK_TOKEN` is required once a model is configured.** The `serve` path
> **fails closed** — it refuses to start with an anonymous alert webhook when an LLM is wired (the
> webhook's labels/annotations flow into the prompt and bill the model), so set
> `config.server.webhook_token_env` to this key. See [Step 5](#harden-for-production).

Only include the keys you use. The chart injects the whole Secret as env vars via `envFrom`, and the
config references each by its env-var name (`api_key_env`, `webhook_url_env`, `private_key_env`, …).
Wiring Matrix, a generic webhook, or another notifier instead of Slack follows the exact same
pattern — see the notifier's own page under
[Integrations]({{< relref "/docs/integrations/_index.md" >}}) for its env-var names.

---

## Step 4 — Configure and install

RunLore installs **in-cluster with one Helm command** (this is the recommended deployment). You give it
a `values.yaml` and run `helm install` — pick one of the three shipped profiles below rather than
writing one from scratch, or jump to **[Install](#install)** if you just want the command.

### Pick a profile

The chart ships **three profiles**, each a valid install on its own. Start at the one that matches
what you have wired, and step up later by copying blocks from the next:

| Profile | What it wires | Start here if |
|---|---|---|
| [`values-minimal.yaml`](https://github.com/Smana/runlore/blob/main/deploy/helm/runlore/values-minimal.yaml) | Alert webhook, a model, Slack. Read-only, ~15 lines. | You want to see it react to a real alert today. |
| [`values-standard.yaml`](https://github.com/Smana/runlore/blob/main/deploy/helm/runlore/values-standard.yaml) | **+** git-synced knowledge catalog, GitHub App curation, metrics + logs evidence, JSON logs and `/metrics`. | You did steps 1–2 and want the **learning loop**. |
| [`values-full.yaml`](https://github.com/Smana/runlore/blob/main/deploy/helm/runlore/values-full.yaml) | **+** leader-elected HA, persistence, a default-deny NetworkPolicy, the approve-rung action ladder, AWS cloud + network-flow signals, instant recall. | You are hardening a production install. |

All three are validated in CI against the chart's `values.schema.json` **and** the agent's config
schema, so a block you copy out of one is known to load.

> **A values file is not a `runlore.yaml`.** Everything the agent itself reads is nested under
> `config:` — the chart unwraps that block verbatim into the mounted `runlore.yaml`. The sibling
> top-level keys (`replicaCount`, `catalog`, `persistence`, `rbac`, `networkPolicy`…) are
> **chart-level** and never reach the agent. The per-integration pages under
> [Integrations]({{< relref "/docs/integrations/_index.md" >}}) show raw `runlore.yaml` snippets: nest
> them under `config:` when pasting into a values file.

### Start: `values-minimal.yaml`

This is the whole file — investigate and notify, nothing else. It references the Secret from
[step 3](#step-3--credentials) by env-var name:

```yaml
replicaCount: 1              # one worker; values-full.yaml adds leader-elected HA
image:
  tag: ""                    # defaults to the chart appVersion; pin it in production
envFrom:
  - secretRef:
      name: runlore-secrets  # ANTHROPIC_API_KEY, RUNLORE_WEBHOOK_TOKEN, SLACK_WEBHOOK_URL
config:
  sources:
    alertmanager: {}         # presence is enablement — accept the alert webhook
  model:
    provider: anthropic      # or `openai` + base_url for any OpenAI-compatible endpoint
    model: claude-sonnet-5
    api_key_env: ANTHROPIC_API_KEY
  server:
    webhook_token_env: RUNLORE_WEBHOOK_TOKEN  # MANDATORY once a model is set (serve fails closed)
  notify:
    slack:
      webhook_url_env: SLACK_WEBHOOK_URL
```

Everything left out is genuinely optional: an unset data source disables the tool it would have
unlocked, and an unset `forge` disables curation. Swap the `model` block for any of the
[LLM providers]({{< relref "/docs/integrations/_index.md" >}}), and the `notify` block for
[Matrix]({{< relref "/docs/integrations/matrix.md" >}}), a
[webhook]({{< relref "/docs/integrations/webhook.md" >}}), or a
[templated]({{< relref "/docs/integrations/templated.md" >}}) payload.

### Step up: standard and full

**[`values-standard.yaml`](https://github.com/Smana/runlore/blob/main/deploy/helm/runlore/values-standard.yaml)**
adds the half that makes RunLore more than a one-shot investigator: `catalog.gitSync: true` plus
`config.catalog.git` (the KB repo from [step 1](#step-1--create-the-knowledge-catalog-repo), re-pulled
on an interval so merged PRs flow back into what the agent searches), `config.forge` with the GitHub
App from [step 2](#step-2--github-app-for-curation-optional), and the
[metrics]({{< relref "/docs/integrations/prometheus.md" >}}) +
[logs]({{< relref "/docs/integrations/victorialogs.md" >}}) endpoints the investigation queries for
evidence. It also turns on JSON logging and the `/metrics` endpoint with a `VMServiceScrape`.

**[`values-full.yaml`](https://github.com/Smana/runlore/blob/main/deploy/helm/runlore/values-full.yaml)**
is the annotated reference for a hardened install — read it top to bottom rather than pasting it, since
its cloud and network blocks describe one concrete environment (EKS + Cilium). It adds: 2 replicas with
leader election (only the leader investigates), a `StatefulSet` + PVC so the outcome ledger and the
hash-chained audit log survive restarts, `networkPolicy.strict` with an explicit egress allowlist and
ingress scoped to your monitoring namespace, `actions.mode: approve` (envelope-filtered remediations
that execute **only** after a human click — see [Design]({{< relref "design.md" >}})), the
[AWS cloud control plane]({{< relref "/docs/integrations/aws-cloud.md" >}}) and a
[network-flow signal]({{< relref "/docs/integrations/hubble.md" >}}), and
`catalog.instant_recall` — which short-circuits the LLM loop entirely when the catalog already holds a
trustworthy answer.

Every key either profile sets is documented in
[Configuration]({{< relref "/docs/configuration/configuration.md" >}}), and the chart's own
[`values.yaml`](https://github.com/Smana/runlore/blob/main/deploy/helm/runlore/values.yaml) carries the
exhaustive, commented list of chart-level knobs.

### Install

Save your chosen profile as `values.yaml`, edit the placeholders, and deploy with a single command —
the chart is an OCI artifact on GHCR, published on every release:

```bash
helm install runlore oci://ghcr.io/smana/charts/runlore -n runlore --create-namespace -f values.yaml
```

> Pin a version with `--version X.Y.Z` (the chart version tracks the RunLore release).
> **Dev alternative** — working from a clone of this repo, install from the chart path instead:
> `helm install runlore deploy/helm/runlore -n runlore --create-namespace -f values.yaml`.

Every published chart is **cosign keyless-signed**. To verify before installing (optional):

```bash
cosign verify ghcr.io/smana/charts/runlore:<version> \
  --certificate-identity-regexp 'https://github.com/Smana/runlore/\.github/workflows/release-chart\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

---

## Step 4b — AWS cloud provider (optional)

Enables the `cloud_what_changed` (CloudTrail) and `cloud_resource_health` (EC2/ASG/EKS) tools so the
agent can see infra changes that never touched GitOps. **Read-only**, authenticated with **in-cluster
identity** — no static AWS keys. Full config, the read-only IAM policy, and how it binds to the
ServiceAccount (EKS Pod Identity or IRSA):
[Integrations → AWS cloud control plane]({{< relref "/docs/integrations/aws-cloud.md" >}}).

**Cilium clusters only** — the EKS Pod Identity credential endpoint runs on the node host network
(`169.254.170.23:80`), which Cilium classifies as the `host` entity, so a plain Kubernetes
NetworkPolicy **cannot** match it and the credential fetch is silently dropped. Set
`networkPolicy.awsPodIdentity: true` (chart value) to render a `CiliumNetworkPolicy` that allows it —
see that page's Notes for the exact YAML and the `hubble observe … DROPPED` command that confirms
this is the issue you're hitting.

> **Under `networkPolicy.strict: true`, that flag is not enough.** It covers only the credential
> fetch; the AWS API calls themselves (CloudTrail, EKS, EC2, STS — public regional endpoints on
> 443) are ordinary egress and are dropped unless you declare them too, leaving both cloud tools
> silently empty. `values-full.yaml` shows the `networkPolicy.extraEgress` rule and how to pin the
> CIDR (PrivateLink endpoint subnets, or your NAT gateway EIP).

---

## Step 5 — Point Alertmanager at the webhook

RunLore reacts to Alertmanager's webhook. Route the alerts you care about to its Service (the
**trigger policy** in `config` is the real filter — Alertmanager routing is just the firehose):

```yaml
# alertmanager config
receivers:
  - name: runlore
    webhook_configs:
      - url: http://runlore.runlore.svc:8080/webhook/alertmanager
route:
  routes:
    - receiver: runlore
      continue: true        # keep your existing routing too
```

With 2+ replicas, every warm replica is `Ready` — the Service may route a webhook to any of them,
and a non-leader replica transparently proxies it to the elected **leader** (single hop, looked up
via the leader-election `Lease`), so only the leader's queue ever investigates.

### Harden for production

Once a model is configured the webhook token is **mandatory** (the `serve` path fails closed — see
Step 3); the chart's NetworkPolicy ingress, however, is permissive by default (`ingressFrom: []` ⇒ any
source), so lock that down before pointing a real alert stream at it:

1. **Require a bearer token.** Name an env var in `server.webhook_token_env` (wired from your Secret);
   unauthenticated requests are then rejected with `401`. This token is **required whenever a model is
   configured** (and therefore also under `actions.mode=auto`) — `serve` refuses to start without it.
   ```yaml
   # values.yaml
   config:
     server:
       webhook_token_env: RUNLORE_WEBHOOK_TOKEN
   ```
   ```yaml
   # alertmanager — send the same token as a bearer credential
   webhook_configs:
     - url: http://runlore.runlore.svc:8080/webhook/alertmanager
       http_config:
         authorization:
           type: Bearer
           credentials_file: /etc/alertmanager/secrets/runlore-webhook-token
   ```
2. **Scope ingress** to only the namespace that should reach the webhook (e.g. your monitoring stack):
   ```yaml
   # values.yaml — spliced into the NetworkPolicy `from:`
   networkPolicy:
     ingressFrom:
       - namespaceSelector:
           matchLabels: { kubernetes.io/metadata.name: monitoring }
   ```

See the [Security model]({{< relref "/docs/security/security-model.md" >}}) for the full posture — redaction, RBAC, the action gate.

---

## Step 6 — Verify

```bash
# every replica becomes Ready once its catalog is warm; the Lease names the leader
# (holder identity is <podName>_<podIP> — the IP lets standbys forward work to it)
kubectl -n runlore get pods
kubectl -n runlore get lease runlore-leader -o jsonpath='{.spec.holderIdentity}'; echo

# startup wiring
kubectl -n runlore logs deploy/runlore | grep -E 'catalog loaded|using LLM investigator|curator enabled|watching gitops failures|serving'
```

Expected lines: `catalog loaded … entries=N`, `using LLM investigator`, `watching gitops failures`,
`curator enabled` (if configured), `runlore serving`.

Fire a test: trigger a `critical`/`prod` alert (or `flux suspend`+break a Kustomization). You should see
`msg=incident … investigate=true` → `msg=findings …`, a message in Slack, and (if curation is on)
`msg=curated url=…` pointing at a PR/issue on your KB repo.

---

## Step 7 — The Learn loop: KB lifecycle & re-runs

When curation is on, each investigation lands in your KB repo with a **lifecycle label** so you can tell
raw findings from vetted knowledge:

- **`triggered`** — RunLore just opened this issue/PR; a raw finding, not yet worked.
- **`investigating`** — being worked (RunLore sets this when you ask it to re-run; see below).
- **`solved`** — root cause confirmed *and the resolution captured*. **Only `solved` entries with a
  written resolution should be merged** as a reusable Playbook — that's the quality gate that keeps the
  catalog trustworthy.
- **`wontfix`** — closed without a Playbook.

(High-confidence findings open as a **PR** drafting an OKF entry; lower-confidence ones open as an
**issue** to triage.)

**Re-run an investigation on demand.** RunLore takes no inbound GitHub webhooks, so it *polls*: add the
**`reinvestigate`** label to one of its curated issues and within a couple of minutes it re-runs the
investigation (building on the captured context), posts the fresh findings as a comment, and moves the
label to `investigating`. Use it after more has happened, or once you've added a relevant Playbook and
want a sharper answer. Only RunLore-originated issues (carrying the `runlore` label) are eligible.

---

## What RunLore can and cannot do

- **Cluster**: **read-only by default** — it reads Flux/Argo resources, metrics (PromQL), logs (LogsQL),
  and network flows (Hubble), and never writes.
- **Cloud (AWS, optional)**: **read-only** — CloudTrail `LookupEvents` + EC2/ASG/EKS `Describe`, via
  in-cluster identity (EKS Pod Identity / IRSA). No mutating cloud calls exist in the code. RBAC is limited to watching those resources + its own
  leader-election `Lease`. With `actions.mode: approve` + `rbac.allowActions: true`, it can execute
  *reversible* Flux ops (suspend/resume/reconcile) **only after explicit human approval** — either
  `POST /actions/<id>/approve` (token-gated) or **Slack Approve/Reject buttons** (enable Slack
  Interactivity with Request URL `…/slack/interactions` and set `slack.signing_secret_env`; clicks are
  HMAC-verified). The envelope is re-checked at execution and every action is audit-logged.
- **Unattended (`actions.mode: auto`)**: executes eligible actions with **no human in the loop** — but only
  *reversible* ops, only above `min_confidence`, rate-limited, and **instantly haltable** via
  `POST /actions/pause` (`/resume`). Start with `dry_run: true`, scope which incidents it acts on via the
  trigger policy, and watch the audit log + delivered summary. Irreversible actions are never auto-run.
- **Forge**: writes issues/PRs to the one KB repo you configure, via the scoped GitHub App.
- **Secrets**: referenced by env-var name from a `Secret` you control; nothing is inlined.

## Next

- [Integrations]({{< relref "/docs/integrations/_index.md" >}}) — every trigger, LLM, data source,
  notifier, and forge RunLore plugs into, each with a minimal config and a local-verification recipe.
- [CLI reference]({{< relref "/docs/reference/cli.md" >}}) — the full `lore demo` / `lore investigate`
  flag and env-var reference from the tiers above.
- [Configuration]({{< relref "/docs/configuration/configuration.md" >}}) — every config key, organized by subsystem.
- [Troubleshooting]({{< relref "troubleshooting.md" >}}) — why an investigation didn't start, timed out, or didn't file a PR.
- [Security model]({{< relref "/docs/security/security-model.md" >}}) — read-only posture, redaction, RBAC, the action gate.
- [Upgrade & uninstall]({{< relref "upgrade-uninstall.md" >}}) — `helm upgrade`/`uninstall`, what persists, and cleanup.
- [Design]({{< relref "design.md" >}}) — architecture and the autonomy ladder.
- [CONTRIBUTING.md](https://github.com/Smana/runlore/blob/main/CONTRIBUTING.md) — run the full feature suite locally on k3d.
