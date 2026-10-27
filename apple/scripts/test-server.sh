#!/bin/sh
# Run the HeraldoKit tests against a local heraldo-server: a fresh build, a
# temp database and throwaway test tokens. Without this script the tests
# that need a server skip.
#
#   apple/scripts/test-server.sh [swift test arguments]
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
repo=$(cd "$here/.." && pwd)
tmp=$(mktemp -d)
pid=
cleanup() {
  [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  /bin/rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 1' INT TERM HUP

(cd "$repo" && go build -o "$tmp/heraldo-server" ./cmd/heraldo-server)
read_token=$(openssl rand -hex 32)
write_token=$(openssl rand -hex 32)
{
  echo "app-read read $(printf %s "$read_token" | "$tmp/heraldo-server" hash-token)"
  echo "app-write write $(printf %s "$write_token" | "$tmp/heraldo-server" hash-token)"
} >"$tmp/tokens"

# A free port from the kernel.
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
HERALDO_ADDR="127.0.0.1:$port" HERALDO_DB="$tmp/heraldo.db" HERALDO_TOKENS_FILE="$tmp/tokens" \
  "$tmp/heraldo-server" >"$tmp/server.log" 2>&1 &
pid=$!
url="http://127.0.0.1:$port"
i=0
until curl -fsS "$url/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -gt 100 ]; then
    cat "$tmp/server.log" >&2
    echo "test-server: heraldo-server did not start" >&2
    exit 1
  fi
  /bin/sleep 0.1
done

cd "$here/HeraldoKit"
HERALDO_TEST_URL="$url" HERALDO_TEST_READ_TOKEN="$read_token" HERALDO_TEST_WRITE_TOKEN="$write_token" \
  swift test "$@"
