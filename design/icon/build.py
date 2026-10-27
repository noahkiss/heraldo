# /// script
# requires-python = ">=3.11"
# dependencies = ["fonttools", "skia-pathops", "numpy", "pillow"]
# ///
"""Build the Heraldo icon: SVG masters, Icon Composer layers, the Apple
AppIcon.icon bundle and the web icon set, all from one geometry.

    uv run design/icon/build.py            # masters, layers, export/
    uv run design/icon/build.py --apple    # also rewrite apple/App/AppIcon.icon

Needs rsvg-convert and ImageMagick 7 (magick) on PATH. Every path is relative
to this file. See README.md for the design rules.
"""
import argparse
import io
import json
import math
import shutil
import subprocess
from pathlib import Path

import numpy as np
import pathops
from fontTools.pens.boundsPen import BoundsPen
from fontTools.pens.svgPathPen import SVGPathPen
from fontTools.pens.transformPen import TransformPen
from fontTools.ttLib import TTFont
from PIL import Image

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
FONT = HERE / "font" / "Chomsky.otf"
APPICON = REPO / "apple" / "App" / "AppIcon.icon"

# ---- Palette
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

THEMES = {"dark": (INK, CREAM), "light": (CREAM, INK)}   # (ground, H)


def ntos(v):
    return f"{v:.1f}".rstrip("0").rstrip(".")


def path_d(p):
    pen = SVGPathPen(None, ntos=ntos)
    p.draw(pen)
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
    """Taper from thin (left) to thick (right); ends cut parallel at CUT_ANGLE."""
    top, bot = [], []
    for i in range(SAMPLES + 1):
        u = i / SAMPLES
        t = thick * RATIO + thick * (1 - RATIO) * u ** EASE
        x = x0 + (x1 - x0) * u
        top.append((x, cy - t / 2))
        bot.append((x, cy + t / 2))
    k = 1 / math.tan(CUT_ANGLE)
    top[0] = (x0 + (bot[0][1] - top[0][1]) * k, top[0][1])
    bot[-1] = (x1 - (bot[-1][1] - top[-1][1]) * k, bot[-1][1])
    p = pathops.Path()
    pen = p.getPen()
    pen.moveTo(bot[0])
    for pt in top:
        pen.lineTo(pt)
    for pt in reversed(bot):
        pen.lineTo(pt)
    pen.closePath()
    p.simplify()
    return p


def svg(parts, ground=None):
    bg = f'<rect width="1024" height="1024" fill="{ground}"/>\n  ' if ground else ""
    body = "\n  ".join(f'<path fill="{c}" d="{d}"/>' for c, d in parts)
    return ('<svg xmlns="http://www.w3.org/2000/svg" width="1024" height="1024" '
            f'viewBox="0 0 1024 1024">\n  {bg}{body}\n</svg>\n')


def coverage(*paths):
    png = subprocess.run(["rsvg-convert", "-w", "1024", "-h", "1024"],
                         input=svg([("#000", path_d(p)) for p in paths]).encode(),
                         capture_output=True, check=True).stdout
    return np.asarray(Image.open(io.BytesIO(png)).convert("RGBA"))[:, :, 3] > 127


def compose(layout, small=False):
    """Place H and rule on the 1024 grid, optically centred.

    Optical centring treats H and rule as one mass: x goes to the area-weighted
    centroid, y to the midpoint of the centroid and the bbox centre (pure
    centroid lifts a top-heavy mark too far).
    """
    h, gw = glyph()
    thick = THICK * (SMALL_T_GAIN if small else 1)
    gap = (CIRCLE_GAP if layout == "circle" else GAP) * (SMALL_GAP_GAIN if small else 1)
    rw = gw * (CIRCLE_RULE_W if layout == "circle" else RULE_W)
    # Centre the rule under the H's mass; the left flag skews its bbox.
    hcx = np.nonzero(coverage(moved(h, (1, 0, 0, 1, 512, 700))))[1].mean() - 512
    r = rule(hcx - rw / 2, hcx + rw / 2, gap, thick)
    h, r = moved(h, (1, 0, 0, 1, 512, 512)), moved(r, (1, 0, 0, 1, 512, 512))
    for _ in range(2):
        ys, xs = np.nonzero(coverage(h, r))
        tx = 512 - xs.mean()
        ty = 512 - (ys.mean() + (ys.min() + ys.max()) / 2) / 2
        h, r = moved(h, (1, 0, 0, 1, tx, ty)), moved(r, (1, 0, 0, 1, tx, ty))
        if layout == "circle":
            ys, xs = np.nonzero(coverage(h, r))
            k = CIRCLE_R / np.hypot(xs + 0.5 - 512, ys + 0.5 - 512).max()
            m = (k, 0, 0, k, 512 * (1 - k), 512 * (1 - k))
            h, r = moved(h, m), moved(r, m)
    ys, xs = np.nonzero(coverage(h, r))
    stats = {"bbox": [int(xs.min()), int(ys.min()), int(xs.max()), int(ys.max())],
             "centroid": [round(float(xs.mean()), 1), round(float(ys.mean()), 1)],
             "max_radius": round(float(np.hypot(xs + 0.5 - 512, ys + 0.5 - 512).max()), 1),
             "rule_thick": round(thick, 1), "rule_thin": round(thick * RATIO, 1),
             "rule_area": round(abs(r.area))}
    return h, r, stats


def render(src, dst, px):
    subprocess.run(["rsvg-convert", "-w", str(px), "-h", str(px), str(src), "-o", str(dst)], check=True)


def magick(*args):
    subprocess.run(["magick", *map(str, args)], check=True)


def build_masters():
    masters, layers = HERE / "masters", HERE / "layers"
    for d in (masters, layers):
        d.mkdir(exist_ok=True)
    geometry = {}
    for layout in ("squircle", "circle"):
        h, r, st = compose(layout)
        hs, rs, st_small = compose(layout, small=True)
        geometry[layout], geometry[f"{layout}-small"] = st, st_small
        hd, rd = path_d(h), path_d(r)
        (layers / f"{layout}-h-ink.svg").write_text(svg([(INK, hd)]))
        (layers / f"{layout}-h-cream.svg").write_text(svg([(CREAM, hd)]))
        (layers / f"{layout}-rule.svg").write_text(svg([(RED, rd)]))
        (layers / f"{layout}-rule-cream.svg").write_text(svg([(CREAM, rd)]))
        for theme, (ground, mark) in THEMES.items():
            (masters / f"heraldo-{layout}-{theme}.svg").write_text(svg([(mark, hd), (RED, rd)], ground))
            (masters / f"heraldo-{layout}-{theme}-small.svg").write_text(
                svg([(mark, path_d(hs)), (RED, path_d(rs))], ground))
    (HERE / "geometry.json").write_text(json.dumps(geometry, indent=1) + "\n")
    return geometry


def build_web():
    """Web set from the dark theme: favicons from the small master, the rest from
    the squircle master, the maskable icon from the circle master."""
    out = HERE / "export"
    out.mkdir(exist_ok=True)
    m = HERE / "masters"
    sq, small, circ = (m / "heraldo-squircle-dark.svg", m / "heraldo-squircle-dark-small.svg",
                       m / "heraldo-circle-dark.svg")
    tmp = out / ".tmp-1024.png"
    render(small, tmp, 1024)
    magick(tmp, "-resize", "32x32", "-unsharp", "0x0.5", out / "favicon-32x32.png")
    magick(tmp, "-resize", "16x16", "-unsharp", "0x0.5", out / "favicon-16x16.png")
    magick(out / "favicon-16x16.png", out / "favicon-32x32.png", out / "favicon.ico")
    render(sq, tmp, 1024)
    magick(tmp, "-resize", "180x180", "-background", INK, "-alpha", "remove", "-alpha", "off",
           out / "apple-touch-icon.png")
    magick(tmp, "-resize", "192x192", out / "icon-192.png")
    magick(tmp, "-resize", "512x512", out / "icon-512.png")
    render(circ, tmp, 1024)
    magick(tmp, "-resize", "512x512", out / "icon-maskable-512.png")
    tmp.unlink()


ICON_JSON = {
    "fill-specializations": [
        {"value": {"solid": "srgb:0.94902,0.92157,0.86667,1"}},          # cream
        {"appearance": "dark", "value": {"solid": "srgb:0.09020,0.09412,0.11765,1"}},  # ink
    ],
    "groups": [
        {   # Front: the rule catches its own highlight above the H.
            "name": "Rule",
            "layers": [{
                "name": "rule", "glass": True,
                # Tinted and clear are monochrome and map luminance: red would go dark
                # and vanish on a dark tint. A cream rule at reduced opacity stays a
                # distinct, dimmer bar under the full-strength H.
                "image-name-specializations": [
                    {"value": "rule.svg"},
                    {"appearance": "tinted", "value": "rule-cream.svg"},
                ],
                "opacity-specializations": [{"value": 1}, {"appearance": "tinted", "value": 0.6}],
            }],
            "shadow": {"kind": "layer-color", "opacity": 0.35},
            "translucency": {"enabled": True, "value": 0.2},
            "lighting": "individual",
            "specular": True,
        },
        {   # Back: the H is the main glass layer.
            "name": "H",
            "layers": [{
                "name": "h",
                "image-name-specializations": [
                    {"value": "h-ink.svg"},
                    {"appearance": "dark", "value": "h-cream.svg"},
                    {"appearance": "tinted", "value": "h-cream.svg"},
                ],
                "glass": True,
            }],
            "shadow": {"kind": "neutral", "opacity": 0.5},
            "translucency": {"enabled": True, "value": 0.35},
            "lighting": "combined",
            "specular": True,
        },
    ],
    "supported-platforms": {"squares": "shared", "circles": ["watchOS"]},
}


def build_apple():
    assets = APPICON / "Assets"
    if assets.exists():
        shutil.rmtree(assets)
    assets.mkdir(parents=True)
    layers = HERE / "layers"
    for src, dst in (("squircle-h-ink.svg", "h-ink.svg"), ("squircle-h-cream.svg", "h-cream.svg"),
                     ("squircle-rule.svg", "rule.svg"), ("squircle-rule-cream.svg", "rule-cream.svg")):
        shutil.copy(layers / src, assets / dst)
    (APPICON / "icon.json").write_text(json.dumps(ICON_JSON, indent=2) + "\n")


def main():
    ap = argparse.ArgumentParser(description="Build the Heraldo icon.")
    ap.add_argument("--apple", action="store_true", help="also rewrite apple/App/AppIcon.icon")
    a = ap.parse_args()
    geometry = build_masters()
    build_web()
    if a.apple:
        build_apple()
    print(json.dumps(geometry, indent=1))


if __name__ == "__main__":
    main()
