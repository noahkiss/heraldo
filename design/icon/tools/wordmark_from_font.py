# /// script
# requires-python = ">=3.11"
# dependencies = ["fonttools", "skia-pathops", "numpy", "pillow"]
# ///
"""Outline the Heraldo wordmark from Chomsky into heraldo.svg. Optional and one-way.

    uv run design/icon/tools/wordmark_from_font.py

It sets "Heraldo" in Chomsky at the icon H's size and weight, applies the manual
kerns below, and draws the tapered rule under the word with the icon rule's
recipe (from_font.py). Then it replaces the hidden `wordmarks` group in
heraldo.svg, and leaves every other element alone. build.py reads that group
and never needs the font.

The group holds two versions, both in the frame of the icon's H:

- `wordmark`: display sizes. Path ids `wordmark-h`, `wordmark-e`, ... and
  `wordmark-rule`.
- `wordmark-small`: header sizes (below about 48 px tall). Looser tracking, and
  the icon's small-size rule (heavier, set closer). Ids `wordmark-small-*`.

The H of each version is the master's `h` path itself, so the wordmark and the
icon share one H.
"""
import re
import sys
from pathlib import Path

import pathops
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.recordingPen import DecomposingRecordingPen
from fontTools.pens.transformPen import TransformPen
from fontTools.svgLib.path import parse_path
from fontTools.ttLib import TTFont

sys.path.insert(0, str(Path(__file__).resolve().parent))
import from_font as ff  # noqa: E402  (shared recipe: rule, embolden, clean, path_d)

TEXT = "Heraldo"
# Manual kerns per letter pair, in thousandths of the H's cap height, added to
# the font's advances. Set by eye on the outlined letters at display size.
KERNS = {"He": -34, "er": 0, "ra": 8, "al": 10, "ld": 0, "do": 0}
SMALL_TRACK = 55            # wordmark-small: extra space per pair, same units
RULE_SPAN = 1.0             # rule length / word ink width
SMALL_T_GAIN, SMALL_GAP_GAIN = ff.SMALL_T_GAIN, ff.SMALL_GAP_GAIN
CLEAR = ff.STEM             # clear-space unit X: the H's stem width
GUIDE = ("Clear space: X = the H stem (113 units) on every side, drawn as the "
         "*-clearspace rects. Use wordmark at 48 px tall and up, wordmark-small "
         "from 20 to 48 px, and the mark alone below 20 px.")


def glyph_run(track):
    """Each letter as an emboldened outline in from_font.glyph()'s frame:
    the H's foot at y=0, its bbox centred on x=0."""
    f = TTFont(ff.FONT)
    gs = f.getGlyphSet()
    cmap = f.getBestCmap()
    hname = cmap[ord("H")]
    bp = BoundsPen(gs)
    gs[hname].draw(bp)
    hx0, hy0, hx1, hy1 = bp.bounds
    s = ff.H_HEIGHT / (hy1 - hy0)
    cap = hy1 * s
    x = -(hx0 + hx1) / 2 * s
    out = []
    for i, ch in enumerate(TEXT):
        name = cmap[ord(ch)]
        rec = DecomposingRecordingPen(gs)
        gs[name].draw(rec)
        p = pathops.Path()
        rec.replay(TransformPen(p.getPen(), (s, 0, 0, -s, x, hy0 * s)))
        p.simplify()
        band = pathops.Path()
        p.draw(band.getPen())
        band.stroke(ff.EMBOLDEN, pathops.LineCap.ROUND_CAP, pathops.LineJoin.ROUND_JOIN, 4)
        band.convertConicsToQuads()
        out.append((ch, pathops.op(p, band, pathops.PathOp.UNION)))
        x += f["hmtx"][name][0] * s
        if i + 1 < len(TEXT):
            x += (KERNS.get(TEXT[i:i + 2], 0) + track) * cap / 1000
    return out


def master_h():
    src = ff.MASTER.read_text()
    d = re.search(r'<path id="h"[^>]*\sd="([^"]+)"', src).group(1)
    p = pathops.Path()
    parse_path(d, p.getPen())
    return p, src


def version(prefix, track, tgain, ggain, h):
    letters = glyph_run(track)
    # Move the run so its H lands on the master's H, then use the master H itself.
    hx0, hy0, _, _ = letters[0][1].bounds
    mx0, my0, _, _ = h.bounds
    m = (1, 0, 0, 1, mx0 - hx0, my0 - hy0)
    paths = [("h", h)] + [(ch, ff.clean(ff.moved(p, m))[0]) for ch, p in letters[1:]]
    ink = [p.bounds for _, p in paths]
    x0, x1 = min(b[0] for b in ink), max(b[2] for b in ink)
    foot = max(b[3] for b in ink)
    w, cx = (x1 - x0) * RULE_SPAN, (x0 + x1) / 2
    rule = ff.clean(ff.rule(cx - w / 2, cx + w / 2, foot + ff.GAP * ggain, ff.THICK * tgain))[0]
    lines = [f'    <g id="{prefix}">']
    for i, (ch, p) in enumerate(paths):
        pid = f"{prefix}-{ch.lower()}" + ("" if TEXT.lower().count(ch.lower()) == 1 else str(i))
        lines.append(f'      <path id="{pid}" class="ink" d="{ff.path_d(p)}"/>')
    lines.append(f'      <path id="{prefix}-rule" class="red" d="{ff.path_d(rule)}"/>')
    bx0, by0, bx1, by1 = (min(b[0] for b in ink + [rule.bounds]), min(b[1] for b in ink + [rule.bounds]),
                          max(b[2] for b in ink + [rule.bounds]), max(b[3] for b in ink + [rule.bounds]))
    lines.append(f'      <rect id="{prefix}-clearspace" fill="none" x="{ff.ntos(bx0 - CLEAR)}" '
                 f'y="{ff.ntos(by0 - CLEAR)}" width="{ff.ntos(bx1 - bx0 + 2 * CLEAR)}" '
                 f'height="{ff.ntos(by1 - by0 + 2 * CLEAR)}"/>')
    lines.append("    </g>")
    return "\n".join(lines)


def main():
    h, src = master_h()
    group = "\n".join([
        '  <g id="wordmarks" style="display:none">',
        f"    <desc>{GUIDE}</desc>",
        version("wordmark", 0, 1, 1, h),
        version("wordmark-small", SMALL_TRACK, SMALL_T_GAIN, SMALL_GAP_GAIN, h),
        "  </g>",
    ])
    if 'id="wordmarks"' in src:
        src = re.sub(r'  <g id="wordmarks".*?\n  </g>', lambda _: group, src, flags=re.S)
    else:
        src = src.replace("</svg>", group + "\n</svg>")
    ff.MASTER.write_text(src)
    print(ff.MASTER)


if __name__ == "__main__":
    main()
