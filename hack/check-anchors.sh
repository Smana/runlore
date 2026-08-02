#!/usr/bin/env bash
# Fail if any in-page anchor link points at a heading that does not exist.
#
# Hugo's refLinksErrorLevel: ERROR catches broken *page* references (relref), but says
# nothing about fragment links inside a page — `[Step 2](#step-2-...)` renders happily
# and lands the reader nowhere.
#
# This reads the real rendered HTML rather than recomputing heading IDs, deliberately.
# Hugo generates IDs with its own "github" style, which is not goldmark's default; a
# reimplementation here would drift from the renderer and then quietly disagree with it.
# The rendered output is the only thing true by construction.
#
# Written after five links in getting-started.md were found broken: a heading with an
# em dash ("## Step 2 — GitHub App") produces a DOUBLE hyphen (step-2--github-app), and
# every hand-written link had guessed a single one.
#
# Attributes are matched BOTH quoted and unquoted. `hugo --minify` drops the quotes, and
# the first version of this script only matched quoted ones: against a minified build it
# found zero anchors on every page and printed "OK ... across 56 pages". A guard that
# reports success while checking nothing is worse than no guard, which is why the zero
# check at the bottom is not optional politeness — it is the load-bearing part.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PUBLIC="${1:-$ROOT/website/public}"

if [ ! -d "$PUBLIC" ]; then
  echo "error: $PUBLIC does not exist — build the site first (cd website && hugo)" >&2
  exit 1
fi

fail=0
pages=0
checked=0

while IFS= read -r page; do
  pages=$((pages + 1))

  # Every id= in the page. Hugo emits heading anchors as ids; the theme emits ids for nav
  # elements too, which only makes this check more permissive, never stricter.
  # `|| true`: a page with no ids is normal, and grep exits 1 on no-match, which set -e
  # would otherwise turn into a silent early exit.
  ids="$(grep -oE 'id="[^"]*"|id=[^ >]+' "$page" | sed 's/^id=//; s/^"//; s/"$//' | sort -u || true)"

  while IFS= read -r frag; do
    [ -z "$frag" ] && continue
    checked=$((checked + 1))
    if ! grep -qxF "$frag" <<<"$ids"; then
      rel="${page#"$PUBLIC"/}"
      echo "::error file=website/content/${rel%/index.html}.md::broken in-page anchor #${frag} — no heading with that id"
      fail=1
    fi
  done < <(grep -oE 'href="#[^"]*"|href=#[^ >]+' "$page" | sed 's/^href=//; s/^"//; s/"$//; s/^#//' | sort -u || true)
done < <(find "$PUBLIC" -name index.html)

if [ "$fail" -ne 0 ]; then
  echo "FAIL: broken in-page anchors above" >&2
  exit 1
fi

# Guard the guard. If a Hugo or theme change alters how anchors are emitted, the greps
# above stop matching and every page passes trivially. Zero anchors across a docs site
# that demonstrably has hundreds means this script is inert, not that the docs are clean.
if [ "$checked" -lt 50 ]; then
  echo "::error::found only $checked in-page anchors across $pages pages — this script is" \
       "no longer matching the rendered HTML and is checking nothing" >&2
  exit 1
fi

echo "OK: $checked in-page anchors across $pages pages all resolve to a heading"
