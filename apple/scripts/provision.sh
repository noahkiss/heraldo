#!/bin/sh
# Save the Mac app's server URL and tokens from the shell. The app saves them
# to its Keychain item, which iCloud Keychain carries to the person's other
# devices, so an agent can set up the Mac and the iPhone with no paste.
#
#   HERALDO_URL=https://heraldo.example.com \
#   HERALDO_READ_TOKEN_CMD='<command that prints the read token>' \
#   HERALDO_WRITE_TOKEN_CMD='<command that prints the write token>' \
#     apple/scripts/provision.sh
#
# Each *_CMD is a shell command that prints one value, such as a password
# manager read. The tokens pass from those commands through a FIFO to the
# app's stdin: never argv, a file, the pasteboard or the log.
#
# The script quits the running app, launches it once with --provision-stdin,
# and waits for the app's one "provision:" log line.
#
# The rules the app keeps (README, "Provision from the shell"):
#   1. Write-only. Nothing reads the credentials back out; the log names the
#      outcome and the server host, never a token.
#   2. Launch-time only, through the flag and stdin. No URL scheme, intent or
#      other entry point reaches it.
#   3. The read token must pass the server check and the write token a scope
#      check that stores nothing. On a failure nothing is saved.
#   4. Settings for a different server are replaced only with HERALDO_REPLACE=1
#      (the app's --replace). New tokens for the same server need nothing.
#   5. A save posts a local notification: "Heraldo settings were set from the
#      command line".
#   6. stdin must be a pipe or FIFO, never a terminal.
#
# HERALDO_APP picks the app (default /Applications/Heraldo.app).
# HERALDO_WRITE_TOKEN_CMD is optional; without it the app saves no write token.
# HERALDO_PROVISION_TIMEOUT is how long to wait for the app's answer, in
# seconds (default 120). The first launch of a newly downloaded copy waits for
# the Gatekeeper "Open" click.
set -eu
die() { echo "provision: $*" >&2; exit 1; }

app=${HERALDO_APP:-/Applications/Heraldo.app}
: "${HERALDO_URL:?set HERALDO_URL to the server URL}"
: "${HERALDO_READ_TOKEN_CMD:?set HERALDO_READ_TOKEN_CMD to a command that prints the read token}"
[ -d "$app" ] || die "no app at $app; set HERALDO_APP"
command -v jq >/dev/null || die "needs jq"
bundle_id=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")

tmp=$(mktemp -d)
trap '/bin/rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM HUP
fifo="$tmp/stdin"
mkfifo -m 600 "$fifo"

# One instance at a time: quit the running app and wait for it to exit.
osascript -e "tell application id \"$bundle_id\" to quit" >/dev/null 2>&1 || true
i=0
while pgrep -f "$app/Contents/MacOS/" >/dev/null; do
  i=$((i + 1))
  [ "$i" -le 50 ] || die "the running app did not quit"
  /bin/sleep 0.2
done

start=$(date '+%Y-%m-%d %H:%M:%S')
# The JSON goes into the FIFO in the background; the write blocks until the
# app opens its stdin. jq reads the tokens from its stdin, not its arguments.
# printf is a shell builtin, so the values never show in a process list.
{
  read_token=$(sh -c "$HERALDO_READ_TOKEN_CMD") || exit 1
  write_token=
  if [ -n "${HERALDO_WRITE_TOKEN_CMD:-}" ]; then write_token=$(sh -c "$HERALDO_WRITE_TOKEN_CMD") || exit 1; fi
  printf '%s\n%s\n' "$read_token" "$write_token"
} | jq -R -n -c --arg url "$HERALDO_URL" \
  '[inputs] as $t | {url: $url, read_token: $t[0], write_token: ($t[1] // "")}' >"$fifo" &
writer=$!
if [ "${HERALDO_REPLACE:-}" = 1 ]; then
  open -n "$app" --stdin "$fifo" --args --provision-stdin --replace
else
  open -n "$app" --stdin "$fifo" --args --provision-stdin
fi
wait "$writer" || die "a token command failed"

# The app logs one "provision:" line with the outcome and no values.
i=0
while :; do
  line=$(/usr/bin/log show --start "$start" --info --style compact \
    --predicate "subsystem == \"$bundle_id\" AND eventMessage BEGINSWITH \"provision:\"" 2>/dev/null |
    grep 'provision:' | tail -1 || true)
  if [ -n "$line" ]; then
    echo "${line#*provision: }" | sed 's/^/provision: /'
    case "$line" in *"provision: saved"*) exit 0 ;; *) exit 1 ;; esac
  fi
  i=$((i + 1))
  [ "$i" -le "${HERALDO_PROVISION_TIMEOUT:-120}" ] || die "no answer from the app; see: log show --last 2m --predicate 'subsystem == \"$bundle_id\"'"
  /bin/sleep 1
done
