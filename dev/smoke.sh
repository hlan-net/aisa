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
CLOUD_LARGE='{"model":"cloud-large"}'
HTTP_CODE='%{http_code}'

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
    code=$(curl -s -o /dev/null -w "$HTTP_CODE" -X POST "$GATEWAY/v1/chat/completions" || true)
    [[ "$code" != "000" ]] && [[ "$code" != "404" ]] && break
    sleep 1
done

curl -fsS -X DELETE "$STUB/debug/requests" >/dev/null

echo "== sources of truth"
expect "consul: consumer chat-ui declared" "interactive" \
    "$(curl -fsS "$CONSUL/v1/kv/aisa/consumers/chat-ui?raw" | jq -r .quota_profile)"
expect "vault: consumer chat-ui has a key" "1" \
    "$(curl -fsS -H 'X-Vault-Token: dev-root' "$VAULT/v1/secret/data/aisa/consumers/chat-ui" | jq -r '.data.data.key_sha256 | length / 64')"
expect "consul: mock-local fails open" "open" \
    "$(curl -fsS "$CONSUL/v1/catalog/service/aisa-backend" | jq -r '.[] | select(.ServiceID == "mock-local") | .ServiceMeta.fail_policy')"
expect "consul: pricing for cloud-large" "0.015" \
    "$(curl -fsS "$CONSUL/v1/kv/aisa/pricing/cloud-large?raw" | jq -r .output_per_1k)"
for _ in $(seq 1 30); do
    [[ "$(curl -fsS "$CONSUL/v1/health/service/aisa-backend?passing" | jq length)" == "2" ]] && break
    sleep 1
done
expect "consul: two healthy aisa-backend instances" "2" \
    "$(curl -fsS "$CONSUL/v1/health/service/aisa-backend?passing" | jq length)"
expect "redis: ping" "PONG" "$("${COMPOSE[@]}" exec -T redis redis-cli ping | tr -d '\r')"

echo "== aisa: /v1/decide with the consumers declared in Consul and their keys in Vault"
for _ in $(seq 1 30); do
    [[ "$(curl -s -o /dev/null -w "$HTTP_CODE" "$AISA/readyz")" == 200 ]] && break
    sleep 1
done
expect "aisa: ready (consumers, declarations, prices, model names and quota profiles loaded)" "200" "$(curl -s -o /dev/null -w "$HTTP_CODE" "$AISA/readyz")"
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
expect "aisa: a model's alias is decided as the model" "200 chat-ui llama3.2" "$(decide dev-key-chat-ui '{"model":"llama3.2:3b"}')"
expect "aisa: a name that is neither a model nor an alias" "404  " "$(decide dev-key-chat-ui '{"model":"no-such-model"}')"

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

req4="${smoke_run_id}-4"
expect "aisa: an unparseable value rejects only its event" "200" \
    "$(send_usage "[{\"request_id\":\"$req4\",\"consumer\":\"chat-ui\",\"model\":\"qwen3\",\"backend\":\"mock-local\",\"status\":200,\"prompt_tokens\":3},{\"request_id\":\"${req4}-bad\",\"status\":200,\"prompt_tokens\":\"abc\"}]")"
expect "aisa: the batch's valid tokens incremented" "$((prompt_before + 20))" "$(tokens_metric chat-ui qwen3 prompt)"

missing_metric() {
    curl -fsS "$AISA/metrics" | awk '
        $1 ~ "^aisa_usage_missing_total{" && $1 ~ "consumer=\"chat-ui\"" && $1 ~ "model=\"qwen3\"" && $1 ~ "backend=\"mock-local\"" { print $2 + 0 }' | head -n1
}
missing_before=$(missing_metric)
missing_before=${missing_before:-0}
expect "aisa: usage without token counts accepted" "200" \
    "$(send_usage "{\"request_id\":\"${smoke_run_id}-5\",\"consumer\":\"chat-ui\",\"model\":\"qwen3\",\"backend\":\"mock-local\",\"status\":\"200\",\"prompt_tokens\":\"0\",\"completion_tokens\":\"0\",\"stream\":\"true\"}")"
expect "aisa: missing usage counted" "$((missing_before + 1))" "$(missing_metric)"

cost_metric() {
    local consumer=$1 model=$2 currency=$3
    curl -fsS "$AISA/metrics" | awk -v c="$consumer" -v m="$model" -v cur="$currency" '
        $1 ~ "^aisa_cost_total{" && $1 ~ "consumer=\"" c "\"" && $1 ~ "model=\"" m "\"" && $1 ~ "currency=\"" cur "\"" { printf "%.6f", $2 + 0 }' | head -n1
}
cost_before=$(cost_metric chat-ui cloud-large EUR)
cost_before=${cost_before:-0.000000}
# The budgets count the calendar month in aisa's time zone, UTC in the dev stack.
period=$(date -u +%Y-%m)
budget_metric() { # budget_metric <metric> <consumer>: the consumer's gauge for this month
    curl -fsS "$AISA/metrics" | awk -v n="$1" -v c="$2" -v p="$period" '
        $1 ~ "^" n "{" && $1 ~ "consumer=\"" c "\"" && $1 ~ "period=\"" p "\"" { print $2 + 0 }' | head -n1
}
spent_before=$(budget_metric aisa_budget_spent chat-ui)
spent_before=${spent_before:-0}
expect "aisa: usage of a priced model accepted" "200" \
    "$(send_usage "{\"request_id\":\"${smoke_run_id}-cost\",\"consumer\":\"chat-ui\",\"model\":\"cloud-large\",\"backend\":\"mock-cloud\",\"status\":200,\"prompt_tokens\":1000,\"completion_tokens\":2000}")"
expect "aisa: cost metric incremented (0.033 EUR)" \
    "$(awk -v b="$cost_before" 'BEGIN { printf "%.6f", b + 0.033 }')" \
    "$(cost_metric chat-ui cloud-large EUR)"

echo "== aisa: monthly budgets"
expect "aisa: chat-ui's budget from Consul (aisa_budget_limit)" "20000000" "$(budget_metric aisa_budget_limit chat-ui)"
expect "aisa: the priced tokens count towards the budget, the free ones above did not" \
    "$((spent_before + 3000))" "$(budget_metric aisa_budget_spent chat-ui)"
# decide_remaining <key> <json body>: the X-Aisa-Budget-Remaining of the decision
decide_remaining() {
    curl -sS -o /dev/null -D - -H "Authorization: Bearer $1" -H "$JSON" -d "$2" "$AISA/v1/decide" |
        tr -d '\r' | awk -F': ' 'tolower($1) == "x-aisa-budget-remaining" { print $2 }'
}
expect "aisa: the decision says what remains of the budget" \
    "$((20000000 - spent_before - 3000))" "$(decide_remaining dev-key-chat-ui "$QWEN")"
expect "aisa: a consumer without a budget gets no such header" "" \
    "$(decide_remaining dev-key-nobody "$QWEN")"
# batch-jobs' budget is turned down to one token: its priced requests are downgraded to qwen3,
# its free ones served, and with on_exhausted reject the priced ones are refused. aisa follows
# aisa/budgets/ with a blocking query, so a change applies within seconds.
batch_budget=$(curl -fsS "$CONSUL/v1/kv/aisa/budgets/batch-jobs?raw")
budget_put() { curl -fsS -o /dev/null -X PUT -d "$1" "$CONSUL/v1/kv/aisa/budgets/batch-jobs"; }
budget_put '{"monthly_tokens": 1, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}'
expect "aisa: usage of a priced model by batch-jobs accepted" "200" \
    "$(send_usage "{\"request_id\":\"${smoke_run_id}-budget\",\"consumer\":\"batch-jobs\",\"model\":\"cloud-large\",\"backend\":\"mock-cloud\",\"status\":200,\"prompt_tokens\":5,\"completion_tokens\":5}")"
for _ in $(seq 1 30); do
    [[ "$(decide dev-key-batch-jobs "$CLOUD_LARGE")" == "200 batch-jobs qwen3" ]] && break
    sleep 1
done
expect "aisa: a priced model past the budget is downgraded" "200 batch-jobs qwen3" "$(decide dev-key-batch-jobs "$CLOUD_LARGE")"
expect "aisa: a free model is served past the budget" "200 batch-jobs llama3.2" "$(decide dev-key-batch-jobs '{"model":"llama3.2"}')"
expect "aisa: nothing remains of the budget" "0" "$(decide_remaining dev-key-batch-jobs '{"model":"llama3.2"}')"
budget_put '{"monthly_tokens": 1, "on_exhausted": "reject"}'
for _ in $(seq 1 30); do
    [[ "$(decide dev-key-batch-jobs "$CLOUD_LARGE")" == "429  " ]] && break
    sleep 1
done
expect "aisa: a priced model past a reject budget is refused" "429  " "$(decide dev-key-batch-jobs "$CLOUD_LARGE")"
expect "aisa: budget denials counted (at least one)" "1" \
    "$(curl -fsS "$AISA/metrics" | awk '$1 == "aisa_decisions_total{consumer=\"batch-jobs\",result=\"deny_budget\"}" { n = $2 } END { print (n + 0 >= 1) }')"
budget_put "$batch_budget"
for _ in $(seq 1 30); do
    [[ "$(decide dev-key-batch-jobs "$CLOUD_LARGE")" == "200 batch-jobs cloud-large" ]] && break
    sleep 1
done
expect "aisa: the budget restored, the priced model is served again" "200 batch-jobs cloud-large" "$(decide dev-key-batch-jobs "$CLOUD_LARGE")"

echo "== aisa: a budget per provider account"
# A backend with a key, registered for this run on the dev agent (no check: an agent
# registration without one is passing): cloud-large then goes through the key "smoke".
provider_backend="smoke-keyed-${smoke_run_id}"
curl -fsS -o /dev/null -X PUT -H "$JSON" -d "{\"ID\":\"$provider_backend\",\"Name\":\"aisa-backend\",\"Address\":\"mock-cloud\",\"Port\":8080,\"Meta\":{\"provider\":\"openai-compatible\",\"models\":\"cloud-large\",\"key\":\"smoke\"}}" \
    "$CONSUL/v1/agent/service/register"
curl -fsS -o /dev/null -X PUT -d '{"monthly_tokens": 1}' "$CONSUL/v1/kv/aisa/budgets/providers/smoke"
provider_metric() { # provider_metric <metric>: the key's gauge for this month
    curl -fsS "$AISA/metrics" | awk -v n="$1" -v p="$period" '
        $1 ~ "^" n "{" && $1 ~ "provider=\"smoke\"" && $1 ~ "period=\"" p "\"" { print $2 + 0 }' | head -n1
}
for _ in $(seq 1 30); do
    [[ "$(provider_metric aisa_provider_budget_limit)" == 1 ]] && break
    sleep 1
done
expect "aisa: the provider budget from Consul (aisa_provider_budget_limit)" "1" "$(provider_metric aisa_provider_budget_limit)"
# aisa reads the catalog every 30 s: wait until the key is charged for a request through it.
for _ in $(seq 1 40); do
    send_usage "{\"request_id\":\"${smoke_run_id}-provider-$RANDOM\",\"consumer\":\"chat-ui\",\"model\":\"cloud-large\",\"backend\":\"$provider_backend\",\"status\":200,\"prompt_tokens\":1,\"completion_tokens\":1}" >/dev/null
    [[ "$(provider_metric aisa_provider_budget_spent)" != "" && "$(provider_metric aisa_provider_budget_spent)" != 0 ]] && break
    sleep 1
done
expect "aisa: a request through the key is charged to its budget" "1" \
    "$(awk -v s="$(provider_metric aisa_provider_budget_spent)" 'BEGIN { print (s + 0 >= 2) }')"
expect "aisa: past the provider budget, every consumer's requests to its models are refused" "429  " \
    "$(decide dev-key-chat-ui "$CLOUD_LARGE")"
expect "aisa: a model the key does not serve is unaffected" "200 chat-ui qwen3" "$(decide dev-key-chat-ui "$QWEN")"
curl -fsS -o /dev/null -X DELETE "$CONSUL/v1/kv/aisa/budgets/providers/smoke"
curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/$provider_backend"
for _ in $(seq 1 30); do
    [[ "$(decide dev-key-chat-ui "$CLOUD_LARGE")" == "200 chat-ui cloud-large" ]] && break
    sleep 1
done
expect "aisa: the provider budget removed, the model is served again" "200 chat-ui cloud-large" "$(decide dev-key-chat-ui "$CLOUD_LARGE")"

echo "== aisa: token quotas"
# A new consumer for each run, so a window left by an earlier run does not matter: declared in
# Consul with the tiny profile, its key's hash in Vault.
quota_consumer="quota-${smoke_run_id}"
quota_key="dev-key-${quota_consumer}"
curl -fsS -o /dev/null -X PUT -d '{"quota_profile": "tiny"}' "$CONSUL/v1/kv/aisa/consumers/$quota_consumer"
curl -fsS -o /dev/null -H 'X-Vault-Token: dev-root' -H "$JSON" \
    -d "{\"data\":{\"key_sha256\":\"$(printf %s "$quota_key" | sha256sum | cut -d' ' -f1)\"}}" \
    "$VAULT/v1/secret/data/aisa/consumers/$quota_consumer"
# aisa follows the declarations at once, and reloads its consumers from Vault for an unknown key
# at most every 5 s (AISA_CONSUMER_MISS_REFRESH); the unknown key above has just used that reload.
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

echo "== aisa: admin UI and management API"
expect "admin: /admin redirects to /admin/" "301" \
    "$(curl -s -o /dev/null -w "$HTTP_CODE" "$AISA/admin")"
# The page is read whole before grep: with pipefail, grep -q exiting at the first match can make
# curl fail on the closed pipe.
admin_html="$(curl -fsS "$AISA/admin/" || true)"
if grep -q "aisa // Administration" <<<"$admin_html"; then
    ok "admin: /admin/ serves the administration dashboard HTML"
else
    fail "admin: /admin/ did not contain dashboard title"
fi
expect "admin: overview returns ok status" "ok" \
    "$(curl -fsS "$AISA/admin/api/overview" | jq -r .status)"
expect "admin: consumers list contains chat-ui" "chat-ui" \
    "$(curl -fsS "$AISA/admin/api/consumers" | jq -r '.[] | select(.name == "chat-ui") | .name')"
expect "admin: quotas list contains interactive" "interactive" \
    "$(curl -fsS "$AISA/admin/api/quotas" | jq -r '.[] | select(.name == "interactive") | .name')"
expect "admin: backends list contains mock-local" "mock-local" \
    "$(curl -fsS "$AISA/admin/api/backends" | jq -r '.[] | select(.id == "mock-local") | .id')"
expect "admin: models list contains qwen3" "qwen3" \
    "$(curl -fsS "$AISA/admin/api/models" | jq -r '.[] | select(.name == "qwen3") | .name')"
expect "admin: the image carries the documentation" "Concept: Architecture" \
    "$(curl -fsS "$AISA/admin/api/docs/concepts/ARCHITECTURE.md" | jq -r .title)"

admin_consumer="admin-user-${smoke_run_id}"
create_resp=$(curl -fsS -X POST -H "$JSON" \
    -d "{\"name\":\"$admin_consumer\",\"quota_profile\":\"interactive\",\"generate_key\":true}" \
    "$AISA/admin/api/consumers")
admin_key=$(jq -r .plaintext_key <<<"$create_resp")
if [[ "$admin_key" =~ ^aisa-[0-9a-f]{64}$ ]]; then
    ok "admin: created consumer key format valid"
else
    fail "admin: created consumer key invalid: '$admin_key'"
fi

for _ in $(seq 1 15); do
    [[ "$(decide "$admin_key" "$QWEN")" == 401* ]] || break
    sleep 1
done
expect "admin: newly created consumer allowed in decide" "200 $admin_consumer qwen3" "$(decide "$admin_key" "$QWEN")"

rotate_resp=$(curl -fsS -X POST -H "$JSON" "$AISA/admin/api/consumers/$admin_consumer/rotate-key")
rotated_key=$(jq -r .plaintext_key <<<"$rotate_resp")
if [[ "$rotated_key" =~ ^aisa-[0-9a-f]{64}$ ]]; then
    ok "admin: rotated consumer key format valid"
else
    fail "admin: rotated consumer key invalid: '$rotated_key'"
fi

for _ in $(seq 1 15); do
    [[ "$(decide "$rotated_key" "$QWEN")" == 401* ]] || break
    sleep 1
done
expect "admin: rotated key allowed in decide" "200 $admin_consumer qwen3" "$(decide "$rotated_key" "$QWEN")"

admin_quota="admin-quota-${smoke_run_id}"
curl -fsS -X POST -H "$JSON" \
    -d "{\"name\":\"$admin_quota\",\"tokens_per_hour\":75000}" \
    "$AISA/admin/api/quotas" >/dev/null
expect "admin: created quota profile listed" "$admin_quota" \
    "$(curl -fsS "$AISA/admin/api/quotas" | jq -r ".[] | select(.name == \"$admin_quota\") | .name")"

# A budget set through the admin API applies to the next decisions: a month of 0 tokens sends
# the consumer's priced requests to the downgrade model, and without the budget they pass again.
admin_budget() { # admin_budget <method> [body]: the status of the call on the consumer's budget
    local args=(-X "$1" -H "$JSON")
    if [[ $# -gt 1 ]]; then args+=(-d "$2"); fi
    curl -sS -o /dev/null -w "$HTTP_CODE" "${args[@]}" "$AISA/admin/api/budgets/$admin_consumer"
}
expect "admin: a budget aisa would refuse is not saved" "400" \
    "$(admin_budget PUT '{"monthly_tokens": 0, "on_exhausted": "downgrade"}')"
expect "admin: budget set" "200" \
    "$(admin_budget PUT '{"monthly_tokens": 0, "on_exhausted": "downgrade", "downgrade_to": "qwen3"}')"
expect "admin: the budget is listed with the month's use" "0 $period" \
    "$(curl -fsS "$AISA/admin/api/budgets" | jq -r ".[] | select(.consumer == \"$admin_consumer\") | \"\(.monthly_tokens) \(.usage.period)\"")"
for _ in $(seq 1 30); do
    [[ "$(decide "$rotated_key" "$CLOUD_LARGE")" == "200 $admin_consumer qwen3" ]] && break
    sleep 1
done
expect "admin: the budget set through the API applies to the next decision" "200 $admin_consumer qwen3" \
    "$(decide "$rotated_key" "$CLOUD_LARGE")"
expect "admin: budget removed" "200" "$(admin_budget DELETE)"
for _ in $(seq 1 30); do
    [[ "$(decide "$rotated_key" "$CLOUD_LARGE")" == "200 $admin_consumer cloud-large" ]] && break
    sleep 1
done
expect "admin: without the budget the priced model is served again" "200 $admin_consumer cloud-large" \
    "$(decide "$rotated_key" "$CLOUD_LARGE")"

curl -fsS -X DELETE "$AISA/admin/api/consumers/$admin_consumer" >/dev/null
expect "admin: deleted consumer no longer listed" "" \
    "$(curl -fsS "$AISA/admin/api/consumers" | jq -r ".[] | select(.name == \"$admin_consumer\") | .name")"

curl -fsS -X DELETE "$AISA/admin/api/quotas/$admin_quota" >/dev/null
expect "admin: deleted quota no longer listed" "" \
    "$(curl -fsS "$AISA/admin/api/quotas" | jq -r ".[] | select(.name == \"$admin_quota\") | .name")"

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
