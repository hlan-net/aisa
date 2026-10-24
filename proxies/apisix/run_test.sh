#!/usr/bin/env bash
# consul-template-run.sh, as the consul-template container of the chart runs it (#97, #98): its
# shell stays PID 1 and reaps what is orphaned to it, the token watch gets consul-template's PID
# and keeps running, a SIGTERM stops consul-template, and its exit status is the container's.
#
# consul-template runs a command that leaves behind what guard.sh leaves behind: every call of
# BusyBox's timeout in a command substitution orphans a process to PID 1. With consul-template
# exec-ed as PID 1, each one stayed a zombie (#98).
#
# Runs in the consul-template image in Docker; needs docker, nothing else.
#
#   ./proxies/apisix/run_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CT_IMAGE="${CONSUL_TEMPLATE_IMAGE:-hashicorp/consul-template:0.43.0}"
NAME="aisa-run-$$"

WORK=$(mktemp -d)
[[ -n "$WORK" && -d "$WORK" ]] || { echo "FAIL: no work directory" >&2; exit 1; }
cleanup() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT
fail() {
    local msg=$1
    echo "FAIL: $msg" >&2
    docker logs "$NAME" 2>&1 | tail -20 >&2 || true
    exit 1
}
logged() { local text=$1; docker logs "$NAME" 2>&1 | grep -c -- "$text" || true; }
# wait_logged <text>: the container logs it within 30 s.
wait_logged() {
    local text=$1
    for _ in $(seq 1 30); do
        [[ "$(logged "$text")" -gt 0 ]] && return 0
        sleep 1
    done
    fail "not logged after 30 s: $text"
}

mkdir -p "$WORK/etc" "$WORK/vault" "$WORK/out"
cp "$SCRIPT_DIR/consul-template-run.sh" "$SCRIPT_DIR/consul-token-watch.sh" "$WORK/etc/"
# A template of a local file, so that no Consul is needed, and a command of 20 calls of timeout
# as guard.sh makes them.
echo backends >"$WORK/etc/catalog"
printf '{{ file "/etc/apisix/aisa/catalog" }}' >"$WORK/etc/in.ctmpl"
cat >"$WORK/etc/orphans.sh" <<'EOF'
#!/bin/sh
for _ in $(seq 1 20); do x=$(timeout 5 true; echo ok); done
echo "orphans: done"
EOF
# A stand-in for consul-template that exits by itself, with the code of its argument.
# shellcheck disable=SC2016 # expanded by the stand-in
printf '#!/bin/sh\nsleep 2\nexit "$1"\n' >"$WORK/etc/exits"
chmod 755 "$WORK/etc/"*
printf %s first >"$WORK/vault/consul-token"
# consul-template runs as a user of its own in the container.
chmod 777 "$WORK/vault" "$WORK/out"

# Started as the chart starts it (chart/templates/deployment.yaml).
docker run -d --name "$NAME" -e CONSUL_TOKEN_WATCH_INTERVAL=1 \
    -v "$WORK/etc":/etc/apisix/aisa:ro -v "$WORK/vault":/vault -v "$WORK/out":/out \
    --entrypoint /etc/apisix/aisa/consul-template-run.sh "$CT_IMAGE" --consul-token /vault/consul-token -- \
    -template /etc/apisix/aisa/in.ctmpl:/out/catalog:/etc/apisix/aisa/orphans.sh \
    -consul-token-file=/vault/consul-token >/dev/null
wait_logged 'orphans: done'
# BusyBox's timeout leaves its process behind for up to a second after the command ends.
sleep 3
# The state and the name of every process in the container but the one that lists them.
procs=$(docker exec "$NAME" sh -c 'for p in /proc/[0-9]*; do [ "${p#/proc/}" = "$$" ] || cut -d" " -f2,3 "$p/stat"; done' |
    awk '{print $2, $1}')
zombies=$(grep -c '^Z' <<<"$procs" || true)
[[ "$zombies" == 0 ]] || fail "$zombies zombies in the container: $(tr '\n' ' ' <<<"$procs")"
echo "ok: what is orphaned to PID 1 is reaped"

grep -q '^[SR] (consul-token-wa' <<<"$procs" || fail "the token watch is not running: $(tr '\n' ' ' <<<"$procs")"
[[ "$(logged 'consul-token-watch: watching /vault/consul-token for consul-template')" == 1 ]] ||
    fail "the token watch did not start with consul-template's PID"
echo "ok: the token watch runs with consul-template's PID"

# A new token in the file, the way the Vault Agent writes it: renamed over the old one.
printf %s second >"$WORK/vault/consul-token.new" && mv "$WORK/vault/consul-token.new" "$WORK/vault/consul-token"
wait_logged 'Reloading configuration'
[[ "$(logged 'the Consul token changed')" == 1 ]] || fail "one new token, not one reload"
echo "ok: a new token reloads consul-template"

# docker stop sends SIGTERM, as Kubernetes does, and kills after 10 s (137). consul-template
# ignores SIGTERM; on SIGINT, its kill_signal, it stops and exits with 12.
docker stop -t 10 "$NAME" >/dev/null
code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
[[ "$code" == 12 ]] || fail "SIGTERM did not stop consul-template: the container exited with $code"
echo "ok: SIGTERM stops consul-template"

# consul-template exits by itself: the container ends with its status.
docker rm -f "$NAME" >/dev/null
docker run --name "$NAME" -e CONSUL_TEMPLATE=/etc/apisix/aisa/exits -v "$WORK/etc":/etc/apisix/aisa:ro \
    --entrypoint /etc/apisix/aisa/consul-template-run.sh "$CT_IMAGE" -- 14 >/dev/null 2>&1 || true
code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME")
[[ "$code" == 14 ]] || fail "the container exited with $code, not consul-template's 14"
echo "ok: the container ends with consul-template's status"

# The watch refuses what is not a PID, loudly: a $ in place of the PID made it end silently (#97).
if out=$(docker run --rm -v "$WORK/etc":/etc/apisix/aisa:ro --entrypoint /etc/apisix/aisa/consul-token-watch.sh \
    "$CT_IMAGE" /vault/consul-token '$' 2>&1); then
    fail "the token watch accepts \$ as a PID"
fi
grep -q 'consul-token-watch: error' <<<"$out" || fail "the token watch refuses a PID without saying so: $out"
echo "ok: the token watch refuses what is not a PID, and says so"
echo "ok: all run tests passed"
