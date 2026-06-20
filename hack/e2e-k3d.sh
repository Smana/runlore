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

cleanup() {
  [[ -n "$MOCK_PID" ]] && kill "$MOCK_PID" 2>/dev/null || true
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
  --k3s-arg "--disable=traefik@server:0"
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

step "3/8 build + import image"
docker build -t "$IMG" --build-arg VERSION=e2e .
k3d image import "$IMG" -c "$CLUSTER"

step "4/8 start mock backends on host :$MOCK_PORT"
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
helm upgrade --install runlore deploy/helm/runlore -n "$NS" \
  --set image.repository=runlore --set image.tag=e2e --set image.pullPolicy=Never \
  --set catalog.configMap=runlore-catalog \
  --set-string config.catalog.dir=/var/lib/runlore/catalog \
  --set-string config.model.base_url="http://$HOST:$MOCK_PORT/v1" \
  --set-string config.model.model=mock-model \
  --set-string config.model.api_key_env=OPENAI_API_KEY \
  --set-string config.notify.slack.webhook_url_env=SLACK_WEBHOOK_URL \
  --set-string config.notify.matrix.homeserver="http://$HOST:$MOCK_PORT" \
  --set-string config.notify.matrix.room_id='!test:mock' \
  --set-string config.notify.matrix.access_token_env=MATRIX_TOKEN \
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
check "mock model drove submit_findings"      /tmp/runlore-mock.log '> submit_findings'
check "Slack delivery"                         /tmp/runlore-mock.log 'MOCK SLACK'
check "Matrix delivery"                        /tmp/runlore-mock.log 'MOCK MATRIX'
check "curator enabled"                        /tmp/runlore.log 'curator enabled'
check "GitHub App token exchange"              /tmp/runlore-mock.log 'MOCK GH-TOKEN'
check "curator opened a PR (confident)"        /tmp/runlore-mock.log 'MOCK GH-PR'
check "curated ref logged"                     /tmp/runlore.log 'msg=curated'

step "8/8 GitOps failure trigger (informer on a real API server)"
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

step "RESULTS"
echo "PASS=$PASS FAIL=$FAIL"
echo "--- runlore.log (tail) ---"; tail -n 15 /tmp/runlore.log
echo "--- mock.log (tail) ---";   tail -n 15 /tmp/runlore-mock.log
[[ "$FAIL" == "0" ]] && green "ALL FEATURES VERIFIED" || { red "$FAIL CHECK(S) FAILED"; exit 1; }
