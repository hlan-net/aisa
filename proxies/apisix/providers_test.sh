#!/usr/bin/env bash
# Validates paid providers behind aisa with real keys (#144, ROADMAP.md v0.3.x): the Gemini API,
# the OpenAI API and the Claude API, each as an openai-compatible backend. It runs APISIX on the
# config that apisix.yaml.ctmpl renders, against the running dev stack (dev/compose.yaml), with
# the stack's real aisa for the decision, the usage events and the render reports, because the
# ledger and the provider budget are what is validated.
#
#   docker compose -f dev/compose.yaml up -d --build --wait
#   ./proxies/apisix/providers_test.sh                 # every provider that has a key
#   ./proxies/apisix/providers_test.sh openai claude   # only these
#
# A provider's key is a file in .local/ (git-ignored, chmod 600) with the key and nothing else:
#
#   .local/gemini-api-key   .local/openai-api-key   .local/claude-api-key
#
# A provider without its file is skipped; with no file at all the script exits 0. The run costs
# money: at most six billed requests per provider, each with a short prompt and max_tokens 16.
# A key goes to the dev stack's Vault for the run and is deleted at the end. It is never
# printed and never an argument of a command, and the rendered file that holds it is in a
# directory only the owner can enter. One run at a time: the proxy's port is fixed.
#
# Needs curl, jq and docker. Exits non-zero when a provider fails a check; a provider that
# fails does not stop the others.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
KEY_DIR="${KEY_DIR:-$REPO_DIR/.local}"
NETWORK="${NETWORK:-aisa-dev_default}"
CT_IMAGE="${CONSUL_TEMPLATE_IMAGE:-hashicorp/consul-template:0.43.0}"
APISIX_IMAGE="${APISIX_IMAGE:-apache/apisix:3.20.0-debian}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:8-alpine}"
AISA="${AISA:-http://127.0.0.1:8084}"
CONSUL="${CONSUL:-http://127.0.0.1:8500}"
VAULT="${VAULT:-http://127.0.0.1:8200}"
VAULT_DEV_TOKEN="${VAULT_DEV_TOKEN:-dev-root}"
# aisa as the proxy reaches it, inside the dev stack's network.
AISA_INSIDE="${AISA_INSIDE:-http://aisa:8080}"
PORT="${PROXY_PORT:-19081}"
PROXY="http://127.0.0.1:$PORT"
# What this run registers carries the run's own name, so that it replaces and removes nothing
# that was there before it: not another run's leftovers, and not a holder's own "validate".
RUN="$(date +%s)-$$"
CONTAINER="aisa-proxy-providers-test-$RUN"
JSON='Content-Type: application/json'
STATUS='%{http_code}'
# The key of chat-ui, a consumer of the dev stack (dev/seed) with a quota and a budget far
# above what a run uses.
CONSUMER_KEY=dev-key-chat-ui
# The local model a used-up provider budget falls back to: the dev stack's mock backend.
LOCAL_MODEL=qwen3
# aisa reads the catalog every 30 s to know which key a backend's tokens are charged to.
CATALOG_WAIT="${CATALOG_WAIT:-35}"

ALL=(gemini openai claude)
declare -A HOST=([gemini]=generativelanguage.googleapis.com [openai]=api.openai.com [claude]=api.anthropic.com)
declare -A BASE=([gemini]=/v1beta/openai [openai]="" [claude]="")
declare -A MODEL=(
    [gemini]="${GEMINI_MODEL:-gemini-2.5-flash-lite}"
    [openai]="${OPENAI_MODEL:-gpt-4o-mini}"
    [claude]="${CLAUDE_MODEL:-claude-haiku-4-5-20251001}"
)
# What a provider answers to a key that is not one. Most say 401, which the proxy turns into
# 502 (PROXY_CONTRACT.md, Account isolation). Google says 400 INVALID_ARGUMENT, which the proxy
# cannot tell from a request the provider rejects, so the client gets 400 (#145).
declare -A REFUSED_KEY=([gemini]=400 [openai]=502 [claude]=502)
# What a client may get of a provider's answer (PROXY_CONTRACT.md, Account isolation): the
# allowlist, the framing, and the headers of the gateway and of aisa. X-APISIX-Upstream-Status
# is the gateway's: on a 5xx it repeats the status the client gets, not the provider's.
ALLOWED_HEADERS='^(content-type|cache-control|retry-after|content-length|transfer-encoding|connection|date|server|x-apisix-upstream-status|x-aisa-[a-z-]+)$'

# Only the owner can enter the directory: the rendered file in it holds the keys.
OUT=$(mktemp -d)
active=()
declare -A ID
proxy_started=0

# shellcheck disable=SC2329 # invoked by the trap
cleanup() {
    local p
    for p in "${active[@]}"; do
        curl -sS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/${ID[$p]}" || true
        curl -sS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/${ID[$p]}-badkey" || true
        curl -sS -o /dev/null -X DELETE "$CONSUL/v1/kv/aisa/budgets/providers/${ID[$p]}" || true
        # The month's total that aisa persisted for the key, and its counter in Redis.
        curl -sS -o /dev/null -X DELETE "$CONSUL/v1/kv/aisa/state/providers/${ID[$p]}?recurse" || true
        docker run --rm --network "$NETWORK" "$REDIS_IMAGE" sh -c \
            'redis-cli -h redis --scan --pattern "$1" | xargs -r redis-cli -h redis del' sh "aisa:*${ID[$p]}*" >/dev/null 2>&1 || true
        # The model's price as it was before the run, or none.
        if [[ -s "$OUT/price.$p" ]]; then
            curl -sS -o /dev/null -X PUT --data-binary "@$OUT/price.$p" "$CONSUL/v1/kv/aisa/pricing/${MODEL[$p]}" || true
        elif [[ -e "$OUT/price.$p" ]]; then
            curl -sS -o /dev/null -X DELETE "$CONSUL/v1/kv/aisa/pricing/${MODEL[$p]}" || true
        fi
        curl -sS -o /dev/null -X DELETE -H "X-Vault-Token: $VAULT_DEV_TOKEN" "$VAULT/v1/secret/metadata/aisa/providers/${ID[$p]}" || true
        curl -sS -o /dev/null -X DELETE -H "X-Vault-Token: $VAULT_DEV_TOKEN" "$VAULT/v1/secret/metadata/aisa/providers/${ID[$p]}-badkey" || true
    done
    # Only a container this run started.
    if [[ $proxy_started -eq 1 ]]; then docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; fi
    rm -rf "$OUT"
}
trap cleanup EXIT

ok() { echo "ok   $*"; }
fail() {
    echo "FAIL $*" >&2
    exit 1
}
expect() {
    local what=$1 want=$2 got=$3
    if [[ "$got" == "$want" ]]; then ok "$what"; else fail "$what: want '$want', got '$got'"; fi
}

# metric <name> <label="value">...: the sum of the series of aisa's metric that carry every
# label, 0 when there is none.
metric() {
    local name=$1
    shift
    curl -fsS "$AISA/metrics" | awk -v name="$name" -v labels="$*" '
        BEGIN { n = split(labels, want, " ") }
        index($0, name "{") == 1 || $1 == name {
            for (i = 1; i <= n; i++) if (index($0, want[i]) == 0) next
            sum += $NF
        }
        END { printf "%d\n", sum }'
}

# settle <before> <name> <label>...: waits until the metric has grown past <before> and has
# stopped growing, and prints it. http-logger sends a batch a second after the last event.
settle() {
    local before=$1 now last=-1
    shift
    for _ in $(seq 1 20); do
        now=$(metric "$@")
        if [[ "$now" != "$before" && "$now" == "$last" ]]; then
            echo "$now"
            return 0
        fi
        last=$now
        sleep 1
    done
    echo "$now"
}

# decide <model>: asks aisa itself for the decision, which costs nothing; prints the status
# and the model the decision names.
decide() {
    local model=$1
    curl -sS -o /dev/null -D "$OUT/decide.headers" -w "$STATUS" -X POST \
        -H "Authorization: Bearer $CONSUMER_KEY" -H "X-Aisa-Requested-Model: $model" "$AISA/v1/decide"
    echo " $(tr -d '\r' <"$OUT/decide.headers" | awk -F': ' 'tolower($1) == "x-aisa-model" { print $2 }')"
}

# until_decision <model> <want>: waits until aisa's decision for the model is <want>, a
# status and a model as decide prints them.
until_decision() {
    local model=$1 want=$2 got=""
    for _ in $(seq 1 90); do
        got=$(decide "$model")
        [[ "$got" == "$want" ]] && return 0
        sleep 1
    done
    fail "aisa's decision for $model: want '$want', got '$got'"
}

# chat <model> <extra JSON fields> <prefix>: sends one chat request through the proxy and
# prints its status; the response's headers and body go to <prefix>.headers and <prefix>.body.
chat() {
    local model=$1 extra=$2 prefix=$3
    curl -sS -o "$prefix.body" -D "$prefix.headers" -w "$STATUS" --max-time 120 \
        -H "Authorization: Bearer $CONSUMER_KEY" -H "$JSON" \
        -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in three words.\"}]$extra}" \
        "$PROXY/v1/chat/completions"
}

# served <description> <want> <got> <prefix>: checks a status and, when it is another, says
# which error the client got. The error is the gateway's or aisa's own, never a provider's text.
served() {
    local what=$1 want=$2 got=$3 prefix=$4
    [[ "$got" == "$want" ]] || fail "$what: want '$want', got '$got' with the error $(jq -c '.error | {type, code}' "$prefix.body" 2>/dev/null || echo '(no JSON body)')"
    ok "$what"
}

# stray_headers <file>: the names of the response headers that a client must not get.
stray_headers() {
    local file=$1
    tr -d '\r' <"$file" | awk -F': ' 'NR > 1 && NF > 1 { print tolower($1) }' | grep -v -E "$ALLOWED_HEADERS" | sort -u | tr '\n' ' ' | sed 's/ $//' || true
}

# put_kv <key> <json>
put_kv() {
    local key=$1 value=$2
    curl -fsS -o /dev/null -X PUT -d "$value" "$CONSUL/v1/kv/$key"
}

# register <provider>: its key in Vault, its backend in Consul, a price and a provider budget.
register() {
    local p=$1 meta
    # The key travels from its file to Vault on standard input, never as an argument.
    jq -n --rawfile k "$KEY_DIR/$p-api-key" '{data: {api_key: ($k | gsub("\\s"; ""))}}' |
        curl -fsS -o /dev/null -X POST -H "X-Vault-Token: $VAULT_DEV_TOKEN" -d @- "$VAULT/v1/secret/data/aisa/providers/${ID[$p]}"
    curl -fsS -o /dev/null -X POST -H "X-Vault-Token: $VAULT_DEV_TOKEN" -d '{"data": {"api_key": "not-a-key"}}' \
        "$VAULT/v1/secret/data/aisa/providers/${ID[$p]}-badkey"
    meta=$(jq -n --arg m "${MODEL[$p]}" --arg k "${ID[$p]}" --arg path "${BASE[$p]}" \
        '{provider: "openai-compatible", models: $m, key: $k} + (if $path == "" then {} else {path: $path} end)')
    # No check: a service without one counts as passing.
    jq -n --arg id "${ID[$p]}" --arg host "${HOST[$p]}" --argjson meta "$meta" \
        '{ID: $id, Name: "aisa-backend", Address: $host, Port: 443, Meta: $meta}' |
        curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register"
    jq -n --arg id "${ID[$p]}-badkey" --arg host "${HOST[$p]}" --argjson meta "$meta" --arg m "${MODEL[$p]}-badkey" \
        '{ID: $id, Name: "aisa-backend", Address: $host, Port: 443, Meta: ($meta + {models: $m, key: $id})}' |
        curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register"
    # A price above zero makes the model a priced one, which budgets apply to. The price the
    # model had, if any, is kept for cleanup; an empty file says it had none.
    curl -sS -f -o "$OUT/price.$p" "$CONSUL/v1/kv/aisa/pricing/${MODEL[$p]}?raw" 2>/dev/null || : >"$OUT/price.$p"
    put_kv "aisa/pricing/${MODEL[$p]}" '{"input_per_1k": 0.001, "output_per_1k": 0.002, "currency": "USD"}'
    put_kv "aisa/budgets/providers/${ID[$p]}" '{"monthly_tokens": 1000000, "on_exhausted": "reject"}'
}

render() {
    mkdir -p "$OUT/rendered" "$OUT/rendered/apisix.yaml.reports"
    # consul-template runs as a user of its own in the container, so it needs these two open.
    # They are reached from the host only through $OUT, which stays the owner's alone.
    chmod 777 "$OUT/rendered" "$OUT/rendered/apisix.yaml.reports"
    docker run --rm --network "$NETWORK" \
        -e CONSUL_HTTP_ADDR=consul:8500 -e VAULT_ADDR=http://vault:8200 -e VAULT_TOKEN="$VAULT_DEV_TOKEN" \
        -e AISA_DECIDE_URI="$AISA_INSIDE/v1/decide" -e AISA_USAGE_URI="$AISA_INSIDE/v1/usage" \
        -e AISA_RENDER_URI="$AISA_INSIDE/v1/render" \
        -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$OUT/rendered":/rendered \
        "$CT_IMAGE" -config=/etc/apisix/aisa/consul-template.hcl -once >"$OUT/render.log" 2>&1 ||
        fail "consul-template could not render (its log is not shown: it can hold a key)"
    [[ -s "$OUT/rendered/apisix.yaml" ]] || fail "guard.sh promoted nothing"
}

proxy() {
    proxy_started=1
    docker run -d --name "$CONTAINER" --network "$NETWORK" -p "127.0.0.1:$PORT:9080" \
        -v "$SCRIPT_DIR/config.yaml":/usr/local/apisix/conf/config.yaml:ro \
        -v "$OUT/rendered":/rendered:ro \
        "$APISIX_IMAGE" sh -c 'ln -sf /rendered/apisix.yaml /usr/local/apisix/conf/apisix.yaml && exec /docker-entrypoint.sh docker-start' >/dev/null
    local code
    for _ in $(seq 1 30); do
        code=$(curl -s -o /dev/null -w "$STATUS" "$PROXY/v1/models" || true)
        [[ "$code" == 200 ]] && return 0
        sleep 1
    done
    fail "APISIX did not load the rendered routes"
}

# gateway_error <trigger>: the error object the proxy gives a client when the mock backend
# fails with <trigger>. A provider's failure of the same kind must give the same object.
gateway_error() {
    local trigger=$1
    curl -sS -H "Authorization: Bearer $CONSUMER_KEY" -H "$JSON" \
        -d "{\"model\":\"$LOCAL_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$trigger\"}]}" \
        "$PROXY/v1/chat/completions" | jq -cS '.error'
}

# validate <provider>: checks 2 to 9 of #144. Runs in a subshell, so a failed check ends this
# provider only.
validate() {
    local p=$1
    local model=${MODEL[$p]} backend=${ID[$p]} f="$OUT/$p"
    local prompt completion before_p before_c before_b after_p after_c missing status stray spent refused

    echo "== $p: $model"
    until_decision "$model" "200 $model"

    # 2. A chat completion, not streamed.
    before_p=$(metric aisa_tokens_total "model=\"$model\"" 'direction="prompt"')
    before_c=$(metric aisa_tokens_total "model=\"$model\"" 'direction="completion"')
    before_b=$(metric aisa_tokens_total "model=\"$model\"" "backend=\"$backend\"")
    missing=$(metric aisa_usage_missing_total "model=\"$model\"")
    served "2. a chat completion: status" 200 "$(chat "$model" ',"max_tokens":16' "$f.plain")" "$f.plain"
    expect "2. a chat completion: an answer" true "$(jq -r '(.choices[0].message.content // "") | length > 0' "$f.plain.body")"
    prompt=$(jq -r '.usage.prompt_tokens // 0' "$f.plain.body")
    completion=$(jq -r '.usage.completion_tokens // 0' "$f.plain.body")
    [[ "$prompt" -gt 0 && "$completion" -gt 0 ]] || fail "2. a chat completion: no usage in the body: $(jq -c '.usage' "$f.plain.body")"
    ok "2. a chat completion: the provider reports $prompt prompt and $completion completion tokens"

    # 3. The ledger has exactly these.
    after_p=$(settle "$before_p" aisa_tokens_total "model=\"$model\"" 'direction="prompt"')
    after_c=$(metric aisa_tokens_total "model=\"$model\"" 'direction="completion"')
    expect "3. the ledger: prompt tokens" "$prompt" "$((after_p - before_p))"
    expect "3. the ledger: completion tokens" "$completion" "$((after_c - before_c))"
    expect "3. the ledger: charged to the backend" "$((prompt + completion))" \
        "$(($(metric aisa_tokens_total "model=\"$model\"" "backend=\"$backend\"") - before_b))"
    expect "3. the ledger: no request without usage" "$missing" "$(metric aisa_usage_missing_total "model=\"$model\"")"

    # 4. A chat completion, streamed, as a client that asks for nothing more sends it.
    before_p=$after_p
    before_c=$after_c
    served "4. a streamed completion: status" 200 "$(chat "$model" ',"max_tokens":16,"stream":true' "$f.stream")" "$f.stream"
    expect "4. a streamed completion: an event stream" 1 \
        "$(tr -d '\r' <"$f.stream.headers" | grep -i -c '^content-type: text/event-stream' || true)"
    grep '^data: {' "$f.stream.body" | sed 's/^data: //' >"$f.stream.chunks"
    expect "4. a streamed completion: chunks with text" true \
        "$(jq -s '[.[] | .choices[0]?.delta.content // "" | length] | add > 0' "$f.stream.chunks")"
    expect "4. a streamed completion: ends with [DONE]" 1 "$(grep -c '^data: \[DONE\]' "$f.stream.body" || true)"

    # 5. The ledger has what the stream's last usage says.
    prompt=$(jq -s '[.[] | select(.usage != null) | .usage.prompt_tokens // 0] | last // 0' "$f.stream.chunks")
    completion=$(jq -s '[.[] | select(.usage != null) | .usage.completion_tokens // 0] | last // 0' "$f.stream.chunks")
    [[ "$prompt" -gt 0 && "$completion" -gt 0 ]] ||
        fail "5. the stream carries no usage, so the proxy did not ask for it or the provider does not send it"
    ok "5. the stream reports $prompt prompt and $completion completion tokens"
    after_p=$(settle "$before_p" aisa_tokens_total "model=\"$model\"" 'direction="prompt"')
    after_c=$(metric aisa_tokens_total "model=\"$model\"" 'direction="completion"')
    expect "5. the ledger: streamed prompt tokens" "$prompt" "$((after_p - before_p))"
    expect "5. the ledger: streamed completion tokens" "$completion" "$((after_c - before_c))"
    expect "5. the ledger: no request without usage" "$missing" "$(metric aisa_usage_missing_total "model=\"$model\"")"

    # 6. Nothing of the provider's account reaches the client.
    stray=$(stray_headers "$f.plain.headers")
    expect "6. no header of the provider's on a completion" "" "$stray"
    stray=$(stray_headers "$f.stream.headers")
    expect "6. no header of the provider's on a stream" "" "$stray"
    status=$(chat "$model" ',"max_tokens":-1' "$f.rejected")
    served "6. a request the provider rejects: status" 400 "$status" "$f.rejected"
    expect "6. a request the provider rejects: the gateway's own error" "$ERROR_400" "$(jq -cS '.error' "$f.rejected.body")"
    expect "6. no header of the provider's on its error" "" "$(stray_headers "$f.rejected.headers")"

    # 7. A key the provider refuses is the gateway's failure, not the client's.
    until_decision "$model-badkey" "200 $model-badkey"
    refused=${REFUSED_KEY[$p]}
    served "7. a wrong key: status" "$refused" "$(chat "$model-badkey" ',"max_tokens":16' "$f.badkey")" "$f.badkey"
    if [[ "$refused" == 502 ]]; then
        expect "7. a wrong key: the gateway's own error" "$ERROR_401" "$(jq -cS '.error' "$f.badkey.body")"
    else
        expect "7. a wrong key: the gateway's own error" "$ERROR_400" "$(jq -cS '.error' "$f.badkey.body")"
    fi
    expect "7. no header of the provider's on a refused key" "" "$(stray_headers "$f.badkey.headers")"

    # 8. A used-up provider budget sends the request to the local model.
    spent=$(metric aisa_provider_budget_spent "provider=\"$backend\"")
    [[ "$spent" -gt 1 ]] || fail "8. the provider's key was charged $spent tokens, so its budget cannot be used up"
    ok "8. the provider's key is charged $spent tokens"
    put_kv "aisa/budgets/providers/$backend" "{\"monthly_tokens\": 1, \"on_exhausted\": \"downgrade\", \"downgrade_to\": \"$LOCAL_MODEL\"}"
    until_decision "$model" "200 $LOCAL_MODEL"
    before_p=$(metric aisa_tokens_total "model=\"$model\"")
    served "8. a used-up budget, downgrade: status" 200 "$(chat "$model" ',"max_tokens":16' "$f.downgraded")" "$f.downgraded"
    expect "8. a used-up budget, downgrade: answered by the local model" "$LOCAL_MODEL" "$(jq -r '.model' "$f.downgraded.body")"
    sleep 3 # a usage event of the provider's, if there were one, would have arrived
    expect "8. a used-up budget, downgrade: nothing more from the provider" "$before_p" "$(metric aisa_tokens_total "model=\"$model\"")"

    # 9. Or refuses it.
    before_p=$(metric aisa_decisions_total 'result="deny_provider_budget"')
    put_kv "aisa/budgets/providers/$backend" '{"monthly_tokens": 1, "on_exhausted": "reject"}'
    until_decision "$model" "429 "
    served "9. a used-up budget, reject: status" 429 "$(chat "$model" ',"max_tokens":16' "$f.denied")" "$f.denied"
    expect "9. a used-up budget, reject: the error" "insufficient_quota provider_budget_exhausted" \
        "$(jq -r '"\(.error.type) \(.error.code)"' "$f.denied.body")"
    [[ "$(metric aisa_decisions_total 'result="deny_provider_budget"')" -gt "$before_p" ]] ||
        fail "9. aisa_decisions_total{result=\"deny_provider_budget\"} did not grow"
    ok "9. a used-up budget, reject: counted as deny_provider_budget"
}

wanted=("$@")
[[ ${#wanted[@]} -gt 0 ]] || wanted=("${ALL[@]}")
skipped=()
for p in "${wanted[@]}"; do
    [[ -n "${HOST[$p]:-}" ]] || fail "unknown provider '$p': one of ${ALL[*]}"
    ID[$p]="validate-$p-$RUN"
    if [[ -s "$KEY_DIR/$p-api-key" ]]; then active+=("$p"); else skipped+=("$p"); fi
done
if [[ ${#active[@]} -eq 0 ]]; then
    for p in "${skipped[@]}"; do echo "skip $p: no key in $KEY_DIR/$p-api-key"; done
    echo "no provider has a key; nothing validated"
    exit 0
fi

curl -fsS -o /dev/null "$AISA/healthz" || fail "aisa of the dev stack does not answer at $AISA: docker compose -f dev/compose.yaml up -d --build --wait"

echo "== register: ${active[*]}"
for p in "${active[@]}"; do register "$p"; done
registered=$SECONDS
render
proxy
ERROR_400=$(gateway_error mock:error:400)
ERROR_401=$(gateway_error mock:error:401)
[[ "$ERROR_400" != null && "$ERROR_401" != null ]] || fail "the proxy gave no error for the mock backend's failures"
wait_left=$((CATALOG_WAIT - (SECONDS - registered)))
if [[ $wait_left -gt 0 ]]; then
    echo "waiting ${wait_left}s for aisa to read the catalog"
    sleep "$wait_left"
fi

declare -A result
failed=0
for p in "${active[@]}"; do
    if (validate "$p"); then
        result[$p]=passed
    else
        result[$p]=FAILED
        failed=1
    fi
done

echo "== result"
for p in "${wanted[@]}"; do
    if [[ -n "${result[$p]:-}" ]]; then
        echo "${result[$p]} $p (${MODEL[$p]})"
    else
        echo "skipped $p: no key in $KEY_DIR/$p-api-key"
    fi
done
exit "$failed"
