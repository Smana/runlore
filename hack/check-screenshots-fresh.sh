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

# ACKNOWLEDGED_RENDERER records a renderer commit whose visual change we KNOW the
# shipped screenshots do not yet reflect, because retaking them is blocked on
# something outside this repo: the card can only be rendered by a live Slack
# workspace, and the credentials for one live in the demo cluster.
#
# It is a deliberate, auditable escape — not a mute:
#   - it names ONE commit, so it states exactly which drift is accepted;
#   - the guard still reports the staleness loudly on every run;
#   - and it RE-ARMS: the moment the renderer moves past this commit the guard
#     fails again, so an acknowledgement cannot silently become permanent.
#
# Clear it (set to "") in the same commit that lands retaken screenshots.
ACKNOWLEDGED_RENDERER="a97b1df68c58cc4663d86f42ee160ca5f9f5a638"
ACKNOWLEDGED_REASON="The VISUAL drift is still #432's: it removed the fabricated 'What changed: No Git change identified' line and both shipped screenshots still show it. Retaking needs a live Slack workspace, unavailable until the demo cluster is rebuilt. The named commit is newer only because a security fix (webhook URL sanitised out of an error log) touched slack.go without changing a pixel — RENDERER cannot tell visual edits from non-visual ones, so it re-armed on a no-op."

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
  rend_sha=$(git log -1 --format='%H' -- "${RENDERER[@]}")
  if [ -n "$ACKNOWLEDGED_RENDERER" ] && [ "$rend_sha" = "$ACKNOWLEDGED_RENDERER" ]; then
    echo "::warning file=README.md::the incident-card screenshots are known to be stale (acknowledged)"
    echo "  screenshots last updated: $shot_when" >&2
    echo "  renderer last changed   : $rend_when" >&2
    echo "  acknowledged because    : $ACKNOWLEDGED_REASON" >&2
    echo >&2
    echo "Accepted for exactly this renderer commit. Any further renderer change fails" >&2
    echo "again. Clear ACKNOWLEDGED_RENDERER when the screenshots are retaken." >&2
    exit 0
  fi
  echo "::error file=README.md::the incident-card screenshots are older than the code that draws them"
  echo "  screenshots last updated: $shot_when" >&2
  echo "  renderer last changed   : $rend_when" >&2
  if [ -n "$ACKNOWLEDGED_RENDERER" ]; then
    echo "  (an acknowledgement exists for $ACKNOWLEDGED_RENDERER, but the renderer has moved past it)" >&2
  fi
  echo >&2
  echo "README.md embeds these images as what RunLore actually produces. Retake both" >&2
  echo "against a real investigation, commit them, and this passes again." >&2
  exit 1
fi

echo "OK: screenshots ($shot_when) are at least as recent as the renderer"
