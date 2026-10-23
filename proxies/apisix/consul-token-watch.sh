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
# old one is deleted. It runs until consul-template is gone.
#
#   CONSUL_TOKEN_WATCH_INTERVAL  seconds between two reads of the file  5

TOKEN_FILE=$1
PID=$2
INTERVAL=${CONSUL_TOKEN_WATCH_INTERVAL:-5}

if [ -z "$TOKEN_FILE" ] || [ -z "$PID" ]; then
    echo "usage: consul-token-watch.sh <token-file> <consul-template-pid>" >&2
    exit 2
fi

# The token consul-template starts with, read before it starts. When the file changes between
# the two reads, the next poll sends a reload that changes nothing; it is never missed.
token=$(cat "$TOKEN_FILE" 2>/dev/null || true)

while sleep "$INTERVAL"; do
    kill -0 "$PID" 2>/dev/null || exit 0
    # The Vault Agent replaces the file by renaming a new one over it; an empty or missing file
    # is not a token, and the next read sees the new one.
    new=$(cat "$TOKEN_FILE" 2>/dev/null || true)
    if [ -n "$new" ] && [ "$new" != "$token" ]; then
        token=$new
        echo "consul-token-watch: the Consul token changed, reloading consul-template" >&2
        kill -HUP "$PID" || true
    fi
done
