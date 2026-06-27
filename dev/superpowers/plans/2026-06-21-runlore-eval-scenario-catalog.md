# RunLore Eval Harness — Scenario Catalog & Baseline Campaign (Plan 2 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax. **Prerequisite:** Plan 1 (`...-engine.md`) is merged and green — `lore eval --live` exists. This plan authors *data* and runs the *first campaign* against mycluster-0; it writes almost no Go.

**Goal:** Author the 12-scenario catalog (`eval/scenarios/` YAML + throwaway manifests) and `eval/rubric.md`, then run the first baseline live-fire campaign and capture the report as the current-state quality assessment + the top-gaps list that seeds workstream B (the learning-workflow redesign).

**Architecture:** Each scenario is a YAML file consumed by the Plan-1 `LiveRunner`. Induced scenarios ship a reversible manifest under `eval/scenarios/manifests/`; `setup`/`teardown` shell out to `kubectl`/`flux`. Natural scenarios carry a `precheck` that SKIPs gracefully if the failure isn't present. All induced faults live in the throwaway `runlore-eval` namespace.

**Safety invariants (verify before each run):**
- Cloud is **describe-only** — no scenario induces an AWS mutation.
- Induced faults are confined to namespace `runlore-eval` (or a labelled throwaway app); `teardown` always reverts (Plan-1 runs it via `defer`).
- `kubectl`/`flux`/`aws` CLIs must be on PATH and pointed at `mycluster-0` (`kubectl config current-context`).
- Curation stays off: the Plan-1 `--live` path never builds a curator, so no KB writes happen during the campaign.

---

## Task 1: Scaffold + rubric + the 3 natural scenarios

**Files:** Create `eval/rubric.md`, `eval/scenarios/{harbor-registry-iam-quota,harbor-valkey-dependency-down,vector-cilium-ip-exhaustion}.yaml`

- [ ] **Step 1: Create the namespace scaffold doc + rubric**

Create `eval/rubric.md`:

```markdown
# RunLore eval rubric

Two grading tracks (see docs/superpowers/specs/2026-06-21-runlore-eval-harness-design.md §5).

## Track A — coverage (deterministic)
From the recorded tool-call trace. `coverage = |mandatory expected_sources touched| / |expected_sources|`.
Pass requires `coverage == 1.0`. `optional_sources` are bonus, never gating. Any errored tool is flagged.

## Track B — RCA quality (LLM-judge, blind, stronger model)
| Dimension | Max | Meaning |
|---|---|---|
| root_cause | 3 | 0 wrong / 1 symptom-only / 2 correct-shallow / 3 correct + true root |
| evidence | 3 | cited facts pertinent & true |
| solution | 3 | suggested action vs expected: correct, actionable, reversibility right |
| description | 3 | clarity, completeness, honest unresolved |
| calibration | 2 | high confidence only when correct; confident-wrong penalised hardest |

## Pass gate (per scenario, median over N=3)
`root_cause >= 2` AND `coverage == 1.0` AND no confident-wrong run.
```

- [ ] **Step 2: Author the natural scenarios (zero setup; precheck → SKIP if repaired)**

Create `eval/scenarios/harbor-registry-iam-quota.yaml`:

```yaml
id: harbor-registry-iam-quota
category: dependency
description: harbor-registry CreateContainerConfigError — Crossplane access key Secret missing username
invasive: false
precheck: kubectl get pods -n tooling -l app=harbor,component=registry -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}' | grep -q CreateContainerConfigError
trigger:
  mode: cli
  symptom: harbor registry pod not starting in namespace tooling (CreateContainerConfigError)
  namespace: tooling
ground_truth:
  root_cause: >-
    Crossplane accesskey/xplane-harbor hit the AWS IAM AccessKeysPerUser:2 quota, so
    Secret xplane-harbor-access-key was created without a username key, leaving the
    registry container unable to start (CreateContainerConfigError).
  expected_sources: [kubernetes]
  optional_sources: [aws, gitops]
  expected_action: delete an old IAM access key for the user so Crossplane can mint a new one
  must_reach_root: true
```

Create `eval/scenarios/harbor-valkey-dependency-down.yaml`:

```yaml
id: harbor-valkey-dependency-down
category: dependency
description: harbor core/jobservice/nginx CrashLoopBackOff — valkey (redis) dependency down
invasive: false
precheck: kubectl get pods -n tooling -l app=harbor,component=core -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}' | grep -q CrashLoopBackOff
trigger:
  mode: cli
  symptom: harbor core is CrashLoopBackOff in namespace tooling
  namespace: tooling
ground_truth:
  root_cause: >-
    harbor-valkey-primary:6379 connection refused (valkey is down), so harbor
    core/jobservice/nginx crash on startup. The dependency outage, not harbor itself,
    is the root cause.
  expected_sources: [kubernetes, logs]
  optional_sources: []
  expected_action: restore the harbor-valkey-primary service (investigate why valkey is down)
  must_reach_root: true
```

Create `eval/scenarios/vector-cilium-ip-exhaustion.yaml`:

```yaml
id: vector-cilium-ip-exhaustion
category: node
description: victoria-logs-vector stuck ContainerCreating — Cilium IPAM IP exhaustion on the node
invasive: false
precheck: kubectl get pods -n observability -l app.kubernetes.io/name=vector -o jsonpath='{.items[*].status.phase}' | grep -q Pending
trigger:
  mode: cli
  symptom: victoria-logs-vector pod stuck ContainerCreating in namespace observability
  namespace: observability
ground_truth:
  root_cause: >-
    Cilium IPAM has no IPs available on the node ("no IPs currently available on the
    node"), so the vector pod sandbox cannot get an address and stays ContainerCreating.
  expected_sources: [kubernetes]
  optional_sources: [network]
  expected_action: free or add node IP capacity (Cilium IPAM pool) so the pod can be scheduled
  must_reach_root: true
```

- [ ] **Step 3: Validate parse + prechecks against the live cluster**

Run:
```bash
cd /home/smana/Sources/runlore
go build -o /tmp/lore ./cmd/lore
# parse-only: scenarios load without error (uses the real config but no model call yet)
/tmp/lore eval --live --scenarios eval/scenarios --config <your-runlore.yaml> --n 1 2>&1 | head -5 || true
# confirm each precheck reflects reality (exit 0 = present, non-0 = absent => will SKIP):
kubectl get pods -n tooling -l app=harbor,component=registry
kubectl get pods -n tooling -l app=harbor,component=core
kubectl get pods -n observability -l app.kubernetes.io/name=vector
```
Expected: scenarios parse; adjust each `precheck` label selector to match the real pod labels on mycluster-0 (the selectors above are best-effort — **confirm with `kubectl get pods --show-labels`** and fix before relying on SKIP behaviour).

- [ ] **Step 4: Commit**

```bash
cd /home/smana/Sources/runlore
git add eval/rubric.md eval/scenarios/harbor-registry-iam-quota.yaml eval/scenarios/harbor-valkey-dependency-down.yaml eval/scenarios/vector-cilium-ip-exhaustion.yaml
git commit -m "eval(scenarios): rubric + 3 natural scenarios (harbor x2, vector)"
```

---

## Task 2: Induced "what-changed" scenarios (4, 5) + manifests

**Files:** Create `eval/scenarios/{gitops-bad-image-tag,gitops-broken-kustomization}.yaml`, `eval/scenarios/manifests/{eval-victim-app.yaml,eval-victim-bad-kustomization.yaml}`

- [ ] **Step 1: Throwaway victim app manifest (a Deployment we can break)**

Create `eval/scenarios/manifests/eval-victim-app.yaml`:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: runlore-eval
  labels: { runlore.io/eval: "true" }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: eval-victim
  namespace: runlore-eval
  labels: { app: eval-victim, runlore.io/eval: "true" }
spec:
  replicas: 1
  selector: { matchLabels: { app: eval-victim } }
  template:
    metadata: { labels: { app: eval-victim } }
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.9   # healthy baseline; scenarios mutate this
          resources:
            requests: { cpu: 10m, memory: 16Mi }
            limits: { cpu: 50m, memory: 32Mi }
```

- [ ] **Step 2: Scenario 4 — bad image tag**

Create `eval/scenarios/gitops-bad-image-tag.yaml`:

```yaml
id: gitops-bad-image-tag
category: what-changed
description: image patched to a non-existent tag -> ImagePullBackOff
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-app.yaml
  - kubectl -n runlore-eval set image deploy/eval-victim app=registry.k8s.io/pause:v9.9.9-does-not-exist
  - kubectl -n runlore-eval rollout status deploy/eval-victim --timeout=20s || true
trigger:
  mode: cli
  symptom: eval-victim pods not starting in namespace runlore-eval (image pull errors)
  namespace: runlore-eval
ground_truth:
  root_cause: >-
    The eval-victim Deployment image was changed to tag v9.9.9-does-not-exist, which
    cannot be pulled (ImagePullBackOff/ErrImagePull). A change is the cause.
  expected_sources: [kubernetes, logs]
  optional_sources: [gitops]
  expected_action: revert the image tag to a valid one (roll back the change)
  must_reach_root: true
teardown:
  - kubectl delete -f eval/scenarios/manifests/eval-victim-app.yaml --ignore-not-found
```

> **Note on `expected_sources`:** the victim is a plain Deployment (not Flux-managed), so `what_changed`/`gitops` is *optional* here — the change shows in pod status/events, not a Git diff. Scenario 5 covers the true Flux path.

- [ ] **Step 3: Scenario 5 — broken Flux Kustomization**

Create `eval/scenarios/manifests/eval-victim-bad-kustomization.yaml`:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: eval-victim-broken
  namespace: flux-system
  labels: { runlore.io/eval: "true" }
spec:
  interval: 1m
  path: ./this/path/does/not/exist
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  timeout: 30s
```

Create `eval/scenarios/gitops-broken-kustomization.yaml`:

```yaml
id: gitops-broken-kustomization
category: what-changed
description: a Flux Kustomization pointed at a non-existent path -> Ready=False
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-bad-kustomization.yaml
  - sleep 20  # let Flux reconcile and report Ready=False
trigger:
  mode: cli
  symptom: Flux Kustomization eval-victim-broken is not ready in flux-system
  namespace: flux-system
ground_truth:
  root_cause: >-
    Kustomization/eval-victim-broken references spec.path ./this/path/does/not/exist,
    which does not exist in the source, so the build fails and the Kustomization is
    Ready=False. The source GitRepository itself is healthy.
  expected_sources: [gitops]
  optional_sources: [kubernetes]
  expected_action: correct spec.path (or remove the Kustomization)
  must_reach_root: true
teardown:
  - kubectl delete -f eval/scenarios/manifests/eval-victim-bad-kustomization.yaml --ignore-not-found
```

- [ ] **Step 4: Dry-run setup/teardown once, by hand, to confirm reversibility**

Run:
```bash
kubectl apply -f eval/scenarios/manifests/eval-victim-app.yaml
kubectl -n runlore-eval set image deploy/eval-victim app=registry.k8s.io/pause:v9.9.9-does-not-exist
kubectl -n runlore-eval get pods   # expect ImagePullBackOff/ErrImagePull
kubectl delete -f eval/scenarios/manifests/eval-victim-app.yaml --ignore-not-found
kubectl get ns runlore-eval        # expect: not found (cleaned)
```
Expected: fault appears, teardown removes the namespace. Repeat the same for the bad Kustomization, confirming `flux get kustomization eval-victim-broken` shows `Ready=False`, then delete.

- [ ] **Step 5: Commit**

```bash
cd /home/smana/Sources/runlore
git add eval/scenarios/gitops-bad-image-tag.yaml eval/scenarios/gitops-broken-kustomization.yaml eval/scenarios/manifests/eval-victim-app.yaml eval/scenarios/manifests/eval-victim-bad-kustomization.yaml
git commit -m "eval(scenarios): what-changed scenarios (bad image tag, broken kustomization)"
```

---

## Task 3: Saturation, network, DNS scenarios (6, 7, 11) + manifests

**Files:** Create `eval/scenarios/{saturation-mem-pressure,network-policy-drop,dns-resolution-failure}.yaml`, manifests for the network-drop and DNS faults.

- [ ] **Step 1: Scenario 6 — memory saturation**

Create `eval/scenarios/saturation-mem-pressure.yaml`:

```yaml
id: saturation-mem-pressure
category: saturation
description: a memory-hog workload drives OOMKills under a tight limit
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-app.yaml
  - |
    kubectl -n runlore-eval run mem-hog --image=polinux/stress --restart=Never \
      --limits=memory=64Mi --requests=memory=64Mi -- \
      stress --vm 1 --vm-bytes 256M --vm-hang 0 || true
  - sleep 25  # let it OOMKill a few times and emit metrics
trigger:
  mode: cli
  symptom: pod mem-hog repeatedly restarting (OOMKilled) in namespace runlore-eval
  namespace: runlore-eval
ground_truth:
  root_cause: >-
    The mem-hog pod requests 256M but is capped at a 64Mi memory limit, so the kernel
    OOMKills it repeatedly (reason OOMKilled, exit 137). Resource saturation against the
    limit, with no Git change involved.
  expected_sources: [kubernetes]
  optional_sources: [metrics, logs]
  expected_action: raise the memory limit or reduce the workload's footprint
  must_reach_root: true
teardown:
  - kubectl -n runlore-eval delete pod mem-hog --ignore-not-found
  - kubectl delete -f eval/scenarios/manifests/eval-victim-app.yaml --ignore-not-found
```

> If `metrics` must be *mandatory* (to force `query_metrics`), move it from `optional_sources` to `expected_sources` after confirming the agent can scrape mycluster-0 VictoriaMetrics for this pod — verify in the first baseline run, then tighten.

- [ ] **Step 2: Scenario 7 — network policy drop**

Create `eval/scenarios/manifests/eval-victim-netpol-deny.yaml`:

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: eval-victim-deny-egress
  namespace: runlore-eval
  labels: { runlore.io/eval: "true" }
spec:
  endpointSelector:
    matchLabels: { app: eval-victim }
  egress: []   # default-deny all egress for the victim
```

Create `eval/scenarios/network-policy-drop.yaml`:

```yaml
id: network-policy-drop
category: network
description: a default-deny CiliumNetworkPolicy silently drops the victim's egress
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-app.yaml
  - kubectl apply -f eval/scenarios/manifests/eval-victim-netpol-deny.yaml
  - sleep 15
trigger:
  mode: cli
  symptom: eval-victim in namespace runlore-eval cannot reach any dependency (connection timeouts)
  namespace: runlore-eval
ground_truth:
  root_cause: >-
    CiliumNetworkPolicy eval-victim-deny-egress applies an empty egress allowlist
    (default-deny) to app=eval-victim, so all its outbound connections are dropped by
    policy — which presents as application connection timeouts, not an app bug.
  expected_sources: [network]
  optional_sources: [kubernetes, logs]
  expected_action: add an explicit egress allow rule (or remove the deny policy)
  must_reach_root: true
teardown:
  - kubectl delete -f eval/scenarios/manifests/eval-victim-netpol-deny.yaml --ignore-not-found
  - kubectl delete -f eval/scenarios/manifests/eval-victim-app.yaml --ignore-not-found
```

- [ ] **Step 3: Scenario 11 — DNS resolution failure**

Create `eval/scenarios/manifests/eval-victim-dns-broken.yaml`:

```yaml
# A client pod whose only egress allowance OMITS the kube-dns L7 DNS rule, so the
# toFQDNs allowlist never sees resolved IPs — the documented Cilium DNS gotcha.
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: eval-dns-victim-policy
  namespace: runlore-eval
  labels: { runlore.io/eval: "true" }
spec:
  endpointSelector:
    matchLabels: { app: eval-dns-victim }
  egress:
    - toFQDNs:
        - matchName: example.com   # allowed destination, but with NO DNS L7 rule below
    # intentionally NO kube-dns egress with toPorts.rules.dns -> resolution silently breaks
```

Create `eval/scenarios/dns-resolution-failure.yaml`:

```yaml
id: dns-resolution-failure
category: dns
description: a toFQDNs policy missing the DNS L7 rule breaks name resolution silently
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-app.yaml
  - |
    kubectl -n runlore-eval run eval-dns-victim --image=curlimages/curl --restart=Never \
      --labels=app=eval-dns-victim --command -- sleep 3600 || true
  - kubectl apply -f eval/scenarios/manifests/eval-victim-dns-broken.yaml
  - sleep 15
trigger:
  mode: cli
  symptom: pod eval-dns-victim in namespace runlore-eval fails all requests with name resolution errors
  namespace: runlore-eval
ground_truth:
  root_cause: >-
    CiliumNetworkPolicy eval-dns-victim-policy allows toFQDNs but omits the kube-dns
    egress L7 DNS rule (toPorts.rules.dns.matchPattern "*"), so Cilium proxies the DNS
    query but never sees the response IPs — the toFQDNs allowlist has no IPs to match and
    every connection is dropped. DNS appears to "work" while downstream traffic fails.
  expected_sources: [network]
  optional_sources: [logs, kubernetes]
  expected_action: add the kube-dns egress rule with toPorts.rules.dns matchPattern "*"
  must_reach_root: true
teardown:
  - kubectl delete -f eval/scenarios/manifests/eval-victim-dns-broken.yaml --ignore-not-found
  - kubectl -n runlore-eval delete pod eval-dns-victim --ignore-not-found
  - kubectl delete -f eval/scenarios/manifests/eval-victim-app.yaml --ignore-not-found
```

- [ ] **Step 4: Hand-validate each fault appears + reverts (as in Task 2 Step 4), then commit**

```bash
cd /home/smana/Sources/runlore
git add eval/scenarios/saturation-mem-pressure.yaml eval/scenarios/network-policy-drop.yaml eval/scenarios/dns-resolution-failure.yaml eval/scenarios/manifests/eval-victim-netpol-deny.yaml eval/scenarios/manifests/eval-victim-dns-broken.yaml
git commit -m "eval(scenarios): saturation, network-drop, dns-resolution scenarios"
```

---

## Task 4: Cert + storage scenarios (10, 12) + manifests

**Files:** Create `eval/scenarios/{cert-issuance-expiry,pvc-storage-unbound}.yaml` + manifests.

- [ ] **Step 1: Scenario 10 — cert issuance failure**

Create `eval/scenarios/manifests/eval-victim-bad-cert.yaml`:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: eval-bad-cert
  namespace: runlore-eval
  labels: { runlore.io/eval: "true" }
spec:
  secretName: eval-bad-cert-tls
  duration: 1h
  issuerRef:
    name: does-not-exist-issuer   # broken issuerRef -> stuck NotReady
    kind: ClusterIssuer
    group: cert-manager.io
  dnsNames: [eval.runlore-eval.svc]
```

Create `eval/scenarios/cert-issuance-expiry.yaml`:

```yaml
id: cert-issuance-expiry
category: cert
description: a Certificate with a broken issuerRef never becomes Ready
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-app.yaml
  - kubectl apply -f eval/scenarios/manifests/eval-victim-bad-cert.yaml
  - sleep 20
trigger:
  mode: cli
  symptom: Certificate eval-bad-cert in namespace runlore-eval is not Ready; TLS secret missing
  namespace: runlore-eval
ground_truth:
  root_cause: >-
    Certificate eval-bad-cert references ClusterIssuer does-not-exist-issuer, which does
    not exist, so cert-manager cannot issue the cert — it stays Ready=False and the TLS
    secret is never created. Any workload depending on that cert would fail its TLS path.
  expected_sources: [kubernetes]
  optional_sources: [logs]
  expected_action: point issuerRef at a valid (Cluster)Issuer
  must_reach_root: true
teardown:
  - kubectl delete -f eval/scenarios/manifests/eval-victim-bad-cert.yaml --ignore-not-found
  - kubectl delete -f eval/scenarios/manifests/eval-victim-app.yaml --ignore-not-found
```

- [ ] **Step 2: Scenario 12 — PVC unbound**

Create `eval/scenarios/manifests/eval-victim-bad-pvc.yaml`:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: eval-bad-pvc
  namespace: runlore-eval
  labels: { runlore.io/eval: "true" }
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: this-storageclass-does-not-exist
  resources:
    requests: { storage: 1Gi }
---
apiVersion: v1
kind: Pod
metadata:
  name: eval-pvc-consumer
  namespace: runlore-eval
  labels: { app: eval-pvc-consumer, runlore.io/eval: "true" }
spec:
  containers:
    - name: app
      image: registry.k8s.io/pause:3.9
      volumeMounts: [{ name: data, mountPath: /data }]
      resources: { requests: { cpu: 10m, memory: 16Mi }, limits: { cpu: 50m, memory: 32Mi } }
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: eval-bad-pvc }
```

Create `eval/scenarios/pvc-storage-unbound.yaml`:

```yaml
id: pvc-storage-unbound
category: storage
description: a PVC with a non-existent StorageClass never binds; its consumer Pod stays Pending
invasive: true
setup:
  - kubectl apply -f eval/scenarios/manifests/eval-victim-bad-pvc.yaml
  - sleep 15
trigger:
  mode: cli
  symptom: pod eval-pvc-consumer stuck Pending in namespace runlore-eval (volume not bound)
  namespace: runlore-eval
ground_truth:
  root_cause: >-
    PVC eval-bad-pvc requests storageClassName this-storageclass-does-not-exist, which
    has no provisioner, so the claim never binds and the consumer Pod stays Pending
    (FailedScheduling: pod has unbound immediate PersistentVolumeClaims).
  expected_sources: [kubernetes]
  optional_sources: []
  expected_action: use a valid StorageClass (e.g. the cluster's EBS gp3 class)
  must_reach_root: true
teardown:
  - kubectl delete -f eval/scenarios/manifests/eval-victim-bad-pvc.yaml --ignore-not-found
  - kubectl get ns runlore-eval -o name >/dev/null 2>&1 && kubectl delete ns runlore-eval --ignore-not-found || true
```

- [ ] **Step 3: Hand-validate + commit**

```bash
cd /home/smana/Sources/runlore
git add eval/scenarios/cert-issuance-expiry.yaml eval/scenarios/pvc-storage-unbound.yaml eval/scenarios/manifests/eval-victim-bad-cert.yaml eval/scenarios/manifests/eval-victim-bad-pvc.yaml
git commit -m "eval(scenarios): cert-issuance + pvc-storage scenarios"
```

---

## Task 5: Cloud read-only (8) + instant-recall (9)

**Files:** Create `eval/scenarios/{cloud-node-context-readonly,known-pattern-recall}.yaml`

- [ ] **Step 1: Scenario 8 — cloud context (describe-only, no induced change)**

Create `eval/scenarios/cloud-node-context-readonly.yaml`:

```yaml
id: cloud-node-context-readonly
category: cloud
description: agent must surface real recent AWS control-plane changes + node health (read-only)
invasive: false
precheck: aws sts get-caller-identity >/dev/null 2>&1
trigger:
  mode: cli
  symptom: investigate recent AWS-side changes and node-group health for the EKS cluster mycluster-0 in eu-west-3
  namespace: kube-system
ground_truth:
  # No induced fault: this scenario verifies the AWS tools are exercised and that the
  # agent reports a REAL recent CloudTrail mutation + EC2/ASG/EKS health. Pin root_cause
  # at run time to a known recent event (e.g. the Karpenter bottlerocket AMI bump, or a
  # node scale event) by reading CloudTrail before the run. Edit this line per run.
  root_cause: "PIN-AT-RUNTIME: a recent CloudTrail mutating event on mycluster-0 (e.g. EC2 RunInstances/ASG update)"
  expected_sources: [aws]
  optional_sources: [kubernetes]
  expected_action: confirm the change was intended; correlate with any node disruption
  must_reach_root: false
```

> `root_cause` here is intentionally pinned per run — before the campaign, run `aws cloudtrail lookup-events --max-results 20 --region eu-west-3` and paste the most relevant recent mutating event so the judge has a real target. `must_reach_root: false` because this scenario grades *AWS-tool exercise + plausible correlation*, not a single deterministic root cause.

- [ ] **Step 2: Scenario 9 — instant recall (depends on a seeded KB entry)**

Create `eval/scenarios/known-pattern-recall.yaml`:

```yaml
id: known-pattern-recall
category: instant-recall
description: re-firing a known symptom should short-circuit via kb_search, not re-run the full loop
invasive: false
# Precondition: the catalog (cfg.catalog) must contain a KB entry matching this symptom
# AND instant_recall must be enabled in config. If not, this SKIPs.
precheck: test -n "$RUNLORE_RECALL_READY"
trigger:
  mode: cli
  symptom: harbor registry pod CreateContainerConfigError in namespace tooling
  namespace: tooling
ground_truth:
  root_cause: >-
    A known, catalogued pattern: harbor registry CreateContainerConfigError from the
    Crossplane IAM access-key quota. The agent should recall the catalog entry rather
    than re-investigate from scratch.
  expected_sources: [kb]
  optional_sources: [kubernetes]
  expected_action: apply the catalogued resolution (free an IAM access key)
  must_reach_root: true
```

> Scenario 9 is the **bridge to workstream B**: it only passes once a curated KB entry exists and instant-recall is enabled. Until then it SKIPs (precondition guard). During the baseline run, expect it to SKIP — that's the signal that the *read* side of the learning loop has nothing to recall yet.

- [ ] **Step 3: Commit**

```bash
cd /home/smana/Sources/runlore
git add eval/scenarios/cloud-node-context-readonly.yaml eval/scenarios/known-pattern-recall.yaml
git commit -m "eval(scenarios): cloud read-only context + instant-recall bridge scenarios"
```

---

## Task 6: Run the baseline campaign + capture the assessment

**Files:** Produces `eval/reports/<stamp>.md` + `.json` (committed as the baseline).

- [ ] **Step 1: Pre-flight checks**

Run:
```bash
kubectl config current-context              # MUST be mycluster-0
aws sts get-caller-identity                  # cloud describe works
flux get kustomizations | head              # flux CLI works
# pin scenario 8's root_cause from real CloudTrail:
aws cloudtrail lookup-events --max-results 20 --region eu-west-3 \
  --query 'Events[?contains(EventName,`Run`)||contains(EventName,`Update`)||contains(EventName,`Create`)].[EventTime,EventName,Username]' --output table
# edit eval/scenarios/cloud-node-context-readonly.yaml root_cause with the chosen event
```
Expected: context is mycluster-0; CloudTrail returns recent events; scenario 8 pinned.

- [ ] **Step 2: Run the campaign (N=3, stronger judge)**

Use a frontier Anthropic model as the blind judge while the cluster's configured model runs the investigations:

```bash
cd /home/smana/Sources/runlore
go build -o /tmp/lore ./cmd/lore
ANTHROPIC_API_KEY=<key> /tmp/lore eval --live \
  --config <your-runlore.yaml> \
  --scenarios eval/scenarios \
  --record eval/fixtures \
  --report-dir eval/reports \
  --n 3 \
  --judge-provider anthropic --judge-model claude-opus-4-8 --judge-api-key-env ANTHROPIC_API_KEY \
  2>&1 | tee /tmp/eval-run.log
```
Expected: each scenario runs (naturals SKIP if their precheck is absent), a report prints, and `eval/reports/<stamp>.md`/`.json` + `eval/fixtures/*.yaml` are written. **Verify cleanup:** `kubectl get ns runlore-eval` → `not found`.

- [ ] **Step 3: Triage the report — name the top gaps (the workstream-B input)**

Read `eval/reports/<stamp>.md`. Capture, in a short `eval/reports/<stamp>-gaps.md`, the **top 3-5 gaps** the baseline exposes, each as: scenario, dimension that failed (coverage source missing / low root_cause / confident-wrong / tool error), and the likely RunLore fix. This list is the explicit handoff to the learning-workflow redesign (workstream B) and to any RunLore tool/wiring fixes.

- [ ] **Step 4: Commit the baseline**

```bash
cd /home/smana/Sources/runlore
git add eval/reports/ eval/fixtures/ eval/scenarios/cloud-node-context-readonly.yaml
git commit -m "eval(baseline): first live-fire campaign report + recorded fixtures + gap list"
```

- [ ] **Step 5: Verify-before-done gate**

Confirm, in one fresh command run each:
- `kubectl get ns runlore-eval` → `Error ... not found` (no leaked induced faults).
- `ls eval/fixtures/*.yaml | wc -l` → ≥ number of non-skipped scenarios (replay corpus seeded).
- `/tmp/lore eval --cases eval/fixtures --config <cfg>` runs the recorded fixtures through the **existing replay** harness without error (proves live→replay composition).

---

## Self-Review

- **Spec coverage** (design §4 matrix): all 12 scenarios authored — natural (1-3, Task 1), what-changed (4-5, Task 2), saturation/network/dns (6,7,11, Task 3), cert/storage (10,12, Task 4), cloud read-only (8) + recall (9, Task 5); baseline run + gap list (Task 6). Coverage-source mandatory/optional split follows §5 (e.g. metrics optional-then-tighten on scenario 6; gitops optional on the non-Flux victim in scenario 4). Cloud stays describe-only (D7). Curation stays off (D6) — no `--curate` used. N=3 + stronger blind judge (D9/D10) in Task 6 Step 2.
- **Placeholder scan:** scenario 8's `root_cause` is a deliberate pin-at-runtime value (documented), not a forgotten placeholder; scenario 9 SKIPs by design until workstream B seeds the KB. Every manifest is complete and reversible.
- **Safety:** every invasive scenario's `teardown` deletes exactly what its `setup` created; the namespace is removed by the last scenario / verified in Task 6 Step 5. Selectors in natural prechecks are flagged for live confirmation (Task 1 Step 3).

---

## What this plan delivers

A committed 12-scenario catalog exercising every data source + cause class against mycluster-0, a `rubric.md`, the first baseline campaign report (the current-state quality assessment), a recorded replay corpus under `eval/fixtures/`, and a top-gaps list that becomes the input to **workstream B** — the learning-workflow redesign. Scenario 9 (instant-recall) is the explicit seam where the testing campaign hands off to the learning loop.
