#!/usr/bin/env bash
# Fail if the shipped Slack screenshots depict a card RunLore no longer draws.
#
# README.md and the website home page embed two captures of the incident card.
# Nothing tied them to the renderer, so a card refactor silently turned them into
# advertisements for a layout that no longer exists — the README kept showing it,
# confidently and wrongly. That is not hypothetical: on 2026-08-02 the card was
# refactored twice in one day (#399 moved the metadata below the answer, #409
# restored the investigation's own conclusion to the verdict line) while the
# images sat unchanged from 2026-07-14.
#
# WHAT THIS COMPARES — and why it is no longer commit history.
#
# The first version of this guard asked "has internal/notify/slack.go changed since
# the screenshots were committed?". That is a proxy for the real question, and it
# was wrong in both directions:
#
#   - It cried wolf. slack.go carries the transport as well as the card, so adding
#     thread capture (#482) moved no pixel and failed this guard on four stacked
#     PRs with nothing the author could honestly do about it. "A guard that cries
#     wolf gets muted" was already written in this file; it took four days to come
#     true.
#   - Its escape hatch was dead on arrival. The acknowledgement named a COMMIT, and
#     this repo squash-merges: #475 acknowledged its own branch commit, the squash
#     collapsed that into a different SHA, and main went red the moment it merged.
#     Only a PR that did not itself touch the renderer could ever write a valid one.
#
# So it compares the CARD ITSELF. internal/notify/testdata/incident-card.golden.json
# is the Block Kit payload slackMessageWith renders for a fixture set chosen to hold
# every arm of the card open (see internal/notify/card_golden_test.go). Two tests
# make that file trustworthy:
#
#   TestSlackCardMatchesTheShippedGolden      the golden tracks the renderer, so it
#                                             cannot be hand-edited to any digest
#   TestCardGoldenCoversEveryRenderedBranch   the fixtures reach every arm, so a
#                                             changed branch cannot leave the
#                                             digest standing still
#
# Both run in `ci`, on every PR. This script is the other half: it asks whether the
# card has moved away from what the pictures show.
#
# Being content rather than history, the digest survives a squash-merge untouched,
# needs no git history at all, and stays silent for every change a reader of the
# README would never see.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

SHOTS=(assets/slack-notification.png assets/recall-notification.png)
CARD_GOLDEN=internal/notify/testdata/incident-card.golden.json

# DIGEST_AT_SHOT is the card AS THE SHIPPED SCREENSHOTS DEPICT IT: the golden as
# rendered at f8f01ea, the commit running in the cluster the captures were taken
# from. Both images were retaken 2026-08-20 from real investigations on a live
# workspace, so this is the renderer's current output rather than a reconstruction —
# no worktree replay needed this time.
#
# Update it in the same commit that lands retaken screenshots — and clear the
# acknowledgement below at the same time.
DIGEST_AT_SHOT="7e20faaa535f1f01597b8e10e078f731acc65c387b21bafe2ae0b97c959ee434"

# ACKNOWLEDGED_DIGEST records ONE card the shipped screenshots are known not to
# show, because retaking them is blocked on something outside this repo: the card
# can only be rendered by a live Slack workspace, and the credentials for one live
# in the demo cluster.
#
# It is a deliberate, auditable escape — not a mute:
#   - it names ONE digest, so it states exactly which drift is accepted;
#   - the guard still reports the staleness loudly on every run;
#   - and it RE-ARMS: the moment the card changes again the digest stops matching
#     and this fails, so an acknowledgement cannot silently become permanent.
#
# Clear it (set to "") in the same commit that lands retaken screenshots.
ACKNOWLEDGED_DIGEST="c96c7645999c821d18707513b9a67c3e50f41f8c8a65bb6aed112b880db0a5e0"
ACKNOWLEDGED_REASON="feat(slack): the investigation-silence feature added a 🔕 menu (silenceActionID) beside the existing 👍/👎 feedback buttons — internal/notify/card_golden_test.go's fixtures now render it, moving the golden's digest. A follow-up fix then stopped rendering the 🔕 overflow for fixtures with no TriggerKey (fingerprint-only attribution), moving the digest again, and a second one shortened the menu labels from Go duration strings ('🔕 Silence 1h0m0s') to the spelling the docs and values.yaml use ('🔕 Silence 1h'), moving it once more. The 2026-08-26 UX fix then replaced the overflow element itself with a labelled static_select ('🔕 Silence…' placeholder), because an overflow takes no text and drew as a bare '···' that nobody recognised — this is the one change of the four a reader of the screenshots would actually notice, and it moved the digest again. The shipped screenshots (assets/slack-notification.png, assets/recall-notification.png) predate this control and cannot be retaken here: the card can only be rendered by a live Slack workspace, and the credentials for one live in the demo cluster outside this repo. Clear this and update DIGEST_AT_SHOT in the same commit that lands retaken screenshots."

for f in "${SHOTS[@]}" "$CARD_GOLDEN"; do
  [ -f "$f" ] || { echo "::error::$f is missing — update hack/check-screenshots-fresh.sh" >&2; exit 1; }
done

# A copy-paste that set both to the same value would accept every future card
# forever, while still looking like a one-off acknowledgement.
if [ -n "$ACKNOWLEDGED_DIGEST" ] && [ "$ACKNOWLEDGED_DIGEST" = "$DIGEST_AT_SHOT" ]; then
  echo "::error::ACKNOWLEDGED_DIGEST equals DIGEST_AT_SHOT — that is not an acknowledgement, it is a permanent mute. Clear it." >&2
  exit 1
fi

current=$(sha256sum "$CARD_GOLDEN" | cut -d' ' -f1)

if [ "$current" = "$DIGEST_AT_SHOT" ]; then
  echo "OK: the card still renders as the shipped screenshots show it ($current)"
  exit 0
fi

if [ -n "$ACKNOWLEDGED_DIGEST" ] && [ "$current" = "$ACKNOWLEDGED_DIGEST" ]; then
  echo "::warning file=README.md::the incident-card screenshots are known to be stale (acknowledged)"
  echo "  screenshots show card: $DIGEST_AT_SHOT" >&2
  echo "  renderer now draws   : $current" >&2
  echo "  acknowledged because : $ACKNOWLEDGED_REASON" >&2
  echo >&2
  echo "Accepted for exactly this card. Any further change to it fails again." >&2
  echo "Clear ACKNOWLEDGED_DIGEST when the screenshots are retaken." >&2
  exit 0
fi

echo "::error file=README.md::the incident card has changed since the shipped screenshots were taken"
echo "  screenshots show card: $DIGEST_AT_SHOT" >&2
echo "  renderer now draws   : $current" >&2
if [ -n "$ACKNOWLEDGED_DIGEST" ]; then
  echo "  (an acknowledgement exists for $ACKNOWLEDGED_DIGEST, but the card has moved past it)" >&2
fi
echo >&2
echo "README.md and the website embed these images as what RunLore actually produces." >&2
echo "Diff $CARD_GOLDEN against its previous revision to see what a reader would now" >&2
echo "notice. Then either:" >&2
echo "  - retake both captures against a real investigation and set DIGEST_AT_SHOT to" >&2
echo "    $current, clearing ACKNOWLEDGED_DIGEST; or" >&2
echo "  - set ACKNOWLEDGED_DIGEST to $current with a reason, if retaking is blocked." >&2
exit 1
