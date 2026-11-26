#!/bin/sh
# Notarize and staple the Release build that scripts/build.sh made, then zip
# it for a GitHub release: build/Heraldo-<version>-macos.zip.
# Needs a Developer ID build (Config/Local.xcconfig) and notarytool credentials,
# either a Keychain profile stored once:
#
#   xcrun notarytool store-credentials <profile> --key AuthKey.p8 --key-id <KEY_ID> --issuer <ISSUER_ID>
#   NOTARY_PROFILE=<profile> apple/scripts/notarize.sh
#
# or, with no NOTARY_PROFILE, the App Store Connect API key in Config/Local.env
# (ASC_KEY_ID, ASC_ISSUER_CMD, ASC_KEY_CMD), which needs no Keychain access.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap '/bin/rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
if [ -n "${NOTARY_PROFILE:-}" ]; then
  set -- --keychain-profile "$NOTARY_PROFILE"
else
  [ -f "$here/Config/Local.env" ] ||
    { echo "notarize: set NOTARY_PROFILE, or fill in Config/Local.env (see Local.example.env)" >&2; exit 1; }
  . "$here/Config/Local.env"
  : "${ASC_KEY_ID:?set ASC_KEY_ID in Config/Local.env}"
  : "${ASC_ISSUER_CMD:?set ASC_ISSUER_CMD in Config/Local.env}"
  : "${ASC_KEY_CMD:?set ASC_KEY_CMD in Config/Local.env}"
  key="$tmp/AuthKey_$ASC_KEY_ID.p8"
  (umask 077 && sh -c "$ASC_KEY_CMD" >"$key") || { echo "notarize: ASC_KEY_CMD failed" >&2; exit 1; }
  issuer=$(sh -c "$ASC_ISSUER_CMD") || { echo "notarize: ASC_ISSUER_CMD failed" >&2; exit 1; }
  [ -s "$key" ] && [ -n "$issuer" ] || { echo "notarize: empty API key or issuer ID" >&2; exit 1; }
  set -- --key "$key" --key-id "$ASC_KEY_ID" --issuer "$issuer"
fi
app=$(find "$here/build/Build/Products/Release" -maxdepth 1 -name '*.app' | head -1)
[ -n "$app" ] || { echo "no Release build; run scripts/build.sh first" >&2; exit 1; }
zip="$here/build/notarize.zip"
ditto -c -k --keepParent "$app" "$zip"
xcrun notarytool submit "$zip" "$@" --wait
rm -f "$zip"
xcrun stapler staple "$app"
spctl -a -vv -t exec "$app"
version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Contents/Info.plist")
out="$here/build/$(basename "$app" .app)-$version-macos.zip"
/bin/rm -f "$out"
ditto -c -k --keepParent "$app" "$out"
echo "release zip: $out"
