#!/usr/bin/env bash
# Run golangci-lint under the Go toolchain this module pins, not whatever `go` the
# developer happens to have on PATH.
#
# golangci-lint bundles staticcheck (honnef.co/go/tools), which builds its own IR from
# the AST. That IR builder only understands the Go versions it was released against, so
# a NEWER local toolchain makes it panic rather than report:
#
#   buildir: panic during analysis: unexpected expr: *ast.KeyValueExpr
#   Running error: can't run linter goanalysis_metalinter
#
# The panic aborts the whole run — not just staticcheck — so the exit status says
# "failed" for a reason that has nothing to do with the code. Observed with Go 1.27.0
# local against go.mod's toolchain go1.26.6.
#
# That failure mode is worse than it looks: the tempting workaround is
# `--disable=staticcheck`, which does produce a clean run, and silently drops one of
# the five linters in the `standard` set for everyone who adopts it. Pinning the
# toolchain instead keeps the full set — the same one CI runs, since the CI job
# resolves its Go from this very line via setup-go's go-version-file.
#
# GOTOOLCHAIN=auto (the default) does NOT do this on its own: it upgrades when the
# local toolchain is too OLD, and otherwise leaves a newer one in place.
#
# Any arguments are passed through, so `hack/lint.sh ./internal/notify/...` works.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Prefer the explicit `toolchain` line; fall back to the `go` directive, which is what
# a module without a toolchain line pins to.
toolchain="$(awk '/^toolchain /{print $2; exit}' go.mod)"
if [[ -z "$toolchain" ]]; then
  toolchain="go$(awk '/^go /{print $2; exit}' go.mod)"
fi

if ! command -v golangci-lint >/dev/null 2>&1; then
  echo "hack/lint.sh: golangci-lint not on PATH — see .golangci.yml for the version CI uses" >&2
  exit 127
fi

echo "hack/lint.sh: GOTOOLCHAIN=$toolchain golangci-lint run ${*:-./...}"
GOTOOLCHAIN="$toolchain" exec golangci-lint run "${@:-./...}"
