#!/usr/bin/env bash
# Spike S2 + S6: sends one request per scenario through the dev stack and checks the usage event
# http-logger delivered for it against the mock backend's deterministic token counts.
#
#   ./adapters/apisix/spikes/s2-s6-usage-events/run.sh
#
# Checks marked GAP assert today's known-wrong behaviour (see SPIKES.md), so a gateway upgrade that
# changes it shows up as a failure. Exits non-zero on any unexpected result. Needs curl and jq.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
export APISIX_ROUTES=$here/apisix.yaml
COMPOSE=(docker compose -f "$repo/dev/compose.yaml" -f "$here/compose.override.yaml")
GATEWAY=http://127.0.0.1:9080
STUB=http://127.0.0.1:8081

"${COMPOSE[@]}" up -d --build --wait >/dev/null
for i in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/u/v1/chat/completions" || true)
    [[ "$code" != "000" ]] && [[ "$code" != "404" ]] && break
    [[ "$i" -eq 60 ]] && { echo "gateway not ready (last status $code)" >&2; exit 1; }
    sleep 1
done

body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
failures=0

# check <description> <want> <got>
check() {
    if [[ "$2" = "$3" ]]; then
        printf '  ok    %-62s %s\n' "$1" "$3"
    else
        printf '  FAIL  %-62s want %s, got %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
    fi
}

# send <route> <key> <json> [curl args...]: one request, then waits for its usage event.
# Sets STATUS, and EVENT to the event (http-logger sends asynchronously, batch size 1).
send() {
    local route=$1 key=$2 json=$3
    shift 3
    curl -fsS -X DELETE "$STUB/debug/requests" >/dev/null
    STATUS=$(curl -sS -N -o "$body_file" -w '%{http_code}' "$@" \
        -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
        -d "$json" "$GATEWAY/$route/v1/chat/completions" || true)
    EVENT=null
    for _ in $(seq 1 20); do
        EVENT=$(curl -fsS "$STUB/debug/requests?kind=usage" | jq -c '.[-1].body // null')
        [[ "$EVENT" != null ]] && break
        sleep 0.5
    done
}
field() { jq -r --arg f "$1" '.[$f] | if . == null then "null" else tostring end' <<<"$EVENT"; }
ftype() { jq -r --arg f "$1" '.[$f] | type' <<<"$EVENT"; }
tokens() { echo "$(field prompt_tokens)/$(field completion_tokens)"; }
# usage chunks the client received in a streamed response
client_usage() { grep '^data: {' "$body_file" | sed 's/^data: //' | jq -rs '[.[] | select(.usage != null) | .usage | "\(.prompt_tokens)/\(.completion_tokens)"] | join(",")'; }

prompt='{"model":"qwen3","messages":[{"role":"user","content":"one two three"}]'   # 3 prompt tokens

echo "== S6: the usage event schema, non-streaming (mock: 3 prompt, 16 completion tokens)"
send u dev-key-chat-ui "$prompt}"
check "status" 200 "$(field status)"
check "prompt/completion tokens" 3/16 "$(tokens)"
check "tokens are JSON numbers" number "$(ftype prompt_tokens)"
check "consumer (from X-Aisa-Consumer)" chat-ui "$(field consumer)"
check "model / requested_model" qwen3/qwen3 "$(field model)/$(field requested_model)"
check "backend (ai-proxy-multi instance name)" mock-local "$(field backend)"
check "stream" false "$(field stream)"
check "ts is ISO 8601" true "$(field ts | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T' && echo true || echo false)"
check "latency_ms and ttft_ms are numbers" number/number "$(ftype latency_ms)/$(ftype ttft_ms)"
check "ttft_ms ≥ the mock's 50 ms TTFT" true "$(jq '.ttft_ms >= 50' <<<"$EVENT")"
decide_id=$(curl -fsS "$STUB/debug/requests?kind=decide" | jq -r '.[-1].headers["X-Request-Id"] // "none"')
check "request_id matches the one /v1/decide received" "$(field request_id)" "$decide_id"

echo
echo "== S2: token counts for streamed responses"
send u dev-key-chat-ui "$prompt"',"stream":true,"stream_options":{"include_usage":true}}'
check "stream, client asks for usage: tokens" 3/16 "$(tokens)"
check "stream: ttft_ms < latency_ms" true "$(jq '.ttft_ms < .latency_ms' <<<"$EVENT")"

send u dev-key-chat-ui "$prompt"',"stream":true}'
check "stream, client does not ask for usage: tokens" 3/16 "$(tokens)"
check "  ... APISIX added include_usage: client still gets a usage chunk" 3/16 "$(client_usage)"

send u dev-key-chat-ui "$prompt"',"stream":true,"max_tokens":3}'
check "stream with max_tokens 3: tokens" 3/3 "$(tokens)"

send n dev-key-chat-ui "$prompt"',"stream":true}'
check "GAP backend sends no streamed usage: tokens logged as" 0/0 "$(tokens)"
check "GAP   ... as strings, not numbers" string "$(ftype completion_tokens)"
check "  ... although the client received tokens" true "$(grep -c '"content":"' "$body_file" | awk '{print ($1 > 1)}' | sed 's/1/true/;s/0/false/')"

send n dev-key-chat-ui "$prompt}"
check "same backend, non-streaming: tokens" 3/16 "$(tokens)"

send u dev-key-chat-ui "$prompt"',"stream":true}' -H 'X-Mock-Completion-Tokens: 100' --max-time 0.3
received=$(grep -c '"content":"' "$body_file" || true)
check "GAP client disconnects mid-stream: tokens logged as" 0/0 "$(tokens)"
check "  ... although the client had received more than 10 tokens" true "$([[ "$received" -gt 10 ]] && echo true || echo false)"
check "  ... status" 200 "$(field status)"

echo
echo "== Events for requests that never reach a backend, or fail there"
send u dev-key-chat-ui '{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}'
check "backend answers 404: status / tokens" "404 0/0" "$(field status) $(tokens)"

send u dev-key-blocked "$prompt}"
check "aisa denies (429): status / tokens / backend" "429 0/0 null" "$(field status) $(tokens) $(field backend)"

send u dev-key-blocked "$prompt}" -H 'X-Aisa-Consumer: forged'
check "denied, client sends X-Aisa-Consumer: consumer" null "$(field consumer)"

echo
if [[ "$failures" -eq 0 ]]; then
    echo "all checks as expected"
else
    echo "$failures unexpected results" >&2
    exit 1
fi
