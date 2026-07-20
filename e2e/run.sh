#!/usr/bin/env bash
#
# Runs the txnpure e2e tests against a throwaway PostgreSQL cluster.
#
# It locates the PostgreSQL server binaries (PATH, Homebrew, or the Debian/
# Ubuntu /usr/lib/postgresql layout), initdbs a cluster in a temp directory,
# starts the server on a private unix socket (no TCP port, so concurrent runs
# cannot collide), exports TXNPURE_E2E_PG_DSN, runs `go test ./...` in this
# directory, and tears everything down. No Docker required.
#
# Unlike go-txnproof's run.sh there is no server-log configuration: txnpure's
# e2e asserts the client-side checkpoint verdict, not a server-log cross-check.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"

find_pgbin() {
    if command -v initdb >/dev/null 2>&1 && command -v pg_ctl >/dev/null 2>&1; then
        dirname "$(command -v initdb)"
        return 0
    fi
    # Fall back to well-known install locations; unmatched globs stay
    # literal and fail the -x test harmlessly. The last match wins, which
    # is the highest version under each layout's lexical ordering.
    candidates=""
    if command -v brew >/dev/null 2>&1; then
        candidates="$candidates $(brew --prefix)/opt/postgresql*/bin"
    fi
    candidates="$candidates /opt/homebrew/opt/postgresql*/bin /usr/local/opt/postgresql*/bin /usr/lib/postgresql/*/bin"
    best=""
    for d in $candidates; do
        if [ -x "$d/initdb" ] && [ -x "$d/pg_ctl" ]; then
            best="$d"
        fi
    done
    if [ -n "$best" ]; then
        printf '%s\n' "$best"
        return 0
    fi
    return 1
}

if ! pgbin="$(find_pgbin)"; then
    echo "error: PostgreSQL server binaries (initdb, pg_ctl) not found on PATH," >&2
    echo "under \$(brew --prefix)/opt/postgresql*/bin, or /usr/lib/postgresql/*/bin." >&2
    echo "Install PostgreSQL to run the e2e tests." >&2
    exit 1
fi
echo "using PostgreSQL binaries in $pgbin ($("$pgbin/pg_ctl" --version))"

workdir="$(mktemp -d "${TMPDIR:-/tmp}/txnpure-e2e.XXXXXX")"
data="$workdir/data"
logfile="$workdir/server.log"

cleanup() {
    "$pgbin/pg_ctl" -D "$data" -m immediate stop >/dev/null 2>&1 || true
    rm -rf "$workdir"
}
trap cleanup EXIT

"$pgbin/initdb" -D "$data" -U postgres -A trust --no-sync >/dev/null

cat >>"$data/postgresql.conf" <<CONF
listen_addresses = ''
unix_socket_directories = '$workdir'
logging_collector = off
fsync = off
CONF

"$pgbin/pg_ctl" -D "$data" -l "$logfile" -w start >/dev/null

export TXNPURE_E2E_PG_DSN="host=$workdir port=5432 user=postgres dbname=postgres sslmode=disable"

echo "cluster ready in $workdir; running e2e tests"
cd "$here"
go test -count=1 "$@" ./...
