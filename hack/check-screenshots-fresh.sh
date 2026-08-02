#!/usr/bin/env bash
# Fail if the shipped Slack screenshots are older than the code that renders them.
#
# README.md embeds two captures of the incident card. Nothing tied them to the
# renderer, so a card refactor silently turned them into pictures of a layout that
# no longer exists — the README kept advertising it, confidently and wrongly.
#
# That is not hypothetical. On 2026-08-02 the card was refactored twice in one day
# (#399 moved the metadata below the answer, #409 restored the investigation's own
# conclusion to the verdict line) while the images sat unchanged from 2026-07-14.
#
# Timestamps come from git, not the filesystem: a fresh clone stamps every file
# with the checkout time, so mtimes would compare equal and this would pass in CI
# while failing to check anything.
#
# Requires full history — run it where actions/checkout uses fetch-depth: 0.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SHOTS=(assets/slack-notification.png assets/recall-notification.png)
# The files whose output the screenshots depict. Deliberately narrow: a change to
# the webhook client or the notifier registry does not alter what the card LOOKS
# like, and a guard that cries wolf gets muted.
RENDERER=(internal/notify/slack.go internal/notify/format.go)

last_commit_time() {
  local t
  t=$(git log -1 --format=%ct -- "$@" 2>/dev/null || true)
  [ -n "$t" ] || return 1
  printf '%s' "$t"
}

for f in "${SHOTS[@]}" "${RENDERER[@]}"; do
  [ -f "$f" ] || { echo "::error::$f is missing — update hack/check-screenshots-fresh.sh" >&2; exit 1; }
done

shot_t=$(last_commit_time "${SHOTS[@]}") || {
  echo "::error::no git history for the screenshots — this needs fetch-depth: 0" >&2; exit 1; }
rend_t=$(last_commit_time "${RENDERER[@]}") || {
  echo "::error::no git history for the renderer — this needs fetch-depth: 0" >&2; exit 1; }

shot_when=$(git log -1 --format='%ad (%h)' --date=short -- "${SHOTS[@]}")
rend_when=$(git log -1 --format='%ad (%h) %s' --date=short -- "${RENDERER[@]}")

if [ "$rend_t" -gt "$shot_t" ]; then
  echo "::error file=README.md::the incident-card screenshots are older than the code that draws them"
  echo "  screenshots last updated: $shot_when" >&2
  echo "  renderer last changed   : $rend_when" >&2
  echo >&2
  echo "README.md embeds these images as what RunLore actually produces. Retake both" >&2
  echo "against a real investigation, commit them, and this passes again." >&2
  exit 1
fi

echo "OK: screenshots ($shot_when) are at least as recent as the renderer"
