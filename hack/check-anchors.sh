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

# Map a rendered page back to the content file that produced it, for the ::error
# annotation. `docs/foo/index.html` is usually `docs/foo.md`, but a SECTION index comes
# from `docs/foo/_index.md` — and an annotation naming a file that does not exist is one
# GitHub silently drops, so the reviewer sees the failure only in the raw log.
source_file() {
  local rel="${1#"$PUBLIC"/}"; rel="${rel%/index.html}"
  if [ -f "$ROOT/website/content/${rel}/_index.md" ]; then
    echo "website/content/${rel}/_index.md"
  else
    echo "website/content/${rel}.md"
  fi
}

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
      echo "::error file=$(source_file "$page")::broken in-page anchor #${frag} — no heading with that id"
      fail=1
    fi
  done < <(grep -oE 'href="#[^"]*"|href=#[^ >]+' "$page" | sed 's/^href=//; s/^"//; s/"$//; s/^#//' | sort -u || true)
done < <(find "$PUBLIC" -name index.html)

# ---------------------------------------------------------------------------
# Cross-page anchors: `[x]({{< relref "other.md#section" >}})`.
#
# The loop above only sees href="#frag" — same-page links. A relref to ANOTHER page
# renders as href="/docs/…/other/#frag", which it never matched, and Hugo's
# refLinksErrorLevel: ERROR validates only the page half of that reference: the
# fragment is unchecked by both. So the site's most fragile links — the ones whose
# target heading lives in a file you are not editing — were the only ones with no
# guard at all.
#
# Found the hard way. Three links pointed at
# security-model.md#the-feedback-channels--exposure--trust-model with two hyphens; the
# heading is "## The feedback channels (👍/👎) — exposure & trust model", where the
# dropped emoji-in-parens AND the dropped em dash each leave a separator behind, so the
# real id has THREE. Same trap as the em-dash case in the header comment above, one
# level nastier, and it had already shipped.
#
# Index is "<url-path>#<id>" lines so a link can be checked with one grep -qxF and no
# associative arrays (the URL path is exactly what href= contains, so no normalising).
index="$(mktemp)"
trap 'rm -f "$index"' EXIT
while IFS= read -r page; do
  url="/${page#"$PUBLIC"/}"; url="${url%index.html}"
  grep -oE 'id="[^"]*"|id=[^ >]+' "$page" \
    | sed "s/^id=//; s/^\"//; s/\"$//; s|^|${url}#|" >> "$index" || true
done < <(find "$PUBLIC" -name index.html)
sort -u -o "$index" "$index"

xchecked=0
while IFS= read -r page; do
  while IFS= read -r link; do
    [ -z "$link" ] && continue
    xchecked=$((xchecked + 1))
    if ! grep -qxF "$link" "$index"; then
      # Only links into pages this build produced. An href to a path with no rendered
      # page is a broken *page* ref, which refLinksErrorLevel already fails on; flagging
      # it here too would just double-report someone else's error.
      [ -f "$PUBLIC${link%%#*}index.html" ] || continue
      echo "::error file=$(source_file "$page")::broken cross-page anchor ${link} — that page has no heading with that id"
      fail=1
    fi
  done < <(grep -oE 'href="/[^"#]*#[^"]*"|href=/[^ >#]*#[^ >]+' "$page" | sed 's/^href=//; s/^"//; s/"$//' | sort -u || true)
done < <(find "$PUBLIC" -name index.html)

if [ "$fail" -ne 0 ]; then
  echo "FAIL: broken anchors above" >&2
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
# Same reasoning for the cross-page pass, at the scale that pass actually operates on.
if [ "$xchecked" -lt 20 ]; then
  echo "::error::found only $xchecked cross-page anchors across $pages pages — the" \
       "cross-page pass is no longer matching the rendered HTML and is checking nothing" >&2
  exit 1
fi

echo "OK: $checked in-page and $xchecked cross-page anchors across $pages pages all resolve to a heading"
