#!/bin/sh
# Runs consul-template in the proxy's pod, with what has to run beside it (#97, #98).
#
# Usage: consul-template-run.sh [--guard <target-file>] [--consul-token <token-file>] -- <consul-template args>
#
#   --guard <target-file>        every 30 s, sends the render reports aisa has not taken yet and
#                                rechecks a hold (guard.sh)
#   --consul-token <token-file>  reloads consul-template when the Vault Agent writes it a new
#                                Consul token (consul-token-watch.sh, #90)
#
# This shell stays the container's PID 1 and runs consul-template as its child, instead of
# exec-ing it. PID 1 inherits every process whose parent exits first, and consul-template does
# not reap them: BusyBox's timeout, which guard.sh calls, leaves such a process behind on every
# call, and with consul-template as PID 1 each one stayed a zombie until the container's
# pids.max was reached (#98). The shell's wait reaps them. Being the parent, it also knows
# consul-template's PID ($!), which the token watch needs; the command of a container must not
# name the shell's PID itself, because Kubernetes reduces $$ in it to $ (#97).
#
# The shell exits with consul-template's status. It passes SIGHUP (reload) on, and turns SIGTERM,
# which Kubernetes stops a container with, into SIGINT: consul-template stops on its kill_signal,
# SIGINT by default, and ignores SIGTERM, so a container stopped by SIGTERM used to wait for the
# end of its grace period and be killed.
#
#   CONSUL_TEMPLATE   the binary  /bin/consul-template

GUARD_TARGET=
TOKEN_FILE=
while [ $# -gt 0 ]; do
    case $1 in
        --guard) GUARD_TARGET=$2; shift 2 ;;
        --consul-token) TOKEN_FILE=$2; shift 2 ;;
        --) shift; break ;;
        *) echo "consul-template-run: error: unknown option $1" >&2; exit 2 ;;
    esac
done

if [ -n "$GUARD_TARGET" ]; then
    # No guard of this container's last life can still run, but its lock can remain on the
    # volume, and the PID in it may belong to a new process now.
    rm -rf "$GUARD_TARGET.lock"
    # consul-template runs guard.sh only when the rendered file changes. A hold (a rendered file
    # without model routes, guard.sh step 6) ends on its own because this loop runs the guard
    # again while the mark of the hold exists; it restarts with the container, so a hold
    # survives a restart. The loop also sends the render reports aisa has not taken yet (the
    # first render comes before aisa is up). guard.sh takes a lock, so the loop and
    # consul-template never run it at once.
    while sleep 30; do
        /etc/apisix/aisa/guard.sh --resend "$GUARD_TARGET"
        if [ -f "$GUARD_TARGET.empty-since" ]; then
            GUARD_RECHECK=1 /etc/apisix/aisa/guard.sh "$GUARD_TARGET.staged" "$GUARD_TARGET" ||
                echo "guard: error: the recheck of a hold failed (see above); the next render runs the guard again" >&2
        fi
    done &
fi

# Read by the watch before consul-template starts, so that a token written in between is not
# missed.
[ -z "$TOKEN_FILE" ] || CONSUL_TOKEN_WATCH_START=$(cat "$TOKEN_FILE" 2>/dev/null || true)

"${CONSUL_TEMPLATE:-/bin/consul-template}" "$@" &
ct=$!
trap 'kill -INT "$ct" 2>/dev/null' TERM INT
trap 'kill -HUP "$ct" 2>/dev/null' HUP

if [ -n "$TOKEN_FILE" ]; then
    CONSUL_TOKEN_WATCH_START=$CONSUL_TOKEN_WATCH_START /etc/apisix/aisa/consul-token-watch.sh "$TOKEN_FILE" "$ct" &
fi

# wait returns early when a trapped signal arrives; consul-template is then still running, or a
# zombie that kill -0 still finds, and the next wait collects its status.
status=0
while kill -0 "$ct" 2>/dev/null; do
    wait "$ct"
    status=$?
done
exit "$status"
