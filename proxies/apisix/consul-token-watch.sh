#!/bin/sh
# Reloads consul-template when the Vault Agent writes it a new Consul token (#90).
#
# Usage: consul-token-watch.sh <token-file> <consul-template-pid>
#
# consul-template reads -consul-token-file once, when it starts, and keeps that token: a new
# token in the file is not used until the process starts again. The Vault Agent writes a new
# token before the lease of the old one ends, and Consul deletes the old one when it does; from
# then on every query of consul-template is answered 403 (ACL not found). Its retries run out
# and it exits, and only the restart of its container picks the new token up.
#
# This script polls the file and sends consul-template SIGHUP when the token changes. A reload
# creates its Consul client again, which reads the file, so the new token is in use before the
# old one is deleted. It runs until consul-template is gone, and says so when it stops.
# consul-template-run.sh starts it with consul-template's PID.
#
#   CONSUL_TOKEN_WATCH_INTERVAL  seconds between two reads of the file  5
#   CONSUL_TOKEN_WATCH_START     the token consul-template started with, when it was read before
#                                consul-template started; read from the file when unset

TOKEN_FILE=$1
PID=$2
INTERVAL=${CONSUL_TOKEN_WATCH_INTERVAL:-5}

# A PID that is not a number is refused loudly: a watch that ends at once without a word is
# how #97 went unnoticed.
case $PID in
    '' | *[!0-9]*)
        echo "consul-token-watch: error: usage: consul-token-watch.sh <token-file> <consul-template-pid>, not '$TOKEN_FILE' '$PID'" >&2
        exit 2 ;;
    *) ;;
esac
if [ -z "$TOKEN_FILE" ] || ! kill -0 "$PID" 2>/dev/null; then
    echo "consul-token-watch: error: no token file given, or no process $PID to reload" >&2
    exit 2
fi

# The token consul-template starts with, read before it starts. When the file changes between
# the two reads, the next poll sends a reload that changes nothing; it is never missed.
if [ "${CONSUL_TOKEN_WATCH_START+set}" = set ]; then
    token=$CONSUL_TOKEN_WATCH_START
else
    token=$(cat "$TOKEN_FILE" 2>/dev/null || true)
fi
echo "consul-token-watch: watching $TOKEN_FILE for consul-template ($PID)" >&2

while sleep "$INTERVAL"; do
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "consul-token-watch: consul-template ($PID) is gone, stopping" >&2
        exit 0
    fi
    # The Vault Agent replaces the file by renaming a new one over it; an empty or missing file
    # is not a token, and the next read sees the new one.
    new=$(cat "$TOKEN_FILE" 2>/dev/null || true)
    if [ -n "$new" ] && [ "$new" != "$token" ]; then
        token=$new
        echo "consul-token-watch: the Consul token changed, reloading consul-template" >&2
        kill -HUP "$PID" || true
    fi
done
