#!/usr/bin/env bash
# Spike S2 against a real Ollama: compares the token counts Ollama reports natively
# (prompt_eval_count / eval_count) with its OpenAI-compatible endpoint and with the usage events
# APISIX logs, for non-streamed and streamed requests.
#
#   OLLAMA_URL=http://gpu-box.internal:11434 MODEL=qwen3 ./adapters/apisix/spikes/s2-s6-usage-events/ollama.sh
#
# The dev stack must be able to reach Ollama: set GATEWAY_OLLAMA_URL when the containers reach it
# by another address than this machine (default: OLLAMA_URL). Generation is pinned (temperature 0, fixed seed,
# a small token limit) so the runs are comparable. Needs curl and jq. Prints a table; nothing is
# asserted, because counts from a real model are what this spike is meant to find out.
set -euo pipefail

: "${OLLAMA_URL:?set OLLAMA_URL, e.g. http://gpu-box.internal:11434}"
GATEWAY_OLLAMA_URL=${GATEWAY_OLLAMA_URL:-$OLLAMA_URL}
MODEL=${MODEL:-qwen3}
LIMIT=${LIMIT:-32}
PROMPT=${PROMPT:-"List three colours of the rainbow, one per line."}

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
routes=$(mktemp --suffix=.yaml)
body_file=$(mktemp)
trap 'rm -f "$routes" "$body_file"' EXIT

# The spike's routes, with route /u/ pointed at Ollama instead of the mock backend.
sed "s#[a-z]*://mock-local:8080/v1/chat/completions#${GATEWAY_OLLAMA_URL%/}/v1/chat/completions#" \
    "$here/apisix.yaml" >"$routes"
chmod 644 "$routes"
export APISIX_ROUTES=$routes
COMPOSE=(docker compose -f "$repo/dev/compose.yaml" -f "$here/compose.override.yaml")
"${COMPOSE[@]}" up -d --build --wait >/dev/null
until [[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:9080/u/v1/chat/completions)" != "404" ]]; do sleep 1; done

messages=$(jq -cn --arg p "$PROMPT" '[{role: "user", content: $p}]')
opts=$(jq -cn --argjson n "$LIMIT" '{temperature: 0, seed: 1, num_predict: $n}')

echo "Ollama $(curl -fsS "$OLLAMA_URL/api/version" | jq -r .version), model $MODEL, limit $LIMIT tokens"
printf '%-52s %8s %11s\n' "path" "prompt" "completion"
row() { printf '%-52s %8s %11s\n' "$1" "$2" "$3"; }

# 1. Ollama's native API: the reference counts.
r=$(curl -fsS "$OLLAMA_URL/api/chat" -d "$(jq -cn --arg m "$MODEL" --argjson msg "$messages" --argjson o "$opts" \
    '{model: $m, messages: $msg, stream: false, options: $o}')")
row "native /api/chat" "$(jq .prompt_eval_count <<<"$r")" "$(jq .eval_count <<<"$r")"

oai() { # oai <stream> <include_usage>
    jq -cn --arg m "$MODEL" --argjson msg "$messages" --argjson n "$LIMIT" --argjson s "$1" --argjson u "$2" \
        '{model: $m, messages: $msg, max_tokens: $n, temperature: 0, seed: 1, stream: $s}
         + (if $u then {stream_options: {include_usage: true}} else {} end)'
}
stream_usage() { grep '^data: {' "$body_file" | sed 's/^data: //' | jq -rs \
    '[.[] | select(.usage != null) | .usage] | last // {} | "\(.prompt_tokens // "-") \(.completion_tokens // "-")"'; }

# 2. Ollama's OpenAI-compatible endpoint, directly.
r=$(curl -fsS "$OLLAMA_URL/v1/chat/completions" -d "$(oai false false)")
row "direct /v1, non-streaming" "$(jq .usage.prompt_tokens <<<"$r")" "$(jq .usage.completion_tokens <<<"$r")"
curl -fsSN "$OLLAMA_URL/v1/chat/completions" -d "$(oai true true)" >"$body_file"
read -r p c <<<"$(stream_usage)"; row "direct /v1, streaming, include_usage" "$p" "$c"
curl -fsSN "$OLLAMA_URL/v1/chat/completions" -d "$(oai true false)" >"$body_file"
read -r p c <<<"$(stream_usage)"; row "direct /v1, streaming, no include_usage" "$p" "$c"

# 3. Through APISIX: what the usage event says.
via_gateway() { # via_gateway <label> <stream> <include_usage>
    curl -fsS -X DELETE http://127.0.0.1:8081/debug/requests >/dev/null
    curl -fsSN -H 'Authorization: Bearer dev-key-chat-ui' -H 'Content-Type: application/json' \
        -d "$(oai "$2" "$3")" http://127.0.0.1:9080/u/v1/chat/completions >"$body_file"
    local ev=null
    for _ in $(seq 1 20); do
        ev=$(curl -fsS 'http://127.0.0.1:8081/debug/requests?kind=usage' | jq -c '.[-1].body // null')
        [[ "$ev" != null ]] && break
        sleep 0.5
    done
    row "APISIX usage event, $1" "$(jq -r .prompt_tokens <<<"$ev")" "$(jq -r .completion_tokens <<<"$ev")"
}
via_gateway "non-streaming" false false
via_gateway "streaming, include_usage" true true
via_gateway "streaming, no include_usage" true false

echo
echo "Expected: every row equals the native one. A '-' means that path returned no usage;"
echo "a 0 in an APISIX row means the gateway logged no usage (see GAP checks in run.sh)."
