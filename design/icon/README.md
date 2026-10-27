# Heraldo icon

A blackletter H over a red rule, like a newspaper masthead. This folder holds the
SVG masters, the layers for the Apple app icon, the web icon set, and the script
that builds them all.

## Contents

| Path | Contents |
|---|---|
| `build.py` | Builds everything below from the font and the rules in this file |
| `font/` | Chomsky (`Chomsky.otf`) and its licence, SIL OFL 1.1 (`OFL.txt`) |
| `masters/` | Full icons on a 1024 grid: `heraldo-{squircle,circle}-{dark,light}.svg`, plus `-small` versions for favicons |
| `layers/` | The H and the rule as separate SVGs on a transparent ground, per layout |
| `export/` | Web set: `favicon.ico`, 16 and 32 px PNGs, `apple-touch-icon.png`, `icon-192.png`, `icon-512.png`, `icon-maskable-512.png` |
| `geometry.json` | Placement and rule figures from the last build |

The Apple app icon is `apple/App/AppIcon.icon`, an Icon Composer bundle that
`build.py --apple` writes.

## Source

The H is the capital H of [Chomsky](https://github.com/ctrlcctrlv/chomsky), an
OFL blackletter by Fredrick R. Brennan in the style of a newspaper nameplate.
`build.py` outlines the glyph with fontTools. It grows the outline by 3 px a side
and merges the growth into the fill, so the hairlines hold at small sizes. Every
shape is a filled path; no SVG uses a stroke.

## Palette

| Token | Hex | Use |
|---|---|---|
| ink | `#17181E` | Dark ground; the H in light |
| cream | `#F2EBDD` | Light ground; the H in dark |
| red | `#E0313F` | The rule, in both themes |

Light and dark share one geometry. Only the ground and the H swap colours.

## Geometry rules

- **Grid:** 1024 x 1024, full bleed. No baked-in corners; the system applies its own mask.
- **H:** 620 px tall. The stem is 113 px wide and the hairline is about 28 px.
- **Rule:** a taper, thin at the left and thick at the right.
  - The thick end is 0.50 x stem (56.5 px). The thin end is 0.48 x the thick end (27.1 px).
  - Thickness eases in with exponent 1.4. That matches the H's bottom-left swash.
  - Both ends are blunt cuts at 39.5 degrees, the H's own pen angle.
  - The rule is 90% of the glyph width, centred under the H's mass. Its centreline sits 62 px below the H's foot.
- **Optical centring:** the H and the rule count as one mass.
  - x moves to the area-weighted centroid.
  - y moves to the midpoint of the centroid and the bbox centre.
- **Circle layout** (watchOS, maskable PWA icon):
  - The rule shrinks to 74% of the glyph width, with a 54 px gap.
  - The mark is scaled until no pixel lies beyond 392 px from the centre. The maskable safe zone is 409.6 px.
- **Small masters** (favicons): the rule is 1.45 x heavier and sits 0.8 x the gap from the H.

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
uv run design/icon/build.py           # masters, layers, export/
uv run design/icon/build.py --apple   # also rewrite apple/App/AppIcon.icon
```

Then run `apple/scripts/check.sh` to confirm the app still builds with the icon.
