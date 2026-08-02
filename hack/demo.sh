#!/usr/bin/env bash
# Demo: a REAL RunLore investigation, on recorded evidence, with no cluster, no API
# key and no network. The model turns are replayed from a transcript recorded once
# against a live model (examples/demo/*.transcript.json); the tools, the
# investigation loop and the rendered verdict card are the production code paths.
#
# Requires: Go. Nothing else.
# Usage: hack/demo.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$(mktemp -d)/lore"

go build -o "$BIN" "$ROOT/cmd/lore"
cd "$ROOT"          # the transcript + scenario paths are repo-relative
"$BIN" demo investigate --offline default
