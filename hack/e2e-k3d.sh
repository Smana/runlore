#!/usr/bin/env bash
# Back-compat entrypoint: the e2e suite is provider-agnostic and lives in
# hack/e2e-local.sh (E2E_PROVIDER=k3d|kind). This wrapper pins the historical
# k3d behavior so CI (.github/workflows/e2e-k3d.yml) and existing muscle
# memory keep working unchanged.
set -euo pipefail
exec env E2E_PROVIDER=k3d "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/e2e-local.sh" "$@"
