#!/bin/sh
# Archive Heraldo for iOS and upload it to App Store Connect for TestFlight.
# Needs Config/Local.xcconfig (Apple Distribution and the App Store profiles)
# and Config/Local.env (the API key; see Local.example.env).
#
#   apple/scripts/testflight.sh
#
# The build number is the UTC time to the minute, so every upload is higher
# than the last on any machine. MARKETING_VERSION stays in Names.xcconfig.
#
# After the upload it waits until App Store Connect has processed the build
# (scripts/ascwait, so it needs Go). Exit 0 means the build is valid; a
# rejected build prints Apple's errors and exits 1.
#   TESTFLIGHT_WAIT_TIMEOUT  how long to wait, a Go duration (default 30m)
#   TESTFLIGHT_NO_WAIT=1     stop after the upload; skips the wait
#   TESTFLIGHT_DRY_RUN=1     archive and export a signed IPA to build/ios/export,
#                            upload nothing; needs no API key and no Local.env
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
scheme=Heraldo
die() { echo "testflight: $*" >&2; exit 1; }

[ -f "$here/Config/Local.xcconfig" ] ||
  die "no Config/Local.xcconfig; copy Config/Local.example.xcconfig and set the iOS signing"
grep -Eq '^[[:space:]]*CODE_SIGN_IDENTITY\[sdk=iphoneos\*\]' "$here/Config/Local.xcconfig" ||
  die "Config/Local.xcconfig sets no iOS signing identity; see Local.example.xcconfig step 2"
dry=${TESTFLIGHT_DRY_RUN:-}
tmp=$(mktemp -d)
trap '/bin/rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
if [ "$dry" = 1 ]; then
  TESTFLIGHT_NO_WAIT=1
  destination=export
else
  destination=upload
  [ -f "$here/Config/Local.env" ] ||
    die "no Config/Local.env; copy Config/Local.example.env and fill in the API key"
  . "$here/Config/Local.env"
  : "${ASC_KEY_ID:?set ASC_KEY_ID in Config/Local.env}"
  : "${ASC_ISSUER_CMD:?set ASC_ISSUER_CMD in Config/Local.env}"
  : "${ASC_KEY_CMD:?set ASC_KEY_CMD in Config/Local.env}"
  key="$tmp/AuthKey_$ASC_KEY_ID.p8"
  (umask 077 && sh -c "$ASC_KEY_CMD" >"$key") || die "ASC_KEY_CMD failed"
  chmod 600 "$key"
  issuer=$(sh -c "$ASC_ISSUER_CMD") || die "ASC_ISSUER_CMD failed"
  [ -s "$key" ] && [ -n "$issuer" ] || die "empty API key or issuer ID"
fi

# Build the wait helper now: a failure after the upload would read as a
# rejected build.
wait_timeout=${TESTFLIGHT_WAIT_TIMEOUT:-30m}
if [ "${TESTFLIGHT_NO_WAIT:-}" != 1 ]; then
  printf '%s\n' "$wait_timeout" | grep -Eq '^([0-9]+(\.[0-9]+)?(h|m|s))+$' ||
    die "TESTFLIGHT_WAIT_TIMEOUT=$wait_timeout is not a duration such as 30m or 1h"
  command -v go >/dev/null || die "go not found; install Go or set TESTFLIGHT_NO_WAIT=1"
  (cd "$here/.." && go build -o "$tmp/ascwait" ./apple/scripts/ascwait) ||
    die "could not build scripts/ascwait"
fi

"$here/scripts/generate.sh"
build=$(date -u +%Y%m%d%H%M)
out="$here/build/ios"
archive="$out/$scheme.xcarchive"
mkdir -p "$out"
/bin/rm -rf "$archive" "$out/export"

echo "archiving $scheme, build $build"
if ! xcodebuild -project "$here/Heraldo.xcodeproj" -scheme "$scheme" -configuration Release \
  -destination 'generic/platform=iOS' -derivedDataPath "$out" -archivePath "$archive" \
  CURRENT_PROJECT_VERSION="$build" archive >"$out/archive.log" 2>&1; then
  grep -E 'error' "$out/archive.log" | head -40 >&2
  die "archive failed; full log: $out/archive.log"
fi

# Export options: every signed bundle in the archive with the profile it embeds.
app=$(find "$archive/Products/Applications" -maxdepth 1 -name '*.app' | head -1)
plist="$out/ExportOptions.plist"
{
  cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>method</key><string>app-store-connect</string>
	<key>destination</key><string>$destination</string>
	<key>signingStyle</key><string>manual</string>
	<key>signingCertificate</key><string>Apple Distribution</string>
	<key>uploadSymbols</key><true/>
	<key>manageAppVersionAndBuildNumber</key><false/>
	<key>provisioningProfiles</key>
	<dict>
PLIST
  for bundle in "$app" "$app"/PlugIns/*.appex; do
    [ -f "$bundle/embedded.mobileprovision" ] || continue
    id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$bundle/Info.plist")
    security cms -D -i "$bundle/embedded.mobileprovision" >"$tmp/profile.plist" 2>/dev/null
    name=$(/usr/libexec/PlistBuddy -c 'Print :Name' "$tmp/profile.plist")
    printf '\t\t<key>%s</key><string>%s</string>\n' "$id" "$name"
  done
  printf '\t</dict>\n</dict>\n</plist>\n'
} >"$plist"

if [ "$dry" = 1 ]; then
  echo "dry run: exporting build $build, uploading nothing"
  # /usr/bin first: a Homebrew rsync on PATH breaks the IPA step with "Copy failed".
  if ! PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH" xcodebuild -exportArchive \
    -archivePath "$archive" -exportPath "$out/export" -exportOptionsPlist "$plist" \
    >"$out/export.log" 2>&1; then
    grep -E 'error|Error' "$out/export.log" | head -40 >&2
    die "export failed; full log: $out/export.log"
  fi
  ipa=$(find "$out/export" -maxdepth 1 -name '*.ipa' | head -1)
  [ -n "$ipa" ] || die "export made no IPA; see $out/export.log"
  echo "dry run: signed IPA at $ipa; nothing uploaded"
  exit 0
fi

echo "uploading build $build"
# /usr/bin first: a Homebrew rsync on PATH breaks the IPA step with "Copy failed".
if ! PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH" xcodebuild -exportArchive \
  -archivePath "$archive" -exportPath "$out/export" -exportOptionsPlist "$plist" \
  -authenticationKeyPath "$key" -authenticationKeyID "$ASC_KEY_ID" \
  -authenticationKeyIssuerID "$issuer" >"$out/upload.log" 2>&1; then
  grep -E 'error|Error' "$out/upload.log" | head -40 >&2
  die "upload failed; full log: $out/upload.log"
fi
grep -q 'Upload succeeded' "$out/upload.log" || die "no upload confirmation; see $out/upload.log"
version=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$app/Info.plist")
echo "uploaded $scheme $version ($build); App Store Connect is processing it"

# The transfer succeeded, but App Store Connect can still reject the build
# while processing it (for example ITMS-90626). A rejected build never shows
# in the builds list; ascwait reads its state and errors from the app's
# buildUploads endpoint.
if [ "${TESTFLIGHT_NO_WAIT:-}" = 1 ]; then
  echo "not done until it shows in TestFlight: processing can still reject it; Apple emails the reason"
  exit 0
fi
bundle_id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Info.plist")
echo "waiting up to $wait_timeout for App Store Connect to process build $build"
status=0
"$tmp/ascwait" -key "$key" -key-id "$ASC_KEY_ID" -issuer "$issuer" \
  -bundle-id "$bundle_id" -build "$build" -platform IOS -timeout "$wait_timeout" || status=$?
case $status in
0) echo "$scheme $version ($build) is processed and valid in TestFlight" ;;
1) die "App Store Connect rejected build $build; fix the errors above and upload again" ;;
*) die "build $build: processing state unknown; check TestFlight, Apple emails the reason if it fails" ;;
esac
