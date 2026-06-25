#!/usr/bin/env bash
# End-to-end test of RunLore on a throwaway k3d cluster.
#
# Exercises each feature against a REAL Kubernetes API server (informer, RBAC,
# deployment, config, webhook Service) with mock external backends (OpenAI model,
# Slack, Matrix) so the full chain executes: trigger -> policy -> workqueue ->
# ReAct loop (what_changed + kb_search) -> findings -> Slack/Matrix.
#
# Usage: hack/e2e-k3d.sh [--keep]    (--keep leaves the cluster up for inspection)
set -euo pipefail

CLUSTER=runlore-e2e
NS=runlore
IMG=runlore:e2e
MOCK_PORT=9999
KEEP=0
[[ "${1:-}" == "--keep" ]] && KEEP=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
MOCK_PID=""

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

# free_port kills whatever listens on a TCP port (by PID — `go run`'s compiled
# child survives a parent kill, so we target the listener directly).
free_port() {
  local pid
  pid=$(ss -ltnp 2>/dev/null | grep ":$1 " | grep -oP 'pid=\K[0-9]+' | head -1) || true
  [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
}

cleanup() {
  [[ -n "$MOCK_PID" ]] && kill "$MOCK_PID" 2>/dev/null || true
  free_port "$MOCK_PORT"; free_port 9998
  if [[ "$KEEP" == "0" ]]; then
    k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
  else
    echo "kept cluster '$CLUSTER' and mock (pid $MOCK_PID); delete with: k3d cluster delete $CLUSTER"
  fi
}
trap cleanup EXIT

PASS=0; FAIL=0
check() { # check <desc> <logfile> <pattern>
  if grep -qE "$3" "$2"; then green "PASS: $1"; PASS=$((PASS+1)); else red "FAIL: $1 (pattern: $3)"; FAIL=$((FAIL+1)); fi
}

step "1/8 create k3d cluster"
k3d cluster delete "$CLUSTER" >/dev/null 2>&1 || true
k3d cluster create "$CLUSTER" --wait --timeout 120s --no-lb \
  --k3s-arg "--disable=traefik@server:0" \
  --k3s-arg "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:0"   # tolerate a near-full dev host (absolute free space is ample)
kubectl config use-context "k3d-$CLUSTER" >/dev/null

step "2/8 install minimal Flux CRDs (Kustomization + GitRepository)"
kubectl apply -f - <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: { name: kustomizations.kustomize.toolkit.fluxcd.io }
spec:
  group: kustomize.toolkit.fluxcd.io
  scope: Namespaced
  names: { kind: Kustomization, plural: kustomizations, singular: kustomization, listKind: KustomizationList }
  versions:
    - name: v1
      served: true
      storage: true
      subresources: { status: {} }
      schema:
        openAPIV3Schema: { type: object, x-kubernetes-preserve-unknown-fields: true }
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: { name: gitrepositories.source.toolkit.fluxcd.io }
spec:
  group: source.toolkit.fluxcd.io
  scope: Namespaced
  names: { kind: GitRepository, plural: gitrepositories, singular: gitrepository, listKind: GitRepositoryList }
  versions:
    - name: v1
      served: true
      storage: true
      subresources: { status: {} }
      schema:
        openAPIV3Schema: { type: object, x-kubernetes-preserve-unknown-fields: true }
YAML
kubectl wait --for=condition=Established crd/kustomizations.kustomize.toolkit.fluxcd.io --timeout=30s
# A real Flux install owns flux-system; the chart namespaces its controller-logs
# Role + RoleBinding into it (rbac.controllerLogNamespaces), so it must pre-exist.
kubectl create namespace flux-system --dry-run=client -o yaml | kubectl apply -f -

step "3/8 build + import image"
docker build -t "$IMG" --build-arg VERSION=e2e .
k3d image import "$IMG" -c "$CLUSTER"

step "4/8 start mock backends on host :$MOCK_PORT"
free_port "$MOCK_PORT"; free_port 9998   # clear any stale mock from a prior run
go run -tags e2e ./hack/e2e/mock ":$MOCK_PORT" >/tmp/runlore-mock.log 2>&1 &
MOCK_PID=$!
sleep 2
curl -sf "http://127.0.0.1:$MOCK_PORT/healthz" >/dev/null 2>&1 || true  # 404 ok, just proves it's up
green "mock pid $MOCK_PID"

step "5/8 helm install runlore"
kubectl create ns "$NS" >/dev/null 2>&1 || true
kubectl -n "$NS" create configmap runlore-catalog --from-file=examples/runbooks/ \
  --dry-run=client -o yaml | kubectl apply -f -
HOST="host.k3d.internal"
# GitHub App key for the curator (mock GitHub API at the same host:port).
openssl genrsa -out /tmp/runlore-app-key.pem 2048 2>/dev/null
kubectl -n "$NS" create secret generic runlore-forge \
  --from-file=GITHUB_APP_PRIVATE_KEY=/tmp/runlore-app-key.pem \
  --dry-run=client -o yaml | kubectl apply -f -
# apps ns must exist before install: the namespace-scoped action Role is created here.
kubectl create namespace apps --dry-run=client -o yaml | kubectl apply -f -
helm upgrade --install runlore deploy/helm/runlore -n "$NS" \
  --set image.repository=runlore --set image.tag=e2e --set image.pullPolicy=Never \
  --set replicaCount=1 \
  --set catalog.configMap=runlore-catalog \
  --set-string config.catalog.dir=/var/lib/runlore/catalog \
  --set-string config.outcome.ledger_path=/tmp/outcomes.jsonl \
  --set config.telemetry.metrics_enabled=true \
  --set-string config.model.base_url="http://$HOST:$MOCK_PORT/v1" \
  --set-string config.model.model=mock-model \
  --set-string config.model.api_key_env=OPENAI_API_KEY \
  --set-string config.notify.slack.webhook_url_env=SLACK_WEBHOOK_URL \
  --set-string config.notify.matrix.homeserver="http://$HOST:$MOCK_PORT" \
  --set-string config.notify.matrix.room_id='!test:mock' \
  --set-string config.notify.matrix.access_token_env=MATRIX_TOKEN \
  --set-string config.metrics.url="http://$HOST:$MOCK_PORT" \
  --set-string config.logs.url="http://$HOST:$MOCK_PORT" \
  --set-string config.network.url="$HOST:9998" \
  --set-string config.actions.mode=approve \
  --set-string config.actions.approval_token_env=APPROVAL_TOKEN \
  --set-string config.notify.slack.signing_secret_env=SLACK_SIGNING_SECRET \
  --set rbac.allowActions=true \
  --set networkPolicy.enabled=false \
  --set "config.actions.allow.namespaces={apps}" \
  --set "rbac.actionNamespaces={apps}" \
  --set "config.notify.slack.approver_ids={U_E2E}" \
  --set "env[3].name=APPROVAL_TOKEN" --set-string "env[3].value=e2e-secret" \
  --set "env[4].name=SLACK_SIGNING_SECRET" --set-string "env[4].value=e2e-slack-secret" \
  --set "env[5].name=WEBHOOK_TOKEN" --set-string "env[5].value=e2e-webhook" \
  --set-string config.server.webhook_token_env=WEBHOOK_TOKEN \
  --set-string config.forge.github_api_url="http://$HOST:$MOCK_PORT" \
  --set-string config.forge.kb_repo="mock/repo" \
  --set-string config.forge.base_branch="main" \
  --set config.forge.github_app.app_id=123 \
  --set config.forge.github_app.installation_id=42 \
  --set-string config.forge.github_app.private_key_env=GITHUB_APP_PRIVATE_KEY \
  --set "env[0].name=OPENAI_API_KEY" --set-string "env[0].value=mock" \
  --set "env[1].name=SLACK_WEBHOOK_URL" --set-string "env[1].value=http://$HOST:$MOCK_PORT/slack" \
  --set "env[2].name=MATRIX_TOKEN" --set-string "env[2].value=mocktoken" \
  --set "envFrom[0].secretRef.name=runlore-forge"
kubectl -n "$NS" rollout status deploy/runlore --timeout=90s

step "6/8 startup wiring (config, catalog, RBAC, watch)"
sleep 3
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
check "catalog loaded from ConfigMap" /tmp/runlore.log 'catalog loaded.*entries=[1-9]'
check "LLM investigator active"        /tmp/runlore.log 'using LLM investigator'
check "watching gitops failures"       /tmp/runlore.log 'watching gitops failures'
check "outcome ledger enabled"         /tmp/runlore.log 'outcome ledger enabled'
check "serving"                        /tmp/runlore.log 'runlore serving'

step "7/8 incident webhook -> investigate -> findings -> deliver -> curate"
# Post from inside the cluster (no host port-forward — avoids host port conflicts).
kubectl -n "$NS" create configmap am-payload \
  --from-file=alertmanager-webhook.json=examples/alertmanager-webhook.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$NS" delete pod curl --ignore-not-found >/dev/null 2>&1 || true
kubectl -n "$NS" run curl --image=curlimages/curl:8.11.1 --restart=Never --rm -i --quiet \
  --overrides='{"spec":{"containers":[{"name":"curl","image":"curlimages/curl:8.11.1","command":["curl","-s","-o","/dev/null","-w","webhook HTTP %{http_code}\n","-XPOST","http://runlore.runlore.svc:8080/webhook/alertmanager","--data","@/p/alertmanager-webhook.json"],"volumeMounts":[{"name":"p","mountPath":"/p"}]}],"volumes":[{"name":"p","configMap":{"name":"am-payload"}}]}}'
sleep 6
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
check "incident accepted + investigate=true" /tmp/runlore.log 'msg=incident.*investigate=true'
check "investigation completed (findings)"   /tmp/runlore.log 'msg=findings'
check "mock model received chat/completions"  /tmp/runlore-mock.log 'chat/completions'
check "mock model drove what_changed"         /tmp/runlore-mock.log '> what_changed'
check "mock model drove kb_search"            /tmp/runlore-mock.log '> kb_search'
check "mock model drove query_metrics"        /tmp/runlore-mock.log '> query_metrics'
check "mock model drove query_logs"           /tmp/runlore-mock.log '> query_logs'
check "mock model drove network_drops"        /tmp/runlore-mock.log '> network_drops'
check "metrics backend queried"               /tmp/runlore-mock.log 'MOCK METRICS'
check "logs backend queried"                  /tmp/runlore-mock.log 'MOCK LOGS'
check "hubble (network) queried"              /tmp/runlore-mock.log 'MOCK HUBBLE'
check "mock model drove submit_findings"      /tmp/runlore-mock.log '> submit_findings'
check "Slack delivery"                         /tmp/runlore-mock.log 'MOCK SLACK'
check "Matrix delivery"                        /tmp/runlore-mock.log 'MOCK MATRIX'
check "curator enabled"                        /tmp/runlore.log 'curator enabled'
check "GitHub App token exchange"              /tmp/runlore-mock.log 'MOCK GH-TOKEN'
check "curator opened a PR (confident)"        /tmp/runlore-mock.log 'MOCK GH-PR'
check "curated ref logged"                     /tmp/runlore.log 'msg=curated'
check "rung-2 approval-gated actions enabled"  /tmp/runlore.log 'approval-gated actions enabled'
check "action registered for approval"         /tmp/runlore.log 'actions registered for approval'

step "7b/13 LEARNING LOOP: outcome ledger capture (open) + resolve closes the loop"
# Scrape the OTel /metrics exposition (image is distroless — no shell to read the
# ledger file directly; the ledger emits counters instead).
LLPORT=18086; free_port "$LLPORT"
kubectl -n "$NS" port-forward svc/runlore "$LLPORT:8080" >/tmp/runlore-ll-pf.log 2>&1 &
LLPF=$!; sleep 3
# prefix-match tolerates the _total / _total_total suffix variants of the OTel exporter.
llmetric() { curl -sf "http://localhost:$LLPORT/metrics" 2>/dev/null | awk -v p="^runlore_$1" '$0 ~ p {print int($NF); exit}'; }
OPENED=$(llmetric outcomes_opened); OPENED=${OPENED:-0}
if [[ "$OPENED" -ge 1 ]]; then green "PASS: outcome ledger recorded an investigation open (capture; opened=$OPENED)"; PASS=$((PASS+1))
else red "FAIL: no outcome 'open' recorded (capture; opened=$OPENED)"; FAIL=$((FAIL+1)); fi

# Close the loop: the resolved alert for the investigated incident (fp1). A resolve
# that MATCHES an open increments incidents_resolved_total — proving open→resolve pairing.
RESOLVE_JSON='{"alerts":[{"status":"resolved","labels":{"alertname":"HarborProbeFailure","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"fp1"}]}'
curl -s -o /dev/null -w "resolve webhook HTTP %{http_code}\n" -XPOST "http://localhost:$LLPORT/webhook/alertmanager" -H "Content-Type: application/json" -d "$RESOLVE_JSON" || true
sleep 4
RESOLVED=$(llmetric incidents_resolved); RESOLVED=${RESOLVED:-0}
kill "$LLPF" 2>/dev/null || true; free_port "$LLPORT"
if [[ "$RESOLVED" -ge 1 ]]; then green "PASS: open→resolve loop closed (a resolve matched an open; resolved=$RESOLVED)"; PASS=$((PASS+1))
else red "FAIL: resolve did not match an open (resolved=$RESOLVED)"; FAIL=$((FAIL+1)); fi

step "7c/13 LEARNING LOOP: lore curate wires the ledger-backed Queue + Recurrence passes"
# Exec the binary directly (distroless has no shell). With the ledger configured,
# runCurate enables Queue + Recurrence; the passes run against the mock forge.
kubectl -n "$NS" exec deploy/runlore -- /usr/local/bin/lore curate --config /etc/runlore/runlore.yaml > /tmp/runlore-curate.log 2>&1 || true
check "curate wired Queue + Recurrence (ledger-backed)" /tmp/runlore-curate.log 'Queue . Recurrence enabled'
check "curate grooming the backlog"                     /tmp/runlore-curate.log 'grooming KB backlog'

step "8/13 storm: 40 same-groupKey alerts coalesce into 1 investigation"
# Enable coalescing (MaxBatch=40 flushes synchronously) + OTel metrics exposition.
helm upgrade runlore deploy/helm/runlore -n "$NS" --reuse-values \
  --set "config.investigation.coalesce.enabled=true" \
  --set "config.investigation.coalesce.max_batch=40" \
  --set "config.telemetry.metrics_enabled=true" \
  --set replicaCount=1 >/dev/null
kubectl -n "$NS" rollout status deploy/runlore --timeout=90s >/dev/null
sleep 3

STORM_PORT=18085; free_port "$STORM_PORT"
kubectl -n "$NS" port-forward svc/runlore "$STORM_PORT:8080" >/tmp/runlore-storm-pf.log 2>&1 &
STORM_PF=$!; sleep 3

# 40 critical alerts, unique fingerprints, same groupKey. The first flushes
# immediately (critical fast-path); the other 39 fall inside the cooldown and are
# suppressed → one investigation, not 40.
STORM_PAYLOAD=$(python3 -c "
import json
alerts = [
    {'status': 'firing',
     'labels': {'alertname': 'HighLatency', 'severity': 'critical', 'namespace': 'prod'},
     'fingerprint': 'storm-fp-%d' % i,
     'startsAt': '2026-01-01T00:00:00Z'}
    for i in range(1, 41)
]
print(json.dumps({'groupKey': 'storm-group-key', 'alerts': alerts}))
")
curl -s -o /dev/null -w "storm webhook HTTP %{http_code}\n" \
  -XPOST "http://localhost:$STORM_PORT/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d "$STORM_PAYLOAD" || true
sleep 6   # allow MaxBatch flush + investigation to start

METRICS=$(curl -sf "http://localhost:$STORM_PORT/metrics" 2>/dev/null || true)
kill "$STORM_PF" 2>/dev/null || true; free_port "$STORM_PORT"

# Parse metric values. Metrics are runlore_-prefixed by the OTel Prometheus exporter;
# prefix-match also tolerates _total vs _total_total suffix variants. awk never fails
# the pipeline, so a missing metric → 0 (a clean FAIL, not a set -e crash).
metric() { echo "$METRICS" | awk -v p="^runlore_$1" '$0 ~ p {print int($NF); exit}'; }
RECEIVED=$(metric alerts_received);       RECEIVED=${RECEIVED:-0}
STARTED=$(metric investigations_started); STARTED=${STARTED:-0}
SUPPRESSED=$(metric alerts_suppressed);   SUPPRESSED=${SUPPRESSED:-0}
COALESCED=$(metric alerts_coalesced);     COALESCED=${COALESCED:-0}
ABSORBED=$((SUPPRESSED + COALESCED))

if [[ "$STARTED" -ge 1 && "$STARTED" -le 3 ]]; then
  green "PASS: storm bounded to one investigation (investigations_started_total=$STARTED, want 1-3)"
  PASS=$((PASS+1))
else
  red "FAIL: storm investigations out of bounds (investigations_started_total=$STARTED, want 1-3)"
  FAIL=$((FAIL+1))
fi
if [[ "$ABSORBED" -ge 1 ]]; then
  green "PASS: storm absorbed ($ABSORBED of $RECEIVED — suppressed=$SUPPRESSED, coalesced=$COALESCED)"
  PASS=$((PASS+1))
else
  red "FAIL: storm not absorbed (received=$RECEIVED, suppressed=$SUPPRESSED, coalesced=$COALESCED)"
  FAIL=$((FAIL+1))
fi

step "9/13 GitOps failure trigger (informer on a real API server)"
kubectl create ns apps >/dev/null 2>&1 || true
kubectl apply -f - <<'YAML'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: broken-app, namespace: apps }
spec: { path: ./apps, interval: 1m }
YAML
kubectl patch kustomization broken-app -n apps --subresource=status --type=merge -p \
  '{"status":{"conditions":[{"type":"Ready","status":"False","reason":"BuildFailed","message":"mock build failure","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}'
sleep 6
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
check "gitops failure -> investigate" /tmp/runlore.log 'source=gitops-failure|Kustomization/broken-app'

# Rung 2: approve a pending suspend action and verify it executes on the cluster.
PORT=18090; free_port "$PORT"
kubectl -n "$NS" port-forward svc/runlore "$PORT:8080" >/tmp/runlore-pf.log 2>&1 &
PF=$!; sleep 3
# (a) Token endpoint → execute (suspends broken-app).
ID=$(curl -s -H "X-Approval-Token: e2e-secret" "localhost:$PORT/actions" | grep -oP '"ID":"\K[^"]+' | head -1) || true
curl -s -o /dev/null -w "token approve HTTP %{http_code}\n" -X POST -H "X-Approval-Token: e2e-secret" "localhost:$PORT/actions/$ID/approve" || true
# (b) Signed Slack interaction → approve another pending action (HMAC over v0:ts:body).
ID2=$(curl -s -H "X-Approval-Token: e2e-secret" "localhost:$PORT/actions" | grep -oP '"ID":"\K[^"]+' | head -1) || true
if [[ -n "$ID2" ]]; then
  python3 - "$ID2" "$PORT" <<'PY'
import sys, hmac, hashlib, time, json, urllib.parse, urllib.request
aid, port = sys.argv[1], sys.argv[2]
payload = json.dumps({"user": {"id": "U_E2E", "username": "e2e"}, "actions": [{"action_id": "runlore_approve", "value": aid}]})
body = "payload=" + urllib.parse.quote(payload)
ts = str(int(time.time()))
sig = "v0=" + hmac.new(b"e2e-slack-secret", f"v0:{ts}:{body}".encode(), hashlib.sha256).hexdigest()
req = urllib.request.Request(f"http://localhost:{port}/slack/interactions", data=body.encode(),
    headers={"X-Slack-Request-Timestamp": ts, "X-Slack-Signature": sig, "Content-Type": "application/x-www-form-urlencoded"})
try:
    print("slack interaction HTTP", urllib.request.urlopen(req).status)
except Exception as e:
    print("slack interaction error:", e)
PY
fi
kill "$PF" 2>/dev/null || true; free_port "$PORT"
sleep 3
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
SUSPENDED=$(kubectl get kustomization broken-app -n apps -o jsonpath='{.spec.suspend}' 2>/dev/null || true)
if [[ "$SUSPENDED" == "true" ]]; then green "PASS: approved action executed (broken-app suspended)"; PASS=$((PASS+1))
else red "FAIL: broken-app not suspended (spec.suspend=$SUSPENDED)"; FAIL=$((FAIL+1)); fi
check "execution audit-logged"          /tmp/runlore.log 'action approved and executed'
check "slack button approval executed"  /tmp/runlore.log 'slack approval executed'

step "9/11 rung-3 auto-execution + kill-switch"
# Reconfigure to auto mode: reversible actions execute WITHOUT human approval, gated
# by confidence + rate-limit + the kill-switch.
helm upgrade runlore deploy/helm/runlore -n "$NS" --reuse-values \
  --set-string config.actions.mode=auto \
  --set-json config.actions.auto.min_confidence=0.5 \
  --set-json config.actions.auto.max_per_window=5 \
  --set-string config.actions.audit_log_path=/tmp/runlore-audit.jsonl \
  --set-string config.server.webhook_token_env=WEBHOOK_TOKEN \
  --set replicaCount=1 >/dev/null
kubectl -n "$NS" rollout status deploy/runlore --timeout=120s >/dev/null
PORT=18091; free_port "$PORT"
kubectl -n "$NS" port-forward svc/runlore "$PORT:8080" >/tmp/runlore-pf.log 2>&1 &
PF=$!; sleep 3
# Auto starts paused (fail-closed cold start); resume before exercising execution.
curl -s -o /dev/null -XPOST -H "X-Approval-Token: e2e-secret" "localhost:$PORT/actions/resume"
sleep 1
# (a) Auto executes: un-suspend broken-app, fire a fresh critical incident, expect auto
# to re-suspend it with no human in the loop.
kubectl patch kustomization broken-app -n apps --type=merge -p '{"spec":{"suspend":false}}' >/dev/null
curl -s -o /dev/null -XPOST -H "Authorization: Bearer e2e-webhook" "localhost:$PORT/webhook/alertmanager" \
  -d '{"alerts":[{"status":"firing","labels":{"alertname":"AutoTest1","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"auto-fp-1"}]}'
sleep 6
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
check "rung-3 auto enabled"            /tmp/runlore.log 'AUTO execution ENABLED'
check "auto-executed (no approval)"    /tmp/runlore.log 'auto-executed'
SUSP=$(kubectl get kustomization broken-app -n apps -o jsonpath='{.spec.suspend}' 2>/dev/null || true)
if [[ "$SUSP" == "true" ]]; then green "PASS: auto-execution suspended broken-app without human approval"; PASS=$((PASS+1))
else red "FAIL: auto did not suspend broken-app (spec.suspend=$SUSP)"; FAIL=$((FAIL+1)); fi
# (b) Kill-switch: pause, un-suspend, fire again, expect NO execution.
curl -s -o /dev/null -XPOST -H "X-Approval-Token: e2e-secret" "localhost:$PORT/actions/pause"
kubectl patch kustomization broken-app -n apps --type=merge -p '{"spec":{"suspend":false}}' >/dev/null
curl -s -o /dev/null -XPOST -H "Authorization: Bearer e2e-webhook" "localhost:$PORT/webhook/alertmanager" \
  -d '{"alerts":[{"status":"firing","labels":{"alertname":"AutoTest2","severity":"critical","namespace":"apps"},"startsAt":"2026-06-20T03:14:00Z","fingerprint":"auto-fp-2"}]}'
sleep 6
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
kill "$PF" 2>/dev/null || true; free_port "$PORT"
check "kill-switch paused auto"        /tmp/runlore.log 'auto paused'
SUSP2=$(kubectl get kustomization broken-app -n apps -o jsonpath='{.spec.suspend}' 2>/dev/null || true)
if [[ "$SUSP2" != "true" ]]; then green "PASS: kill-switch blocked auto-execution (broken-app left un-suspended)"; PASS=$((PASS+1))
else red "FAIL: auto executed while paused (spec.suspend=$SUSP2)"; FAIL=$((FAIL+1)); fi

step "10/11 leader election + failover (scale to 2)"
kubectl -n "$NS" scale deploy/runlore --replicas=2 >/dev/null
TOTAL=0; READY=0
for _ in $(seq 1 30); do
  TOTAL=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=runlore --no-headers 2>/dev/null | wc -l | tr -d ' ')
  READY=$(kubectl -n "$NS" get pods -l app.kubernetes.io/name=runlore \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | grep -c True || true)
  [[ "$TOTAL" == "2" && "$READY" == "1" ]] && break
  sleep 2
done
if [[ "$TOTAL" == "2" && "$READY" == "1" ]]; then
  green "PASS: 2 replicas, exactly 1 Ready (leader); the other is hot standby"; PASS=$((PASS+1))
else red "FAIL: replicas=$TOTAL ready=$READY (want 2 / 1)"; FAIL=$((FAIL+1)); fi

HOLDER=$(kubectl -n "$NS" get lease runlore-leader -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
if [[ -n "$HOLDER" ]]; then green "PASS: Lease held by $HOLDER"; PASS=$((PASS+1))
else red "FAIL: no Lease holder"; FAIL=$((FAIL+1)); fi

# Failover: delete the leader; a standby must acquire the Lease.
kubectl -n "$NS" delete pod "$HOLDER" --wait=false >/dev/null 2>&1 || true
NEW=""
for _ in $(seq 1 30); do
  NEW=$(kubectl -n "$NS" get lease runlore-leader -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
  [[ -n "$NEW" && "$NEW" != "$HOLDER" ]] && break
  sleep 2
done
if [[ -n "$NEW" && "$NEW" != "$HOLDER" ]]; then green "PASS: failover — new leader $NEW"; PASS=$((PASS+1))
else red "FAIL: no failover (holder still '$NEW')"; FAIL=$((FAIL+1)); fi

step "11/11 ArgoCD engine (reconfigure + Application Degraded)"
kubectl apply -f - <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: { name: applications.argoproj.io }
spec:
  group: argoproj.io
  scope: Namespaced
  names: { kind: Application, plural: applications, singular: application, listKind: ApplicationList }
  versions:
    - name: v1alpha1
      served: true
      storage: true
      subresources: { status: {} }
      schema:
        openAPIV3Schema: { type: object, x-kubernetes-preserve-unknown-fields: true }
YAML
kubectl wait --for=condition=Established crd/applications.argoproj.io --timeout=30s
# Switch the engine to argocd (reuse prior values; back to 1 replica for a quick roll).
# Reconfigure to the argocd engine. The chart's Recreate strategy makes this
# in-place update roll cleanly under leader election (old pods terminate first).
helm upgrade runlore deploy/helm/runlore -n "$NS" --reuse-values \
  --set replicaCount=1 --set-string config.gitops.engine=argocd >/dev/null
kubectl -n "$NS" rollout status deploy/runlore --timeout=120s
sleep 3
kubectl apply -f - <<'YAML'
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: { name: broken-argo, namespace: apps }
spec: { source: { repoURL: "https://github.com/org/repo", path: apps } }
YAML
kubectl patch application broken-argo -n apps --subresource=status --type=merge -p \
  '{"status":{"health":{"status":"Degraded"},"sync":{"revision":"deadbeef","status":"OutOfSync"},"operationState":{"message":"image pull backoff"}}}'
sleep 6
kubectl -n "$NS" logs deploy/runlore > /tmp/runlore.log 2>&1
check "argocd engine active"          /tmp/runlore.log 'engine=argocd'
check "argocd failure -> investigate" /tmp/runlore.log 'Application/broken-argo'

step "RESULTS"
echo "PASS=$PASS FAIL=$FAIL"
echo "--- runlore.log (tail) ---"; tail -n 15 /tmp/runlore.log
echo "--- mock.log (tail) ---";   tail -n 15 /tmp/runlore-mock.log
[[ "$FAIL" == "0" ]] && green "ALL FEATURES VERIFIED" || { red "$FAIL CHECK(S) FAILED"; exit 1; }
