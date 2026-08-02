#!/usr/bin/env python3
"""Convert <text> elements in an SVG to outlined <path> geometry.

WHY THIS EXISTS

assets/logo-wordmark.svg asked for `font-family: 'Space Grotesk'`, but the font ships
with the family name "Space Grotesk SemiBold". The request therefore never matched, and
every renderer silently fell back to whatever it had:

    $ fc-match "Space Grotesk:weight=demibold"
    NotoSans-Bold.ttf: "Noto Sans" "Bold"

The wordmark is the site logo (website/hugo.yaml) and appears in the README, so it was
rendering in Noto Sans for essentially everyone — a browser has no more access to a
locally-installed font than fontconfig does. Fixing the family string would only move the
problem: any SVG shipped as an <img> must not depend on a font at all.

Outlining removes the dependency entirely. Shaping goes through HarfBuzz, the same shaper
librsvg and browsers use, so kerning and advances match what the design intended.

USAGE

    python3 -m venv .venv && .venv/bin/pip install uharfbuzz fonttools
    .venv/bin/python hack/outline-svg-text.py assets/logo-wordmark.svg ...

Edits in place. A file with no <text> is reported and left alone, so re-running is safe.

FONTS

Weight 600 maps to ~/.fonts/SpaceGrotesk-600.ttf; override the directory with
SPACE_GROTESK_DIR. Any other weight is a hard error rather than a silent substitution —
substituting a font here is the exact bug this script exists to fix.
"""

import os
import pathlib
import re
import sys

import uharfbuzz as hb
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.ttLib import TTFont

FONT_DIR = pathlib.Path(os.environ.get("SPACE_GROTESK_DIR", pathlib.Path.home() / ".fonts"))
WEIGHT_FILES = {"600": "SpaceGrotesk-600.ttf"}

_cache = {}


def load(weight):
    if weight not in WEIGHT_FILES:
        sys.exit(
            f"outline-svg-text: no font for font-weight={weight}. "
            f"Available: {sorted(WEIGHT_FILES)}. Add the .ttf and extend WEIGHT_FILES — "
            "do NOT substitute another weight, that is the bug this script fixes."
        )
    if weight not in _cache:
        path = FONT_DIR / WEIGHT_FILES[weight]
        if not path.is_file():
            sys.exit(f"outline-svg-text: font not found: {path} (set SPACE_GROTESK_DIR)")
        blob = hb.Blob.from_file_path(str(path))
        tt = TTFont(str(path))
        _cache[weight] = (hb.Font(hb.Face(blob)), tt, tt.getGlyphSet(), tt.getGlyphOrder(),
                          tt["head"].unitsPerEm)
    return _cache[weight]


def attr(tag, name, default=None):
    m = re.search(rf'\b{re.escape(name)}="([^"]*)"', tag)
    return m.group(1) if m else default


def measure(runs, weight, size, lsp):
    """Total advance of the shaped runs, in user units."""
    hbfont, _, _, _, upem = load(weight)
    scale = size / upem
    total = 0.0
    for text, _ in runs:
        buf = hb.Buffer()
        buf.add_str(text)
        buf.guess_segment_properties()
        hb.shape(hbfont, buf, {"kern": True, "liga": True})
        for pos in buf.glyph_positions:
            total += pos.x_advance * scale + lsp
    return total - lsp if total else 0.0   # trailing letter-spacing is advance, not ink


def outline(runs, weight, size, lsp, x, baseline, indent):
    hbfont, _, gs, order, upem = load(weight)
    scale = size / upem
    groups = []
    for text, fill in runs:
        buf = hb.Buffer()
        buf.add_str(text)
        buf.guess_segment_properties()
        hb.shape(hbfont, buf, {"kern": True, "liga": True})
        paths = []
        for info, pos in zip(buf.glyph_infos, buf.glyph_positions):
            pen = SVGPathPen(gs)
            gs[order[info.codepoint]].draw(pen)
            d = pen.getCommands()
            if d:  # a space has no contours
                gx = x + pos.x_offset * scale
                paths.append(
                    f'{indent}  <path transform="translate({gx:.3f},{baseline:.3f}) '
                    f'scale({scale:.6f},{-scale:.6f})" d="{d}"/>'
                )
            x += pos.x_advance * scale + lsp
        if paths:
            groups.append(f'{indent}<g fill="{fill}">\n' + "\n".join(paths) + f"\n{indent}</g>")
    return "\n".join(groups)


def parse_runs(inner, outer_fill):
    """Split a <text> body into (string, fill) runs. Handles bare text and <tspan>."""
    runs, pos = [], 0
    for m in re.finditer(r'<tspan([^>]*)>(.*?)</tspan>', inner, re.S):
        if m.start() > pos:
            runs.append((inner[pos:m.start()], outer_fill))
        runs.append((m.group(2), attr(m.group(1), "fill", outer_fill)))
        pos = m.end()
    if pos < len(inner):
        runs.append((inner[pos:], outer_fill))
    cleaned = [(t, f) for t, f in runs if t]
    if any("<" in t for t, _ in cleaned):
        sys.exit(f"outline-svg-text: unsupported markup inside <text>: {inner!r}")
    return cleaned


def convert(path):
    src = pathlib.Path(path)
    svg = src.read_text()
    if "<text" not in svg:
        print(f"{path}: no <text> — nothing to do")
        return False

    def repl(m):
        tag, inner = m.group(1), m.group(2)
        weight = attr(tag, "font-weight", "400")
        size = float(attr(tag, "font-size"))
        lsp = float(attr(tag, "letter-spacing", "0"))
        x = float(attr(tag, "x", "0"))
        y = float(attr(tag, "y", "0"))
        fill = attr(tag, "fill", "#000000")
        runs = parse_runs(inner, fill)
        width = measure(runs, weight, size, lsp)
        anchor = attr(tag, "text-anchor", "start")
        if anchor == "middle":
            x -= width / 2
        elif anchor == "end":
            x -= width
        return outline(runs, weight, size, lsp, x, y, "  ")

    out = re.sub(r"<text([^>]*)>(.*?)</text>", repl, svg, flags=re.S)
    if "<text" in out:
        sys.exit(f"outline-svg-text: {path} still contains <text> after conversion")
    src.write_text(out)
    print(f"{path}: outlined")
    return True


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__.strip().splitlines()[0] + "\n\nusage: outline-svg-text.py <file.svg> ...")
    for p in sys.argv[1:]:
        convert(p)
