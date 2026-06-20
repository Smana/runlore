# Getting Started (Kubernetes)

This guide deploys RunLore into a real cluster: it reacts to incidents, investigates them with an
LLM (grounded in your knowledge catalog), delivers findings to chat, and — optionally — curates what
it learns back to a Git repo as pull requests.

RunLore is **read-only on your cluster**: it never mutates workloads. Its only writes go to the Git
forge (issues/PRs on a repo you designate).

> For local development / testing on k3d, see [CONTRIBUTING.md](../CONTRIBUTING.md) instead.

## Prerequisites

- A Kubernetes cluster running **Flux** (the what-changed spine + failure trigger read Flux
  `Kustomization`/`GitRepository` resources; ArgoCD is on the roadmap).
- An **OpenAI-compatible** model endpoint — in-cluster [vLLM](https://github.com/vllm-project/vllm),
  [Ollama](https://ollama.com/), OpenAI, or OpenRouter. (Native Anthropic is on the roadmap; today,
  reach Claude via an OpenAI-compatible gateway such as OpenRouter.) Keep it in-cluster if you don't
  want telemetry to leave your boundary.
- `kubectl` + `helm` (v3.12+).
- Optional: a **metrics** backend (VictoriaMetrics/Prometheus), a **logs** backend (VictoriaLogs),
  and/or **Cilium Hubble** (Relay) — they enable the `query_metrics` / `query_logs` / `network_drops`
  investigation tools.
- Optional: a **Slack incoming webhook** and/or a **Matrix** account for delivery.
- Optional: a **GitHub App** for curation (the Learn loop) — [step 2](#step-2-github-app-for-curation-optional).
- Optional: [External Secrets Operator](https://external-secrets.io/) to sync credentials from a vault
  (recommended over raw `Secret`s in production).

---

## Step 1 — Create the knowledge-catalog repo

RunLore reads (and, with curation, writes) an **[OKF](https://github.com/GoogleCloudPlatform/knowledge-catalog)
knowledge catalog**: a Git repo of markdown files, each with YAML frontmatter. This is *your* portable
knowledge base — runbooks, past incidents, platform constraints.

1. Create a new (private) Git repo, e.g. `your-org/runlore-kb`.
2. Add entries as `<slug>.md` at the repo root:

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

   Filenames `index.md` and `log.md` are reserved and skipped. Seed it with whatever runbooks you
   already have — the agent gets sharper at *your* platform as the catalog grows.

3. **Make it available in-cluster.** Two options:

   **Option A — Git-sync (recommended; closes the read/write loop).** RunLore clones the repo and
   re-pulls it on an interval, re-indexing automatically. When curation merges a PR into this repo, the
   new knowledge flows straight back into what the agent searches — no manual step. Configure it under
   `config.catalog.git` ([step 4](#step-4-configure-and-install)) and set `catalog.gitSync: true` (which
   mounts a writable mirror). For a private repo, point `token_env` at a read-scoped token in the Secret.

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
  # React: only investigate what matters (controls noise + LLM cost).
  triggers:
    incidents:
      enabled: true
      match:
        severity: [critical]
        environment: [prod]
      dedup: { window: 30m }
    gitops_failures:
      enabled: true            # also react to Flux Ready=False

  # Investigate: the model + the catalog the loop searches.
  model:
    base_url: http://vllm.llm.svc:8000/v1   # any OpenAI-compatible endpoint
    model: <your-model-name>
    api_key_env: OPENAI_API_KEY             # omit/empty for keyless (in-cluster vLLM/Ollama)
  catalog:
    dir: /var/lib/runlore/catalog           # must match catalog.mountPath above
    git:                                     # omit this block if using a static ConfigMap
      url: https://github.com/your-org/runlore-kb
      branch: main
      interval: 5m
      # token_env: KB_GIT_TOKEN              # for a private repo (key in the Secret)

  # Investigate signals (optional) — enable the query_metrics / query_logs tools.
  metrics:
    url: http://vmsingle.observability.svc:8429       # PromQL API base (VictoriaMetrics, or Prometheus on :9090)
  logs:
    url: http://victorialogs.observability.svc:9428   # VictoriaLogs base (LogsQL)
  network:
    url: hubble-relay.kube-system:80                  # Cilium Hubble Relay (gRPC host:port)

  # Deliver: one or both.
  notify:
    slack:
      webhook_url_env: SLACK_WEBHOOK_URL
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

  # HA toggle (default on; harmless with 1 replica).
  leader_election:
    enabled: true
```

Install:

```bash
helm install runlore deploy/helm/runlore -n runlore --create-namespace -f values.yaml
```

> The chart needs the `deploy/helm/runlore` directory from this repo. A packaged chart repo is on the
> roadmap; for now, `git clone` and install from the path (or `helm package` it yourself).

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

## What RunLore can and cannot do

- **Cluster**: read-only. It reads Flux resources, metrics (PromQL), logs (LogsQL), and network flows (Hubble), and never writes
  to the cluster. RBAC is limited to watching `Kustomization`s, reading `GitRepository`s, and its own
  leader-election `Lease`.
- **Forge**: writes issues/PRs to the one KB repo you configure, via the scoped GitHub App.
- **Secrets**: referenced by env-var name from a `Secret` you control; nothing is inlined.

## Next

- [Design](design.md) — architecture and the autonomy ladder.
- [CONTRIBUTING.md](../CONTRIBUTING.md) — run the full feature suite locally on k3d.
