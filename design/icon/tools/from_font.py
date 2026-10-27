# /// script
# requires-python = ">=3.11"
# dependencies = ["fonttools", "skia-pathops", "numpy", "pillow"]
# ///
"""Regenerate design/icon/heraldo.svg from the Chomsky font. Optional and one-way.

heraldo.svg is the icon's source of truth, and build.py reads only that file.
Run this script only to start the master over from the font and the
parameters below. It overwrites heraldo.svg, so any hand edits made there are
lost, and so is the wordmarks group: run tools/wordmark_from_font.py after it.
build.py never runs it.

    uv run design/icon/tools/from_font.py

It outlines Chomsky's H, grows the outline by EMBOLDEN and merges the growth
into the fill. Then it draws the four rules in the H's frame: the squircle rule,
its small-size version, and the shorter circle rule with its small-size version.
It places the H and the squircle rule optically centred on the 1024 grid, cleans
every outline, and writes the file. Needs rsvg-convert.
"""
import io
import math
import subprocess
from pathlib import Path

import numpy as np
import pathops
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.recordingPen import RecordingPen
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from PIL import Image

ICON = Path(__file__).resolve().parent.parent
FONT = ICON / "font" / "Chomsky.otf"
MASTER = ICON / "heraldo.svg"

INK, CREAM, RED = "#17181E", "#F2EBDD", "#E0313F"

# ---- The H: Chomsky's capital H, outlined and emboldened.
H_HEIGHT = 620                 # glyph height on the 1024 grid
EMBOLDEN = 6                   # outline grown by 3 px each side, merged into the fill
STEM = 113                     # measured stem width at H_HEIGHT, embolden included

# ---- The rule: a taper, thin at the left and thick at the right, with
# blunt nib cuts at the H's own pen angle. Ratio and ease come from the H's
# bottom-left swash (thin/thick 0.48, thickening with exponent 1.4).
CUT_ANGLE = math.radians(39.5)  # the H's stem-cap and terminal cut angle
THICK = 0.50 * STEM             # thick end
RATIO = 0.48                    # thin end / thick end
EASE = 1.4
RULE_W = 0.90                   # rule width / glyph width (squircle)
GAP = 62                        # glyph foot to rule centreline (squircle)
SAMPLES = 32

# ---- Circle layout (watchOS, PWA maskable): shorter rule, tighter gap, and the
# whole mark scaled until no pixel lies beyond CIRCLE_R (safe zone is 409.6).
CIRCLE_RULE_W = 0.74
CIRCLE_GAP = 54
CIRCLE_R = 392

# ---- Small-size master (favicons): heavier rule, set closer.
SMALL_T_GAIN = 1.45
SMALL_GAP_GAIN = 0.8


def ntos(v):
    return f"{v:.2f}".rstrip("0").rstrip(".")


def path_d(p):
    """SVG path data, without the explicit closing segment pathops draws back to
    each contour's start (Z already closes it)."""
    rec = RecordingPen()
    p.draw(rec)
    ops, start = list(rec.value), None
    keep = []
    for i, (op, args) in enumerate(ops):
        if op == "moveTo":
            start = args[0]
        nxt = ops[i + 1][0] if i + 1 < len(ops) else None
        if op == "lineTo" and nxt == "closePath" and ntos(args[0][0]) == ntos(start[0]) \
                and ntos(args[0][1]) == ntos(start[1]):
            continue
        keep.append((op, args))
    pen = SVGPathPen(None, ntos=ntos)
    for op, args in keep:
        getattr(pen, op)(*args)
    return pen.getCommands()


def moved(p, m):
    out = pathops.Path()
    p.draw(TransformPen(out.getPen(), m))
    return out


def glyph():
    """Emboldened H as one filled outline; foot at y=0, bbox centred on x=0."""
    f = TTFont(FONT)
    gs = f.getGlyphSet()
    name = f.getBestCmap()[ord("H")]
    bp = BoundsPen(gs)
    gs[name].draw(bp)
    x0, y0, x1, y1 = bp.bounds
    s = H_HEIGHT / (y1 - y0)
    fill = pathops.Path()
    gs[name].draw(TransformPen(fill.getPen(), (s, 0, 0, -s, -(x0 + x1) / 2 * s, y0 * s)))
    fill.simplify()
    band = pathops.Path()
    fill.draw(band.getPen())
    band.stroke(EMBOLDEN, pathops.LineCap.ROUND_CAP, pathops.LineJoin.ROUND_JOIN, 4)
    band.convertConicsToQuads()
    return pathops.op(fill, band, pathops.PathOp.UNION), (x1 - x0) * s


def rule(x0, x1, cy, thick):
    """Taper from thin (left) to thick (right); ends cut parallel at CUT_ANGLE.

    The uncut taper is intersected with the band between two parallel cut
    lines: one through the bottom-left corner, one through the top-right
    corner. Clipping, rather than moving corner points, keeps each end a
    single clean diagonal however far the cut runs along the edge.
    """
    top, bot = [], []
    for i in range(SAMPLES + 1):
        u = i / SAMPLES
        t = thick * RATIO + thick * (1 - RATIO) * u ** EASE
        x = x0 + (x1 - x0) * u
        top.append((x, cy - t / 2))
        bot.append((x, cy + t / 2))
    body = pathops.Path()
    pen = body.getPen()
    pen.moveTo(bot[0])
    for pt in top:
        pen.lineTo(pt)
    for pt in reversed(bot):
        pen.lineTo(pt)
    pen.closePath()
    # Cut lines run up and to the right at CUT_ANGLE (SVG y grows down).
    dx, dy = math.cos(CUT_ANGLE), -math.sin(CUT_ANGLE)
    reach = 4 * thick
    left, right = bot[0], top[-1]
    band = pathops.Path()
    pen = band.getPen()
    pen.moveTo((left[0] - dx * reach, left[1] - dy * reach))
    pen.lineTo((left[0] + dx * reach, left[1] + dy * reach))
    pen.lineTo((right[0] + dx * reach, right[1] + dy * reach))
    pen.lineTo((right[0] - dx * reach, right[1] - dy * reach))
    pen.closePath()
    return pathops.op(body, band, pathops.PathOp.INTERSECTION)


# ---- Outline cleanup
MERGE_DIST = 0.5      # merge on-curve nodes closer than this
LINE_TOL = 0.05       # drop polyline nodes that sit within this of a straight run
MIN_AREA = 4.0        # drop specks and pinholes smaller than this (px^2)


def _dp(pts, tol):
    """Douglas-Peucker on a polyline, keeping both ends."""
    if len(pts) < 3:
        return pts
    (ax, ay), (bx, by) = pts[0], pts[-1]
    L = math.hypot(bx - ax, by - ay) or 1e-9
    i, dmax = 0, -1.0
    for k in range(1, len(pts) - 1):
        d = abs((bx - ax) * (ay - pts[k][1]) - (ax - pts[k][0]) * (by - ay)) / L
        if d > dmax:
            i, dmax = k, d
    if dmax <= tol:
        return [pts[0], pts[-1]]
    return _dp(pts[:i + 1], tol)[:-1] + _dp(pts[i:], tol)


def clean(p):
    """Simplify, then per contour: drop the duplicate closing node, merge nodes
    closer than MERGE_DIST, thin straight polyline runs, and drop specks.
    Returns (path, stats)."""
    q = pathops.Path()
    p.draw(q.getPen())
    q.simplify()
    rec = RecordingPen()
    q.draw(rec)
    contours, cur = [], None
    for op, args in rec.value:
        if op == "moveTo":
            cur = [args[0], []]
            contours.append(cur)
        elif op != "closePath":
            cur[1].append((op, list(args)))
    st = {"merged": 0, "dup_closing": 0, "thinned": 0, "specks": 0}
    out = pathops.Path()
    pen = out.getPen()
    for start, segs in contours:
        if segs and segs[-1][0] == "lineTo" and math.dist(segs[-1][1][-1], start) < 1e-6:
            segs.pop()
            st["dup_closing"] += 1
        kept, at = [], start
        for op, pts in segs:
            if math.dist(pts[-1], at) < MERGE_DIST and all(math.dist(c, at) < MERGE_DIST for c in pts):
                st["merged"] += 1
                continue
            kept.append((op, pts))
            at = pts[-1]
        if kept and math.dist(kept[-1][1][-1], start) < MERGE_DIST and kept[-1][0] == "lineTo":
            kept.pop()
            st["merged"] += 1
        # Thin runs of consecutive line segments.
        thinned, run, prev = [], [], start
        def flush():
            nonlocal run
            if run:
                pts = _dp([prev_run_start[0]] + run, LINE_TOL)[1:]
                st["thinned"] += len(run) - len(pts)
                thinned.extend(("lineTo", [pt]) for pt in pts)
                run = []
        prev_run_start = [start]
        for op, pts in kept:
            if op == "lineTo":
                if not run:
                    prev_run_start = [prev]
                run.append(pts[-1])
            else:
                flush()
                thinned.append((op, pts))
            prev = pts[-1]
        flush()
        poly = [start] + [pts[-1] for _, pts in thinned]
        area = 0.5 * abs(sum(poly[i][0] * poly[i - 1][1] - poly[i - 1][0] * poly[i][1] for i in range(len(poly))))
        if len(poly) < 3 or area < MIN_AREA:
            st["specks"] += 1
            continue
        pen.moveTo(start)
        for op, pts in thinned:
            getattr(pen, op)(*pts)
        pen.closePath()
    return out, st


def coverage(*paths):
    svg = ('<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">'
           + "".join(f'<path d="{path_d(p)}"/>' for p in paths) + "</svg>")
    png = subprocess.run(["rsvg-convert", "-w", "1024", "-h", "1024"], input=svg.encode(),
                         capture_output=True, check=True).stdout
    return np.asarray(Image.open(io.BytesIO(png)).convert("RGBA"))[:, :, 3] > 127


RULES = {   # id: (width share of the glyph, gap, thickness gain, gap gain)
    "rule": (RULE_W, GAP, 1, 1),
    "rule-small": (RULE_W, GAP, SMALL_T_GAIN, SMALL_GAP_GAIN),
    "rule-circle": (CIRCLE_RULE_W, CIRCLE_GAP, 1, 1),
    "rule-circle-small": (CIRCLE_RULE_W, CIRCLE_GAP, SMALL_T_GAIN, SMALL_GAP_GAIN),
}


def main():
    h, gw = glyph()
    # Every rule sits under the H's mass, not its bbox; the left flag skews the bbox.
    hcx = np.nonzero(coverage(moved(h, (1, 0, 0, 1, 512, 700))))[1].mean() - 512
    rules = {}
    for rid, (share, gap, tgain, ggain) in RULES.items():
        rw = gw * share
        rules[rid] = rule(hcx - rw / 2, hcx + rw / 2, gap * ggain, THICK * tgain)
    # Optical centring of H + squircle rule (build.py repeats this per layout).
    m = (1, 0, 0, 1, 512, 512)
    for _ in range(2):
        ys, xs = np.nonzero(coverage(moved(h, m), moved(rules["rule"], m)))
        m = (1, 0, 0, 1, m[4] + 512 - xs.mean(),
             m[5] + 512 - (ys.mean() + (ys.min() + ys.max()) / 2) / 2)
    h = clean(moved(h, m))[0]
    rules = {k: clean(moved(r, m))[0] for k, r in rules.items()}
    alt = "\n".join(f'    <path id="{k}" class="red" d="{path_d(r)}"/>'
                    for k, r in rules.items() if k != "rule")
    MASTER.write_text(f"""<?xml version="1.0" encoding="UTF-8"?>
<!-- Heraldo icon: source of truth; build.py derives everything else.
     Edit this file, then run design/icon/build.py (see README.md).
     Paths: h (the H) and rule (squircle layout). Hidden group "alternates":
     rule-small (favicon weight), rule-circle and rule-circle-small (circle
     layout), all drawn in the H's frame. Palette: classes ink, cream, red. -->
<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" viewBox="0 0 1024 1024">
  <style>
    .ink {{ fill: {INK}; }}
    .cream {{ fill: {CREAM}; }}
    .red {{ fill: {RED}; }}
  </style>
  <path id="h" class="ink" d="{path_d(h)}"/>
  <path id="rule" class="red" d="{path_d(rules['rule'])}"/>
  <g id="alternates" style="display:none">
{alt}
  </g>
</svg>
""")
    print(MASTER)


if __name__ == "__main__":
    main()
