# Heraldo icon and wordmark

A blackletter H over a red rule, like a newspaper masthead. The wordmark sets the
whole name the same way: "Heraldo" in Chomsky over the rule.

**Edit `heraldo.svg`, then run `build.py`.** `heraldo.svg` is the single source of
truth. Every other icon file is derived from it, and each derived SVG says
"Generated ... Do not edit".

## Contents

| Path | Contents |
|---|---|
| `heraldo.svg` | The master. Hand-editable in Inkscape, Figma or Illustrator |
| `build.py` | Derives every output below from `heraldo.svg` |
| `tools/from_font.py` | Optional and one-way: regenerates `heraldo.svg` from the font. `build.py` never runs it |
| `tools/wordmark_from_font.py` | Optional and one-way: rewrites the `wordmarks` group in `heraldo.svg` from the font. `build.py` never runs it |
| `font/` | Chomsky (`Chomsky.otf`) and its licence, SIL OFL 1.1 (`OFL.txt`) |
| `masters/` | Generated full icons: `heraldo-{squircle,circle}-{dark,light}.svg`, plus `-small` versions for favicons |
| `layers/` | Generated: the H and the rule as separate SVGs on a transparent ground, per layout |
| `export/` | Generated web set: `favicon.ico`, 16 and 32 px PNGs, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png`, `icon-maskable-512.png`. `export/logo/` holds the logos, wordmarks, README banners and og-images |
| `geometry.json` | Generated: placement figures from the last build |

`build.py` also writes the app's logo assets, and `build.py --apple` the Apple app icon. See "Derived assets".

## The master

`heraldo.svg` is a 1024 x 1024 canvas with no ground. It contains:

- `h`: the H.
- `rule`: the squircle-layout rule.
- A hidden group `alternates` with rule geometry that a transform cannot derive:
  - `rule-small`: a heavier rule, set closer, for favicons.
  - `rule-circle`: a shorter rule, for the circle layout.
  - `rule-circle-small`: the circle rule at favicon weight.

  A shorter rule cannot come from scaling, because non-uniform scaling would change the 39.5 degree end cuts.
- A hidden group `wordmarks` with the wordmark in two versions (see "Wordmark"):
  - `wordmark`: paths `wordmark-{h,e,r,a,l,d,o}` and `wordmark-rule`.
  - `wordmark-small`: the same ids with `wordmark-small-`.

  Each version also has a `-clearspace` guide rect, and the group's `<desc>` states the size rules.
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

## Derived assets

Every file below comes from `heraldo.svg`. `build.py` writes them all; `--apple` adds the `AppIcon.icon` row.
Generated SVGs start with a "Do not edit" comment. The PNGs carry no comment; this table marks them as generated.

| Asset | Source elements | Treatment | Used for |
|---|---|---|---|
| `masters/heraldo-squircle-{dark,light}.svg` | `h`, `rule` | Optical centring, ground, theme colours | Reference full icons; web set source |
| `masters/heraldo-squircle-{dark,light}-small.svg` | `h`, `rule-small` | As above | Favicons |
| `masters/heraldo-circle-{dark,light}.svg` | `h`, `rule-circle` | Centring, scaled to r 392 | Maskable PWA icon; watchOS reference |
| `masters/heraldo-circle-{dark,light}-small.svg` | `h`, `rule-circle-small` | As above | Small circle uses |
| `layers/{squircle,circle}-{h-ink,h-cream,rule,rule-cream}.svg` | `h`, `rule` or `rule-circle` | Centring, one part per file, no ground | Icon Composer layers |
| `export/favicon.ico`, `favicon-{16x16,32x32}.png` | `h`, `rule-small` | Dark squircle small master | Browser tabs |
| `export/apple-touch-icon.png`, `icon-192.png`, `icon-512.png` | `h`, `rule` | Dark squircle master | iOS home-screen bookmark, PWA manifest |
| `export/icon-maskable-512.png` | `h`, `rule-circle` | Dark circle master | PWA manifest, `purpose: maskable` |
| `export/logo/heraldo-mark-{light,dark,mono}.svg` | `h`, `rule` | Cropped to the mark; mono uses `currentColor` | Landing page and docs headers |
| `export/logo/heraldo-mark-{light,dark}-40.png`, `@2x` | `h`, `rule` | As above, 40 px tall | Header logo at 1x and 2x |
| `export/logo/heraldo-wordmark-{light,dark,mono}.svg` | `wordmark` | Cropped to the ink; mono uses `currentColor` | Wordmark at 48 px tall and up |
| `export/logo/heraldo-wordmark-small-{light,dark,mono}.svg` | `wordmark-small` | As above | Wordmark from 20 to 48 px tall |
| `export/logo/heraldo-wordmark-{light,dark}-28.png`, `@2x` | `wordmark-small` | 28 px tall | Header wordmark at 1x and 2x |
| `export/logo/readme-banner-{light,dark}.{svg,png}` | `wordmark` | Centred on cream or ink, 1280 x 320 | Repository README |
| `export/logo/og-image.{svg,png}` | `wordmark` | Dark wordmark centred on ink, 1200 x 630 | Link previews (Open Graph), the default |
| `export/logo/og-image-mark.{svg,png}` | `h`, `rule` | Dark mark centred on ink, 1200 x 630 | Link previews where the name already shows |
| `apple/App/AppIcon.icon` | `h`, `rule` | Squircle layers plus `icon.json` | App icon (Liquid Glass) |
| `apple/App/Shared/Assets.xcassets/HeraldoMark` | `h`, `rule` | Cropped, light and dark appearances, vector | In-app headers and empty states |
| `apple/App/Shared/Assets.xcassets/HeraldoMarkTemplate` | `h`, `rule` | Cropped, one colour, template rendering | Tinted in-app mark |
| `apple/App/Shared/Assets.xcassets/MenuBarIcon` | `h`, `rule-small` | Cropped, one colour, template, 18 pt tall | macOS menu bar |
| `apple/App/Shared/Assets.xcassets/HeraldoWordmark` | `wordmark` | Cropped, light and dark appearances, vector, 64 pt tall | About screen |

## Wordmark

"Heraldo" in Chomsky, with the red rule under the whole word. The compact form is the mark alone.

- **One H.** The wordmark's H is the master's `h` path. Change the H, then rerun `tools/wordmark_from_font.py`.
- **Kerning:** the font's advances plus manual kerns, in thousandths of the H's cap height: H-e -34, r-a +8, a-l +10. They are set by eye on the outlined letters.
- **Rule:** the icon recipe (thick end 0.50 x stem, thin end 0.48 x thick, ease 1.4, 39.5 degree cuts). It spans the word's ink width, and its centreline sits 62 px below the lowest ink.
- **`wordmark-small`:** for header sizes. Each letter pair gets 55 thousandths of the cap height more space, so the letters stay apart at 28 px. The rule takes the icon's small recipe: 1.45 x heavier, at 0.8 x the gap.
- **Clear space:** X is the H's stem width (113 units, 18% of the H's height). Keep 1X clear on every side. The `-clearspace` rects in the master draw it.
- **Minimum size,** by the wordmark's full height, rule included:

  | Height | Use |
  |---|---|
  | 48 px and up | `wordmark` |
  | 20 to 48 px | `wordmark-small` |
  | Under 20 px | The mark alone. A 16 px favicon is always the mark |

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
uv run design/icon/tools/wordmark_from_font.py   # from_font.py drops the wordmarks group
```

To change the wordmark's kerns or tracking, edit them in `tools/wordmark_from_font.py`, run it, then run `build.py`.
