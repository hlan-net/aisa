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
AISA="${AISA:-http://127.0.0.1:8084}"
COMPOSE=(docker compose -f "$(dirname "$0")/compose.yaml")

pass=0
body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
ok() { pass=$((pass + 1)); echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }

# expect <description> <expected> <actual>
expect() {
    if [[ "$2" = "$3" ]]; then ok "$1"; else fail "$1: want '$2', got '$3'"; fi
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
    [[ "$code" != "000" ]] && [[ "$code" != "404" ]] && break
    sleep 1
done

curl -fsS -X DELETE "$STUB/debug/requests" >/dev/null

echo "== sources of truth"
expect "vault: consumer chat-ui seeded" "interactive" \
    "$(curl -fsS -H 'X-Vault-Token: dev-root' "$VAULT/v1/secret/data/aisa/consumers/chat-ui" | jq -r .data.data.quota_profile)"
expect "consul: mock-local fails open" "open" \
    "$(curl -fsS "$CONSUL/v1/catalog/service/aisa-backend" | jq -r '.[] | select(.ServiceID == "mock-local") | .ServiceMeta.fail_policy')"
expect "consul: pricing for cloud-large" "0.015" \
    "$(curl -fsS "$CONSUL/v1/kv/aisa/pricing/cloud-large?raw" | jq -r .output_per_1k)"
expect "consul: two healthy aisa-backend instances" "2" \
    "$(curl -fsS "$CONSUL/v1/health/service/aisa-backend?passing" | jq length)"
expect "redis: ping" "PONG" "$("${COMPOSE[@]}" exec -T redis redis-cli ping | tr -d '\r')"

echo "== aisa: /v1/decide with the consumers in Vault"
for _ in $(seq 1 30); do
    [ "$(curl -s -o /dev/null -w '%{http_code}' "$AISA/readyz")" = 200 ] && break
    sleep 1
done
expect "aisa: ready (consumers loaded)" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$AISA/readyz")"
# decide <key> <json body> [extra curl args...]: prints "<status> <X-Aisa-Consumer> <X-Aisa-Model>"
decide() {
    local key=$1 body=$2
    shift 2
    curl -sS -o /dev/null -D - "$@" -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
        -d "$body" "$AISA/v1/decide" |
        awk -F': ' 'NR == 1 { status = $0; sub(/^HTTP\/[0-9.]+ /, "", status); sub(/ .*/, "", status) }
                    tolower($1) == "x-aisa-consumer" { c = $2 } tolower($1) == "x-aisa-model" { m = $2 }
                    END { gsub(/\r/, "", c); gsub(/\r/, "", m); print status, c, m }'
}
expect "aisa: known key, model from the body" "200 chat-ui qwen3" "$(decide dev-key-chat-ui '{"model":"qwen3"}')"
expect "aisa: model from X-Aisa-Requested-Model" "200 batch-jobs llama3.2" \
    "$(decide dev-key-batch-jobs '{}' -H 'X-Aisa-Requested-Model: llama3.2')"
expect "aisa: unknown key" "401  " "$(decide dev-key-nobody '{"model":"qwen3"}')"
expect "aisa: no model" "400  " "$(decide dev-key-chat-ui '{"messages":[]}')"
expect "aisa: decisions counted" "1" \
    "$(curl -fsS "$AISA/metrics" | awk '$1 == "aisa_decisions_total{consumer=\"chat-ui\",result=\"allow\"}" { print $2 }')"

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
expect "usage event names the consumer" "chat-ui" "$(jq -r '.[0].body.consumer' <<<"$usage")"
expect "no consumer key in the stub's records" "0" \
    "$(curl -fsS "$STUB/debug/requests" | grep -o 'dev-key-' | wc -l | tr -d ' ')"
expect "no consumer key in the stub's logs" "0" \
    "$("${COMPOSE[@]}" logs stub-aisa | grep -o 'dev-key-' | wc -l | tr -d ' ')"

echo "all $pass checks passed"
