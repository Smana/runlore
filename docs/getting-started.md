# Getting Started (Kubernetes)

This guide deploys RunLore into a real cluster: it reacts to incidents, investigates them with an
LLM (grounded in your knowledge catalog), delivers findings to chat, and — optionally — curates what
it learns back to a Git repo as pull requests.

**Running in-cluster (`lore serve`, via Helm) is the recommended way to run RunLore** — that's how it
reacts to incidents continuously and closes the Learn loop. The CLI (`lore investigate "<symptom>"`) is
for one-off local runs against the same engine; see [CONTRIBUTING.md](../CONTRIBUTING.md) for that.

RunLore is **read-only on your cluster**: it never mutates workloads. Its only writes go to the Git
forge (issues/PRs on a repo you designate).

> For local development / testing on k3d, see [CONTRIBUTING.md](../CONTRIBUTING.md) instead.

## Prerequisites

### Required

- A **Kubernetes cluster running [Flux](https://fluxcd.io/flux/installation/) or
  [Argo CD](https://argo-cd.readthedocs.io/en/stable/getting_started/)** — select with
  `config.gitops.engine` (`flux` default, or `argocd`). The what-changed spine + failure trigger read
  the engine's resources (Flux `Kustomization`/`GitRepository`, or Argo CD `Application`s). Any
  conformant cluster works — EKS, GKE, AKS, or local [k3d](https://k3d.io/) / [kind](https://kind.sigs.k8s.io/)
  (follow each project's install docs); RunLore only needs Flux or Argo CD running on it.
- An **LLM** — either an **OpenAI-compatible** endpoint (in-cluster
  [vLLM](https://github.com/vllm-project/vllm), [Ollama](https://ollama.com/), OpenAI, OpenRouter) or
  **native Anthropic** (`model.provider: anthropic`). Keep it in-cluster if you don't want telemetry to
  leave your boundary.
- `kubectl` + `helm` (v3.12+).

### Optional (but recommended)

- A **GitHub App** for curation — [step 2](#step-2-github-app-for-curation-optional). **Without it the
  Learn loop (curation) is disabled**: RunLore still reacts and investigates, but it can't write what it
  learns back to your KB repo. Since the learning loop is RunLore's differentiator, this is **strongly
  recommended**.

### Optional

- A **metrics** backend (VictoriaMetrics/Prometheus), a **logs** backend (VictoriaLogs),
  and/or a **network-flow** source — they enable the `query_metrics` / `query_logs` / `network_drops`
  investigation tools. The network signal is **pluggable and CNI-agnostic**: Cilium Hubble, **AWS VPC
  Flow Logs** (any AWS VPC, incl. EKS with the AWS VPC CNI), or **GCP Firewall Logs** (any GCP VPC,
  incl. GKE). RunLore does **not** assume Cilium.
- **AWS** read-only access — enables the `cloud_what_changed` (CloudTrail) and
  `cloud_resource_health` (EC2/ASG/EKS) tools, the cloud-control-plane "what changed" lens for infra
  changes outside GitOps. Auth is in-cluster identity (**EKS Pod Identity** or IRSA) — no static keys.
  See [step 4b](#step-4b-aws-cloud-provider-optional).
- A **Slack incoming webhook** and/or a **Matrix** account for delivery.
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
   slug is just the entry's identity — not indexed, not a frontmatter field (the Curator names learned
   entries `slugify(title).md`). Put them at the repo root or in subfolders (`playbooks/`, `incidents/`,
   …); the whole tree is indexed recursively. Example:

   ```markdown
   ---
   type: Playbook
   title: HelmRelease upgrade failure
   description: A Helm chart bump leaves the release Ready=False.
   tags: [flux, helmrelease, upgrade]
   ---
   # Symptom
   Ready=False after a chart bump; often a DB migration that didn't complete.

   # Checks
   - `flux get hr -A | grep -i false`
   - the rendered diff between the two chart versions
   ```

   `index.md` and `log.md` are reserved (a human listing + a changelog) and skipped by the indexer — as
   are dot-files. What `kb_search` actually matches is the frontmatter `title`/`description`/`tags` plus
   the body, **not** the filename — so write those well. Seed it with whatever runbooks you already
   have; the agent gets sharper at *your* platform as the catalog grows.

3. **Make it available in-cluster.** Two options:

   **Option A — Git-sync (recommended; closes the read/write loop).** RunLore clones the repo and
   re-pulls it on an interval, re-indexing automatically. When curation merges a PR into this repo, the
   new knowledge flows straight back into what the agent searches — no manual step. Configure it under
   `config.catalog.git` ([step 4](#step-4-configure-and-install)) and set `catalog.gitSync: true` (which
   mounts a writable mirror). A **private** repo authenticates with the **same curation GitHub App** by
   default ([step 2](#step-2-github-app-for-curation-optional)) — one credential for both reads and
   writes; set `git.token_env` only to use a different token.

   **Option B — ConfigMap (static).** Mount a snapshot; refresh it yourself when the repo changes:

   ```bash
   git clone https://github.com/your-org/runlore-kb /tmp/runlore-kb
   kubectl -n runlore create configmap runlore-catalog \
     --from-file=/tmp/runlore-kb/ \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

---

## Step 2 — GitHub App for curation (optional)

The **Curator** writes findings back to your KB repo: confident root causes become a **PR** drafting an
OKF entry; less-confident ones become an **issue**. Auth is a **GitHub App** — the secure choice over a
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
   | Issues | **Read & write** | open issues for lower-confidence findings |

   If your Flux source repos are **private** and you want real Git diffs from them, also grant
   **Contents: Read-only** and install the App on those repos. Public source repos need nothing.
4. **Create**, then **Generate a private key** — download the `.pem` (you only see it once).
5. Note the **App ID** (on the App's page).
6. **Install App** → install it on **only the specific repos** it needs (the KB repo, plus any private
   source repos) — *not* "All repositories". Open the installation and note the **Installation ID**
   (the number in the install settings URL: `.../installations/<id>`).

### Security best practices

- **Never commit the private key** or put it in `values.yaml`. Store it in a `Secret`
  ([step 3](#step-3-credentials)) — ideally synced from a vault via External Secrets.
- **Scope the installation** to specific repos, and grant only the three write permissions above.
- Installation tokens are already **short-lived (1h) and auto-refreshed** — there is no long-lived
  token to leak. **Rotate the App private key** periodically anyway.
- Restrict who can administer the App in your org.
- RunLore's writes are confined to the forge — it has **no cluster-mutating permissions**.

---

## Step 3 — Credentials

Create a `Secret` with the credentials your config references by env-var name. In production, prefer an
`ExternalSecret` that pulls these from your vault instead of `kubectl create secret`.

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=OPENAI_API_KEY='<model-api-key-or-omit-if-keyless>' \
  --from-literal=SLACK_WEBHOOK_URL='https://hooks.slack.com/services/...' \
  --from-literal=MATRIX_TOKEN='<matrix-access-token>' \
  --from-file=GITHUB_APP_PRIVATE_KEY=/path/to/app-private-key.pem
```

Only include the keys you use. The chart injects the whole Secret as env vars via `envFrom`, and the
config references each by its env-var name (`api_key_env`, `webhook_url_env`, `private_key_env`, …).

---

## Step 4 — Configure and install

RunLore installs **in-cluster with one Helm command** (this is the recommended deployment). You give it
a `values.yaml` and run `helm install` — jump to **[Install](#install)** below if you just want the
command.

### The configuration you'll provide

Create a `values.yaml`. This is a complete production-style example — trim what you don't use:

```yaml
image:
  repository: ghcr.io/smana/runlore
  tag: ""            # defaults to the chart appVersion; pin in production

# HA: 2+ replicas, one active leader (the rest hot standby). See leader_election below.
replicaCount: 2

# Inject the whole Secret as env vars (referenced by *_env config keys below).
envFrom:
  - secretRef:
      name: runlore-secrets

# Catalog source (step 1). Option A — git-sync (recommended): a writable mirror.
catalog:
  gitSync: true
  mountPath: /var/lib/runlore/catalog
  # Option B — static ConfigMap instead:
  # configMap: runlore-catalog

config:
  # GitOps engine the what-changed spine + failure watch read.
  gitops:
    engine: flux          # or "argocd"
  # Enable sources: a key under `sources.<name>` turns that source on. Presence is
  # enablement; the value is the source's own config. The webhook auth token stays
  # server-level (server.webhook_token_env).
  sources:
    alertmanager: {}           # enable the Alertmanager/VMAlert webhook source
    gitops:
      enabled: true            # also react to Flux/Argo CD Ready=False
  # React: only investigate what matters (controls noise + LLM cost). These are the
  # match/failure POLICIES; enablement lives under `sources` above.
  triggers:
    incidents:
      match:
        severity: [critical, warning]   # match against the alert's labels
        # environment: [prod]           # only matches alerts that CARRY an `environment`
                                        # label — omit it if yours don't, or nothing fires
      dedup: { window: 30m }
    # gitops_failures:
    #   debounce: 60s          # re-check window before investigating a Flux failure

  # Investigate: the model + the catalog the loop searches.
  model:
    base_url: http://vllm.llm.svc:8000/v1   # any OpenAI-compatible endpoint
    model: <your-model-name>
    api_key_env: OPENAI_API_KEY             # omit/empty for keyless (in-cluster vLLM/Ollama)
    # — or native Anthropic instead:
    # provider: anthropic
    # model: claude-sonnet-4-6
    # api_key_env: ANTHROPIC_API_KEY        # base_url defaults to api.anthropic.com
  catalog:
    dir: /var/lib/runlore/catalog           # must match catalog.mountPath above
    git:                                     # omit this block if using a static ConfigMap
      url: https://github.com/your-org/runlore-kb
      branch: main
      interval: 5m
      # token_env: KB_GIT_TOKEN              # optional; private repos reuse the curation GitHub App by default
    # Instant recall: skip the LLM loop when the catalog has a high-confidence match
    # for the symptom (faster, cheaper). Off by default; tune min_score for your
    # catalog — BM25 scores are corpus-dependent (observe the "score=" logs).
    # instant_recall: { enabled: true, min_score: 0.3 }

  # Investigate signals (optional) — enable the query_metrics / query_logs tools.
  metrics:
    url: http://vmsingle.observability.svc:8429       # PromQL API base (VictoriaMetrics, or Prometheus on :9090)
  logs:
    url: http://victorialogs.observability.svc:9428   # VictoriaLogs base (LogsQL)
  # Network-flow signal (optional) — the network_drops tool. PLUGGABLE and CNI-agnostic:
  # RunLore does NOT assume Cilium. Pick the provider matching your environment; an empty/
  # absent `network` block disables the tool. Choose ONE:
  network:
    provider: hubble                                  # Cilium Hubble (requires the Cilium CNI)
    hubble:
      url: hubble-relay.kube-system:80                # Hubble Relay gRPC host:port
    # provider: aws-vpc-flow-logs                     # any AWS VPC (incl. EKS + AWS VPC CNI) — REJECT records
    # aws:
    #   region: eu-west-3
    #   log_group: /aws/vpc/flowlogs                  # CloudWatch Logs group receiving the VPC Flow Logs
    # provider: gcp-firewall-logs                     # any GCP VPC (incl. GKE) — DENIED firewall connections
    # gcp:
    #   project: my-gcp-project
  # Cloud context (AWS) — enables cloud_what_changed (CloudTrail) + cloud_resource_health
  # (EC2/ASG/EKS). Read-only; auth is in-cluster identity (no keys). See step 4b.
  cloud:
    provider: aws
    region: eu-west-3
    cluster_name: your-cluster        # scopes EKS nodegroup / ASG queries

  # Deliver: one or both.
  notify:
    slack:
      webhook_url_env: SLACK_WEBHOOK_URL
      # — or a bot token (chat.postMessage) instead of an incoming webhook; the bot
      #   must be a member of the channel (invite it / `conversations.join`):
      # bot_token_env: SLACK_BOT_TOKEN              # xoxb-… (takes precedence over webhook_url_env)
      # channel: C0123456789                        # channel ID or name to post to
      # signing_secret_env: SLACK_SIGNING_SECRET   # enables rung-2 Approve/Reject buttons
    matrix:
      homeserver: https://matrix.org
      room_id: "!yourroom:matrix.org"
      access_token_env: MATRIX_TOKEN

  # Learn: curate findings to your KB repo (omit this block to disable).
  forge:
    kb_repo: your-org/runlore-kb            # the repo from step 1
    base_branch: main
    # github_api_url: https://ghe.example.com/api/v3   # only for GitHub Enterprise Server
    github_app:
      app_id: 123456                         # from step 2
      installation_id: 7654321               # from step 2
      private_key_env: GITHUB_APP_PRIVATE_KEY

  # Autonomy ladder. Default (omitted) = off = read-only findings only.
  #   suggest — propose envelope-filtered remediations, never executed.
  #   approve — register them for human approval (curl or Slack buttons); an approved
  #             action executes a reversible Flux op (suspend/resume/reconcile).
  #   auto    — execute eligible actions WITHOUT approval. Heavily gated (below).
  # approve + auto require chart rbac.allowActions=true.
  # actions:
  #   mode: approve
  #   approval_token_env: APPROVAL_TOKEN   # gate the approval + kill-switch endpoints
  #   allow:
  #     reversible_only: true              # withhold irreversible suggestions
  #     max_blast_radius: 5
  #     kinds: [HelmRelease, Kustomization, Application]
  #   # rung-3 unattended execution (mode: auto). Layered safety: auto ONLY ever runs
  #   # reversible actions, and every decision is logged + delivered. Start with dry_run.
  #   auto:
  #     dry_run: true                      # log "would execute" without executing
  #     min_confidence: 0.85               # only auto-execute above this confidence
  #     max_per_window: 3                  # rate limit (anti-storm)
  #     window: 1h
  #   # Kill-switch (instant, no redeploy): POST /actions/pause | /actions/resume
  #   # (X-Approval-Token). Scope which incidents auto acts on via the trigger policy.
  #   # NOTE: floats like min_confidence must be set via a values file or
  #   # `helm --set-json` — plain `--set x=0.85` is coerced to a string.

  # HA toggle (default on; harmless with 1 replica).
  leader_election:
    enabled: true
```

### Install

With the `values.yaml` above, deploy RunLore with a single command:

```bash
helm install runlore deploy/helm/runlore -n runlore --create-namespace -f values.yaml
```

> The chart needs the `deploy/helm/runlore` directory from this repo. A packaged chart repo is on the
> roadmap; for now, `git clone` and install from the path (or `helm package` it yourself).

---

## Step 4b — AWS cloud provider (optional)

Enables the `cloud_what_changed` (CloudTrail) and `cloud_resource_health` (EC2/ASG/EKS) tools so the
agent can see infra changes that never touched GitOps. **Read-only**, authenticated with **in-cluster
identity** — no static AWS keys.

1. **Config** — add the `config.cloud` block ([step 4](#step-4-configure-and-install)): `provider: aws`,
   your `region`, and the EKS `cluster_name` (scopes nodegroup/ASG queries).

2. **IAM (read-only)** — grant the agent's ServiceAccount a role with *only* these actions, no writes:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       { "Effect": "Allow", "Action": ["cloudtrail:LookupEvents"], "Resource": "*" },
       { "Effect": "Allow", "Action": [
           "ec2:DescribeInstances", "ec2:DescribeInstanceStatus", "ec2:DescribeTags",
           "autoscaling:DescribeAutoScalingGroups", "autoscaling:DescribeScalingActivities",
           "eks:DescribeCluster", "eks:DescribeNodegroup", "eks:ListNodegroups"
         ], "Resource": "*" }
     ]
   }
   ```

   Bind it to the `runlore` ServiceAccount via **[EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html)**
   (preferred) or IRSA. The SDK's default credential chain picks it up — nothing to configure in RunLore.

3. **Cilium clusters only** — the EKS Pod Identity credential endpoint runs on the node host network
   (`169.254.170.23:80`), which Cilium classifies as the `host` entity. A plain Kubernetes NetworkPolicy
   **cannot** match it, so the SDK's credential fetch is silently dropped and the cloud tools hang. Set
   `networkPolicy.awsPodIdentity: true` (chart value) to render a `CiliumNetworkPolicy` that allows it:

   ```yaml
   networkPolicy:
     enabled: true
     awsPodIdentity: true   # CiliumNetworkPolicy: egress to host:80 for the Pod Identity endpoint
   ```

   Confirm with Hubble if calls hang: `hubble observe --pod runlore/<pod> --verdict DROPPED` showing
   `169.254.170.23:80 (host) … DROPPED` is this exact issue.

4. **Memory** — a thorough run (a "pro" model over the full step budget with the cloud tools) is the
   memory peak; the chart default limit is `1.5Gi`. Lower it only if you use a smaller model / fewer tools.

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

With 2+ replicas, only the **leader** is Ready, so the Service routes webhooks to it automatically.

---

## Step 6 — Verify

```bash
# the leader becomes Ready; standbys stay NotReady (expected)
kubectl -n runlore get pods
kubectl -n runlore get lease runlore-leader -o jsonpath='{.spec.holderIdentity}'; echo

# startup wiring
kubectl -n runlore logs deploy/runlore | grep -E 'catalog loaded|using LLM investigator|curator enabled|watching gitops failures|serving'
```

Expected lines: `catalog loaded … entries=N`, `using LLM investigator`, `watching gitops failures`,
`curator enabled` (if configured), `runlore serving`.

Fire a test: trigger a `critical`/`prod` alert (or `flux suspend`+break a Kustomization). You should see
`msg=incident … investigate=true` → `msg=findings …`, a message in Slack/Matrix, and (if curation is on)
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
- **`wont-fix`** — closed without a Playbook.

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

- [Design](design.md) — architecture and the autonomy ladder.
- [CONTRIBUTING.md](../CONTRIBUTING.md) — run the full feature suite locally on k3d.
