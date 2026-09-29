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
JSON='Content-Type: application/json'
QWEN='{"model":"qwen3"}'

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
        -H "Authorization: Bearer $key" -H "$JSON" \
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
    [[ "$(curl -s -o /dev/null -w '%{http_code}' "$AISA/readyz")" == 200 ]] && break
    sleep 1
done
expect "aisa: ready (consumers and quota profiles loaded)" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$AISA/readyz")"
# decide <key> <json body> [extra curl args...]: prints "<status> <X-Aisa-Consumer> <X-Aisa-Model>"
decide() {
    local key=$1 body=$2
    shift 2
    curl -sS -o /dev/null -D - "$@" -H "Authorization: Bearer $key" -H "$JSON" \
        -d "$body" "$AISA/v1/decide" |
        awk -F': ' 'NR == 1 { status = $0; sub(/^HTTP\/[0-9.]+ /, "", status); sub(/ .*/, "", status) }
                    tolower($1) == "x-aisa-consumer" { c = $2 } tolower($1) == "x-aisa-model" { m = $2 }
                    END { gsub(/\r/, "", c); gsub(/\r/, "", m); print status, c, m }'
}
allowed() { curl -fsS "$AISA/metrics" | awk '$1 == "aisa_decisions_total{consumer=\"chat-ui\",result=\"allow\"}" { n = $2 } END { print n + 0 }'; }
allowed_before=$(allowed)
expect "aisa: known key, model from the body" "200 chat-ui qwen3" "$(decide dev-key-chat-ui "$QWEN")"
expect "aisa: model from X-Aisa-Requested-Model" "200 batch-jobs llama3.2" \
    "$(decide dev-key-batch-jobs '{}' -H 'X-Aisa-Requested-Model: llama3.2')"
expect "aisa: unknown key" "401  " "$(decide dev-key-nobody "$QWEN")"
expect "aisa: no model" "400  " "$(decide dev-key-chat-ui '{"messages":[]}')"
expect "aisa: decisions counted" "$((allowed_before + 1))" "$(allowed)"

echo "== aisa: /v1/usage event ingestion and metrics"
send_usage() {
    local body=$1
    curl -sS -o /dev/null -w '%{http_code}\n' -H "$JSON" \
        -d "$body" "$AISA/v1/usage"
}
tokens_metric() {
    local consumer=$1 model=$2 direction=$3
    curl -fsS "$AISA/metrics" | awk -v c="$consumer" -v m="$model" -v d="$direction" '
        $1 ~ "^aisa_tokens_total{" && $1 ~ "consumer=\"" c "\"" && $1 ~ "model=\"" m "\"" && $1 ~ "direction=\"" d "\"" { print $2 + 0 }' | head -n1
}
prompt_before=$(tokens_metric chat-ui qwen3 prompt)
prompt_before=${prompt_before:-0}
smoke_run_id="smoke-$$-$(date +%s)"
req1="${smoke_run_id}-1"
req2="${smoke_run_id}-2"
expect "aisa: usage accepted (single event)" "200" \
    "$(send_usage "{\"request_id\":\"$req1\",\"consumer\":\"chat-ui\",\"model\":\"qwen3\",\"backend\":\"mock-local\",\"status\":200,\"prompt_tokens\":10,\"completion_tokens\":25,\"latency_ms\":150,\"ttft_ms\":50,\"stream\":false}")"
expect "aisa: token metrics incremented" "$((prompt_before + 10))" "$(tokens_metric chat-ui qwen3 prompt)"

expect "aisa: duplicate usage accepted" "200" \
    "$(send_usage "{\"request_id\":\"$req1\",\"consumer\":\"chat-ui\",\"model\":\"qwen3\",\"backend\":\"mock-local\",\"status\":200,\"prompt_tokens\":10,\"completion_tokens\":25}")"
expect "aisa: duplicate event did not double count tokens" "$((prompt_before + 10))" "$(tokens_metric chat-ui qwen3 prompt)"

expect "aisa: usage accepted (batch array)" "200" \
    "$(send_usage "[{\"request_id\":\"$req2\",\"consumer\":\"chat-ui\",\"model\":\"qwen3\",\"backend\":\"mock-local\",\"status\":\"200\",\"prompt_tokens\":\"5\",\"completion_tokens\":\"15\"}]")"
expect "aisa: batch tokens incremented" "$((prompt_before + 15))" "$(tokens_metric chat-ui qwen3 prompt)"

req3="${smoke_run_id}-3"
expect "aisa: partial batch accepted with invalid event skipped" "200" \
    "$(send_usage "[{\"request_id\":\"$req3\",\"consumer\":\"chat-ui\",\"model\":\"qwen3\",\"backend\":\"mock-local\",\"status\":\"200\",\"prompt_tokens\":\"2\"},{\"request_id\":\"\",\"status\":200}]")"
expect "aisa: partial batch valid tokens incremented" "$((prompt_before + 17))" "$(tokens_metric chat-ui qwen3 prompt)"

echo "== aisa: token quotas"
# A new consumer for each run, so a window left by an earlier run does not matter.
quota_consumer="quota-${smoke_run_id}"
quota_key="dev-key-${quota_consumer}"
curl -fsS -o /dev/null -H 'X-Vault-Token: dev-root' -H "$JSON" \
    -d "{\"data\":{\"key_sha256\":\"$(printf %s "$quota_key" | sha256sum | cut -d' ' -f1)\",\"quota_profile\":\"tiny\"}}" \
    "$VAULT/v1/secret/data/aisa/consumers/$quota_consumer"
# aisa reloads its consumers for an unknown key at most every 5 s (AISA_CONSUMER_MISS_REFRESH),
# and the unknown key above has just used that reload.
for _ in $(seq 1 15); do
    [[ "$(decide "$quota_key" "$QWEN")" == 401* ]] || break
    sleep 1
done
expect "aisa: quota: tokens left" "200 $quota_consumer qwen3" "$(decide "$quota_key" "$QWEN")"
expect "aisa: quota: usage of 120 tokens accepted" "200" \
    "$(send_usage "{\"request_id\":\"${smoke_run_id}-quota\",\"consumer\":\"$quota_consumer\",\"model\":\"qwen3\",\"status\":200,\"prompt_tokens\":60,\"completion_tokens\":60}")"
expect "aisa: quota: exhausted after 120 of 100 tokens" "429  " "$(decide "$quota_key" "$QWEN")"
retry_after=$(curl -sS -o /dev/null -D - -H "Authorization: Bearer $quota_key" -d "$QWEN" "$AISA/v1/decide" |
    awk -F': ' 'tolower($1) == "retry-after" { gsub(/\r/, "", $2); print $2 }')
if [[ "$retry_after" =~ ^[0-9]+$ ]] && ((retry_after >= 1 && retry_after <= 3600)); then
    ok "aisa: quota: Retry-After $retry_after s"
else
    fail "aisa: quota: Retry-After: want 1 to 3600, got '$retry_after'"
fi

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
