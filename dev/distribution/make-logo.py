#!/usr/bin/env python3
"""Regenerate dev/distribution/runlore.svg — the stacked logo for cncf/landscape.

Why this exists rather than submitting assets/logo-wordmark.svg directly: that file
contains a live <text> element requesting font-family 'Space Grotesk', but the font's
actual family name is "Space Grotesk SemiBold". The request never matches, so renderers
silently fall back to something else (Noto Sans, on the machine this was written on). A
logo that renders in an arbitrary typeface is not a logo.

This converts the text to outlines using HarfBuzz — the same shaper librsvg and browsers
use — so kerning and advances match what was intended, and the result carries no font
dependency at all.

Usage:
    python3 -m venv .venv && .venv/bin/pip install uharfbuzz fonttools
    .venv/bin/python dev/distribution/make-logo.py > dev/distribution/runlore.svg

Requires SpaceGrotesk-600.ttf; override its location with SPACE_GROTESK_TTF=<path>.
Defaults to ~/.fonts/SpaceGrotesk-600.ttf.
"""

import os
import pathlib
import re
import sys

import uharfbuzz as hb
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.ttLib import TTFont

REPO = pathlib.Path(__file__).resolve().parents[2]
FONT = os.environ.get(
    "SPACE_GROTESK_TTF", str(pathlib.Path.home() / ".fonts/SpaceGrotesk-600.ttf")
)
MARK = REPO / "assets/logo-mark.svg"

# Type set to match assets/logo-wordmark.svg exactly.
SIZE, LSP = 86.0, -2.0

# The mark's real INK bounds. Its viewBox is a nominal 220x220, but the artwork occupies
# x 40..180, y 30..190 — laying out against the viewBox leaves visible dead margin and
# pushes the wordmark too far from the mark.
MARK_INK_X0, MARK_INK_X1 = 40.0, 180.0
MARK_INK_Y0, MARK_INK_Y1 = 30.0, 190.0
GAP = 30.0

if not pathlib.Path(FONT).is_file():
    sys.exit(f"make-logo: font not found: {FONT}\nSet SPACE_GROTESK_TTF to its location.")

blob = hb.Blob.from_file_path(FONT)
hbfont = hb.Font(hb.Face(blob))
tt = TTFont(FONT)
upem = tt["head"].unitsPerEm
gs = tt.getGlyphSet()
order = tt.getGlyphOrder()
scale = SIZE / upem


def shape(text, x, base):
    """Outline `text` at pen position x on baseline `base`. Returns (paths, end-x)."""
    buf = hb.Buffer()
    buf.add_str(text)
    buf.guess_segment_properties()
    hb.shape(hbfont, buf, {"kern": True, "liga": True})
    parts = []
    for info, pos in zip(buf.glyph_infos, buf.glyph_positions):
        pen = SVGPathPen(gs)
        gs[order[info.codepoint]].draw(pen)
        d = pen.getCommands()
        if d:
            # Font space is y-up, SVG is y-down — hence the negative y scale.
            parts.append(
                f'<path transform="translate({x + pos.x_offset * scale:.3f},{base:.3f}) '
                f'scale({scale:.6f},{-scale:.6f})" d="{d}"/>'
            )
        x += pos.x_advance * scale + LSP
    return parts, x


# Measure the whole word first so it can be centred under the mark. Trailing
# letter-spacing is advance, not ink, so it is not part of the width.
_, end = shape("runlore", 0.0, 0.0)
text_w = end - LSP

mark_w = MARK_INK_X1 - MARK_INK_X0
W = max(mark_w, text_w)
mark_x = (W - mark_w) / 2 - MARK_INK_X0
mark_y = -MARK_INK_Y0
base = (MARK_INK_Y1 - MARK_INK_Y0) + GAP + SIZE * 0.72  # cap-height baseline
H = base + SIZE * 0.24                                  # descender room

dark, x = shape("run", (W - text_w) / 2, base)
teal, _ = shape("lore", x, base)

body = re.search(r"<svg[^>]*>(.*)</svg>", MARK.read_text(), re.S).group(1).strip()

out = [
    f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W:.0f} {H:.0f}" '
    f'width="{W:.0f}" height="{H:.0f}">',
    f'  <g transform="translate({mark_x:.3f},{mark_y:.3f})">{body}</g>',
    '  <g fill="#101f4b">\n    ' + "\n    ".join(dark) + "\n  </g>",
    '  <g fill="#14c9a6">\n    ' + "\n    ".join(teal) + "\n  </g>",
    "</svg>",
]
print("\n".join(out))
