#!/bin/sh
# Build Heraldo for the iOS Simulator and for macOS, unsigned, and run the
# HeraldoKit tests. The local gate before a push; CI does not build apple/ yet.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
"$here/scripts/generate.sh"
mkdir -p "$here/build"
for dest in 'generic/platform=iOS Simulator' 'generic/platform=macOS'; do
  log="$here/build/check-$(echo "$dest" | tr -cd 'A-Za-z').log"
  if ! xcodebuild -project "$here/Heraldo.xcodeproj" -scheme Heraldo -configuration Debug \
    -derivedDataPath "$here/build/check" -destination "$dest" build >"$log" 2>&1; then
    grep -E 'error' "$log" | head -40 >&2
    echo "build failed for $dest; full log: $log" >&2
    exit 1
  fi
  echo "built: $dest"
done
(cd "$here/HeraldoKit" && swift test)
