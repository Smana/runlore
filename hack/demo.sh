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

# The one ask, and it lives here rather than in the README on purpose: someone who
# has just watched a correct verdict land — at zero cost, zero risk, no key and no
# cluster — is as willing as they are ever going to be. A single question, printed
# once, worth more than a star.
#
# It is a printed line and nothing else. No ping, no counter, no opt-out to explain:
# this demo runs offline by design, and quietly walking that back to measure
# interest would cost more trust than the number could possibly be worth.
cat <<'ASK'

────────────────────────────────────────────────────────────────────────────
 Did that verdict match a failure you have actually had?

 Where would it have been wrong on your platform? That is the most useful
 thing you can tell me — including if you decide RunLore is not for you.

 → https://github.com/Smana/runlore/discussions
────────────────────────────────────────────────────────────────────────────
ASK
