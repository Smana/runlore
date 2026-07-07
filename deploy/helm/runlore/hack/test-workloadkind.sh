#!/usr/bin/env bash
# Fast, no-cluster render assertions for the workloadKind toggle. Run from anywhere;
# resolves the chart relative to this script so it works from a checkout or a worktree.
#
#   deploy/helm/runlore/hack/test-workloadkind.sh
#
# Exit 0 and prints "PASS: N/N" on success; exits 1 with the first failing assertion.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHART="$SCRIPT_DIR/.."
PASS=0
FAIL=0

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then
    echo "PASS: $desc"; PASS=$((PASS+1))
  else
    echo "FAIL: $desc — expected to find: $needle"; FAIL=$((FAIL+1))
  fi
}

assert_not_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then
    echo "FAIL: $desc — did not expect to find: $needle"; FAIL=$((FAIL+1))
  else
    echo "PASS: $desc"; PASS=$((PASS+1))
  fi
}

echo "--- default render (workloadKind unset) ---"
DEFAULT_RENDER=$(helm template runlore "$CHART" --set persistence.enabled=true)
assert_contains  "default: renders a Deployment"        "$DEFAULT_RENDER" "kind: Deployment"
assert_not_contains "default: no StatefulSet"            "$DEFAULT_RENDER" "kind: StatefulSet"
assert_contains  "default: standalone PVC still renders" "$DEFAULT_RENDER" "kind: PersistentVolumeClaim"
assert_contains  "default: Deployment mounts the shared PVC" "$DEFAULT_RENDER" "claimName:"

echo "--- StatefulSet render (workloadKind=StatefulSet, persistence + 2 replicas) ---"
SS_RENDER=$(helm template runlore "$CHART" \
  --set workloadKind=StatefulSet \
  --set persistence.enabled=true \
  --set replicaCount=2)
assert_contains     "statefulset: renders a StatefulSet"        "$SS_RENDER" "kind: StatefulSet"
assert_not_contains "statefulset: no plain Deployment"          "$SS_RENDER" "kind: Deployment"
assert_contains     "statefulset: has volumeClaimTemplates"     "$SS_RENDER" "volumeClaimTemplates:"
assert_contains     "statefulset: sets serviceName"             "$SS_RENDER" "serviceName:"
assert_contains     "statefulset: headless service present"    "$SS_RENDER" "clusterIP: None"
assert_not_contains "statefulset: no standalone PVC (owns its own via volumeClaimTemplates)" "$SS_RENDER" "kind: PersistentVolumeClaim"
assert_not_contains "statefulset: no persistentVolumeClaim volume (superseded by volumeClaimTemplates)" "$SS_RENDER" "persistentVolumeClaim:"

echo
echo "PASS: $PASS/$((PASS+FAIL))"
[[ $FAIL -eq 0 ]]
