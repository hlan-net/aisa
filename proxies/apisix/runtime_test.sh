#!/usr/bin/env bash
# Runs APISIX on the config that apisix.yaml.ctmpl renders, against the running dev stack
# (dev/compose.yaml), and checks that every request is reported to the usage sink exactly once:
# whether aisa allows it, denies it, or cannot be reached. The stub aisa of the dev stack stands
# in for aisa; it records the usage events it receives.
#
# Which route reports a request depends on APISIX variables (http-logger filters on
# X-Aisa-Consumer and upstream_addr), so only a running APISIX can show that a request is not
# lost or counted twice.
#
#   docker compose -f dev/compose.yaml up -d --build --wait
#   ./proxies/apisix/runtime_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETWORK="${NETWORK:-aisa-dev_default}"
CT_IMAGE="${CONSUL_TEMPLATE_IMAGE:-hashicorp/consul-template:0.43.0}"
APISIX_IMAGE="${APISIX_IMAGE:-apache/apisix:3.19.0-debian}"
STUB="${STUB:-http://127.0.0.1:8081}"
PORT="${PROXY_PORT:-19080}"
PROXY="http://127.0.0.1:$PORT"
CONTAINER=aisa-proxy-runtime-test

OUT=$(mktemp -d)
cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    rm -rf "$OUT"
}
trap cleanup EXIT

pass=0
ok() { pass=$((pass + 1)); echo "ok   $*"; }
fail() {
    echo "FAIL $*" >&2
    docker logs "$CONTAINER" 2>&1 | tail -n 20 >&2 || true
    exit 1
}
expect() { if [[ "$2" == "$3" ]]; then ok "$1"; else fail "$1: want '$2', got '$3'"; fi; }

# render <dir> <decide uri>: renders the template the way a cluster does, with the decision
# sent to <decide uri> and usage to the stub.
render() {
    mkdir -p "$1"
    chmod 777 "$1" # consul-template runs as a user of its own in the container
    docker run --rm --network "$NETWORK" \
        -e CONSUL_HTTP_ADDR=consul:8500 -e VAULT_ADDR=http://vault:8200 -e VAULT_TOKEN=dev-root \
        -e AISA_DECIDE_URI="$2" -e AISA_USAGE_URI=http://stub-aisa:8080/v1/usage \
        -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$1":/rendered \
        "$CT_IMAGE" -config=/etc/apisix/aisa/consul-template.hcl -once
    [[ -s "$1/apisix.yaml" ]] || fail "guard.sh promoted nothing in $1"
}

# proxy <dir>: (re)starts APISIX on the config rendered into <dir> and waits for its routes.
proxy() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker run -d --name "$CONTAINER" --network "$NETWORK" -p "127.0.0.1:$PORT:9080" \
        -v "$SCRIPT_DIR/config.yaml":/usr/local/apisix/conf/config.yaml:ro \
        -v "$1/apisix.yaml":/usr/local/apisix/conf/apisix.yaml:ro \
        "$APISIX_IMAGE" >/dev/null
    for _ in $(seq 1 30); do
        code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$PROXY/v1/chat/completions" || true)
        [[ "$code" != "000" && "$code" != "404" ]] && return 0
        sleep 1
    done
    fail "APISIX did not load the rendered routes"
}

# chat <key> <model>: sends one request and prints its status.
chat() {
    curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
        -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"hello there\"}]}" \
        "$PROXY/v1/chat/completions"
}

# events: the usage events the stub received since the last clear, one JSON object per line.
events() {
    curl -fsS "$STUB/debug/requests?kind=usage" | jq -c '.[].body | if type == "array" then .[] else . end'
}

# one <description> <key> <model> <status> <consumer>: sends one request and checks its status
# and that exactly one usage event reports it, with that status and consumer.
one() {
    local what=$1 key=$2 model=$3 status=$4 consumer=$5
    curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
    expect "$what: status" "$status" "$(chat "$key" "$model")"
    sleep 2 # http-logger sends a batch after a second without events
    local got
    got=$(events)
    expect "$what: one usage event" "1" "$(grep -c . <<<"$got" || true)"
    expect "$what: event status and consumer" "$status $consumer" "$(jq -r '"\(.status) \(.consumer // "")"' <<<"$got")"
    if [[ "$status" == 200 ]]; then
        # Served: the event is the internal hop's, with the backend's token counts.
        expect "$what: event has the token counts" "true" "$(jq -r '(.prompt_tokens | tonumber) > 0' <<<"$got")"
    fi
}

echo "== aisa reachable"
render "$OUT/reachable" http://stub-aisa:8080/v1/decide
proxy "$OUT/reachable"
one "allowed" dev-key-chat-ui qwen3 200 chat-ui
one "denied by a quota" dev-key-blocked qwen3 429 ""
one "unknown key" wrong-key qwen3 401 ""

echo "== aisa unreachable"
# Nothing listens on this port: forward-auth fails and degrades to the internal hop.
render "$OUT/unreachable" http://stub-aisa:1/v1/decide
proxy "$OUT/unreachable"
one "a model that fails open" any-key qwen3 200 ""
one "a model that fails closed" any-key cloud-large 503 ""

echo "all $pass runtime checks passed"
