#!/usr/bin/env bash
# End-to-end smoke test for the dev stack. Start the stack first:
#
#   docker compose -f dev/compose.yaml up -d --build --wait
#   ./dev/smoke.sh
#
# Needs curl and jq. Exits non-zero on the first failed check.
set -euo pipefail

GATEWAY="${GATEWAY:-http://127.0.0.1:9080}"
STUB="${STUB:-http://127.0.0.1:8081}"
VAULT="${VAULT:-http://127.0.0.1:8200}"
CONSUL="${CONSUL:-http://127.0.0.1:8500}"
COMPOSE=(docker compose -f "$(dirname "$0")/compose.yaml")

pass=0
body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
ok() { pass=$((pass + 1)); echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

# expect <description> <expected> <actual>
expect() {
    if [ "$2" = "$3" ]; then ok "$1"; else fail "$1: want '$2', got '$3'"; fi
}

# chat <key> <json body> [extra curl args...]: prints "<status>\n<body>"
chat() {
    local key=$1 body=$2
    shift 2
    curl -sS -o "$body_file" -w '%{http_code}\n' "$@" \
        -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
        -d "$body" "$GATEWAY/v1/chat/completions"
    cat "$body_file"
}

# Wait until the gateway answers; the healthcheck can pass before the route is loaded.
for _ in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/v1/chat/completions" || true)
    [ "$code" != "000" ] && [ "$code" != "404" ] && break
    sleep 1
done

curl -fsS -X DELETE "$STUB/debug/requests" >/dev/null

echo "== sources of truth"
expect "vault: consumer chat-ui seeded" "open" \
    "$(curl -fsS -H 'X-Vault-Token: dev-root' "$VAULT/v1/secret/data/aisa/consumers/chat-ui" | jq -r .data.data.fail_policy)"
expect "consul: pricing for cloud-large" "0.015" \
    "$(curl -fsS "$CONSUL/v1/kv/aisa/pricing/cloud-large?raw" | jq -r .output_per_1k)"
expect "consul: two healthy aisa-backend instances" "2" \
    "$(curl -fsS "$CONSUL/v1/health/service/aisa-backend?passing" | jq length)"
expect "redis: ping" "PONG" "$("${COMPOSE[@]}" exec -T redis redis-cli ping | tr -d '\r')"

echo "== gateway → decide → backend"
out=$(chat dev-key-chat-ui '{"model":"qwen3","messages":[{"role":"user","content":"hello there"}]}')
expect "non-streaming: status" "200" "$(head -n1 <<<"$out")"
expect "non-streaming: usage" "2 16" "$(tail -n +2 <<<"$out" | jq -r '"\(.usage.prompt_tokens) \(.usage.completion_tokens)"')"
expect "non-streaming: served by" "mock-local" "$(tail -n +2 <<<"$out" | jq -r .system_fingerprint)"

out=$(chat dev-key-chat-ui '{"model":"qwen3","stream":true,"stream_options":{"include_usage":true},"max_tokens":3,"messages":[{"role":"user","content":"one two three"}]}' -N)
expect "streaming: status" "200" "$(head -n1 <<<"$out")"
expect "streaming: ends with [DONE]" "data: [DONE]" "$(grep '^data:' <<<"$out" | tail -n1)"
expect "streaming: usage chunk" "3 3" \
    "$(grep '^data: {' <<<"$out" | sed 's/^data: //' | jq -r 'select(.usage != null) | "\(.usage.prompt_tokens) \(.usage.completion_tokens)"')"

expect "unknown key: 401" "401" "$(chat wrong-key '{"model":"qwen3","messages":[{"role":"user","content":"x"}]}' | head -n1)"
expect "denied consumer: 429" "429" "$(chat dev-key-blocked '{"model":"qwen3","messages":[{"role":"user","content":"x"}]}' | head -n1)"

echo "== stub aisa received"
sleep 2 # http-logger sends asynchronously
decides=$(curl -fsS "$STUB/debug/requests?kind=decide")
expect "decide calls" "4" "$(jq length <<<"$decides")"
expect "decide got the model from the body" "body" "$(jq -r '.[0].model_source' <<<"$decides")"
expect "decide results" "200 200 401 429" "$(jq -r '[.[].status] | join(" ")' <<<"$decides")"
usage=$(curl -fsS "$STUB/debug/requests?kind=usage")
expect "usage events for every request" "4" "$(jq '[.[].events] | add' <<<"$usage")"

echo "all $pass checks passed"
