# Heraldo icon

A blackletter H over a red rule, like a newspaper masthead.

**Edit `heraldo.svg`, then run `build.py`.** `heraldo.svg` is the single source of
truth. Every other icon file is derived from it, and each derived SVG says
"Generated ... Do not edit".

## Contents

| Path | Contents |
|---|---|
| `heraldo.svg` | The master. Hand-editable in Inkscape, Figma or Illustrator |
| `build.py` | Derives every output below from `heraldo.svg` |
| `tools/from_font.py` | Optional and one-way: regenerates `heraldo.svg` from the font. `build.py` never runs it |
| `font/` | Chomsky (`Chomsky.otf`) and its licence, SIL OFL 1.1 (`OFL.txt`) |
| `masters/` | Generated full icons: `heraldo-{squircle,circle}-{dark,light}.svg`, plus `-small` versions for favicons |
| `layers/` | Generated: the H and the rule as separate SVGs on a transparent ground, per layout |
| `export/` | Generated web set: `favicon.ico`, 16 and 32 px PNGs, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png`, `icon-maskable-512.png` |
| `geometry.json` | Generated: placement figures from the last build |

`build.py --apple` also writes the Apple app icon, `apple/App/AppIcon.icon`.

## The master

`heraldo.svg` is a 1024 x 1024 canvas with no ground. It contains:

- `h`: the H.
- `rule`: the squircle-layout rule.
- A hidden group `alternates` with rule geometry that a transform cannot derive:
  - `rule-small`: a heavier rule, set closer, for favicons.
  - `rule-circle`: a shorter rule, for the circle layout.
  - `rule-circle-small`: the circle rule at favicon weight.

  A shorter rule cannot come from scaling, because non-uniform scaling would change the 39.5 degree end cuts.
- A `<style>` block with the palette classes `ink`, `cream` and `red`. `build.py` reads the colours from it.

Every rule is drawn in the H's frame. `build.py` only transforms this geometry:

- It translates for optical centring.
- It scales for the circle fit.
- It swaps colours and sets opacity.

`build.py` draws no new geometry. Its output is byte-identical from run to run.

## Palette

| Class | Hex | Use |
|---|---|---|
| ink | `#17181E` | Dark ground; the H in light |
| cream | `#F2EBDD` | Light ground; the H in dark |
| red | `#E0313F` | The rule, in both themes |

Light and dark share one geometry. Only the ground and the H swap colours.

## Geometry rules

These rules apply when you change the shapes, by hand or through `tools/from_font.py`.

- **Grid:** 1024 x 1024, full bleed. No baked-in corners; the system applies its own mask.
- **H:** the capital H of [Chomsky](https://github.com/ctrlcctrlv/chomsky), an OFL blackletter by Fredrick R. Brennan.
  - It is 620 px tall, outlined, and grown by 3 px a side (`EMBOLDEN` 6) with the growth merged into the fill.
  - The stem is 113 px wide and the main hairline about 28 px.
  - The hairlines stay at `EMBOLDEN` 6 by decision: the softness at small sizes under Liquid Glass is accepted.
- **Rule:** a taper, thin at the left and thick at the right.
  - The thick end is 0.50 x stem (56.5 px). The thin end is 0.48 x the thick end (27.1 px).
  - Thickness eases in with exponent 1.4. That matches the H's bottom-left swash.
  - Both ends are single clean cuts at 39.5 degrees, the H's own pen angle.
  - The rule is 90% of the glyph width, centred under the H's mass. Its centreline sits 62 px below the H's foot.
- **Circle layout** (watchOS, maskable PWA icon): the rule is 74% of the glyph width, with a 54 px gap. `build.py` scales the mark until no pixel lies beyond 392 px from the centre. The maskable safe zone is 409.6 px.
- **Small rules:** 1.45 x heavier, and set at 0.8 x the gap.
- **Optical centring:** `build.py` treats the H and the rule as one mass.
  - x moves to the area-weighted centroid.
  - y moves to the midpoint of the centroid and the bbox centre.
- **Outline hygiene:**
  - Filled paths only; no strokes.
  - No self-intersections, specks or stray subpaths.
  - No nodes closer than 0.5 units.
  - Straight runs carry no extra nodes.
  - Coordinates use 2 decimals.

  `tools/from_font.py` cleans to these rules. Keep them when you edit by hand.

## Apple app icon

`AppIcon.icon` has two glass groups and no ground in its art:

- **Rule** (front): the red rule, with its own specular highlight and a layer-colour shadow.
- **H** (back): the main glass layer. It uses `h-ink.svg` by default and `h-cream.svg` for the dark and tinted appearances.
- **Ground:** the bundle fill. It is cream by default and ink for the dark appearance.
- **Tinted and clear:** these appearances are monochrome, so the rule switches to `rule-cream.svg` at 60% opacity. It stays a distinct, dimmer bar under the H.

Preview every appearance with `ictool`, from Icon Composer, which ships with Xcode:

```sh
ictool="$(xcode-select -p)/../Applications/Icon Composer.app/Contents/Executables/ictool"
for r in Default Dark ClearLight ClearDark TintedLight TintedDark; do
  "$ictool" apple/App/AppIcon.icon --export-image --output-file "icon-$r.png" \
    --platform iOS --rendition "$r" --width 1024 --height 1024 --scale 1
done
```

## Rebuild

Run from the repository root. You need [uv](https://docs.astral.sh/uv/),
`rsvg-convert` (librsvg) and ImageMagick 7.

```sh
uv run design/icon/build.py           # masters, layers, export/, geometry.json
uv run design/icon/build.py --apple   # also rewrite apple/App/AppIcon.icon
```

Then run `apple/scripts/check.sh` to confirm the app still builds with the icon.

To start the master over from the font, which overwrites `heraldo.svg`:

```sh
uv run design/icon/tools/from_font.py
```
