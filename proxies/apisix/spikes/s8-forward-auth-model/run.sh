#!/usr/bin/env bash
# Spike S8: runs the variants in apisix.yaml against the dev stack and prints what happened.
#
#   ./proxies/apisix/spikes/s8-forward-auth-model/run.sh
#
# Starts the dev stack with this spike's APISIX config, runs the checks and leaves the stack up
# for inspection (docker compose -f dev/compose.yaml down -v to stop). Needs curl and jq.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
export APISIX_CONFIG=$here/config.yaml APISIX_ROUTES=$here/apisix.yaml
COMPOSE=(docker compose -f "$repo/dev/compose.yaml")
GATEWAY=http://127.0.0.1:9080
STUB=http://127.0.0.1:8081

"${COMPOSE[@]}" up -d --build --wait >/dev/null
# The route file is loaded asynchronously after the port opens.
# 000: not listening yet, 404: routes not loaded yet.
for i in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/a/v1/chat/completions" || true)
    [[ "$code" != "000" ]] && [[ "$code" != "404" ]] && break
    [[ "$i" -eq 60 ]] && { echo "gateway not ready (last status $code)" >&2; exit 1; }
    sleep 1
done

body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT
failures=0
failed=()
# The unsafe or impossible variants: these checks document a failure and must fail.
expected_failures=(
    "route matching sees forward-auth's header"
    "unknown key + client X-Aisa-Model: cloud-large is not served"
)

# call <variant> <key> <json> [curl args...] → sets STATUS, BODY
call() {
    local v=$1 key=$2 json=$3
    shift 3
    STATUS=$(curl -sS -o "$body_file" -w '%{http_code}' "$@" \
        -H "Authorization: Bearer $key" -H 'Content-Type: application/json' \
        -d "$json" "$GATEWAY/$v/v1/chat/completions")
    BODY=$(cat "$body_file")
}
served_by() { jq -r '"\(.system_fingerprint // "-") \(.model // "-")"' <<<"$BODY" 2>/dev/null || echo "- -"; }
last_decide() { curl -fsS "$STUB/debug/requests?kind=decide" | jq -c '.[-1] | {model, model_source, return_model, status, body_bytes: (.body | tostring | length)}'; }

# check <description> <want> <got>
check() {
    if [[ "$2" = "$3" ]]; then
        printf '  ok    %-58s %s\n' "$1" "$3"
    else
        printf '  FAIL  %-58s want %s, got %s\n' "$1" "$2" "$3"
        failures=$((failures + 1))
        failed+=("$1")
    fi
}

hi='{"model":"qwen3","messages":[{"role":"user","content":"hi there"}]}'
cloud='{"model":"cloud-large","messages":[{"role":"user","content":"hi there"}]}'
llama='{"model":"llama3.2","messages":[{"role":"user","content":"hi there"}]}'

echo "== Part 1: does the model reach /v1/decide?"
for v in a b; do
    echo "variant $v"
    call "$v" dev-key-chat-ui "$hi"
    check "request succeeds" 200 "$STATUS"
    d=$(last_decide)
    check "decide saw the model" qwen3 "$(jq -r .model <<<"$d")"
    check "model source" "$([[ "$v" = a ]] && echo body || echo header)" "$(jq -r .model_source <<<"$d")"
    echo "        decide received: $d"
    # A client-supplied X-Aisa-Requested-Model must not win over the body.
    call "$v" dev-key-chat-ui "$hi" -H 'X-Aisa-Requested-Model: cloud-large'
    check "spoofed X-Aisa-Requested-Model ignored" qwen3 "$(last_decide | jq -r .model)"
done

echo
echo "== Part 2: can the route pick the backend from X-Aisa-Model?"
echo "variant c (route vars)"
call c dev-key-chat-ui "$hi"
check "route matching sees forward-auth's header" 200 "$STATUS"

for v in d e; do
    echo "variant $v"
    call "$v" dev-key-chat-ui "$hi"
    check "qwen3 → mock-local" "mock-local qwen3" "$(served_by)"
    call "$v" dev-key-chat-ui "$cloud"
    check "cloud-large → mock-cloud" "mock-cloud cloud-large" "$(served_by)"
    call "$v" dev-key-batch-jobs "$cloud"
    check "downgrade: batch-jobs cloud-large → mock-local qwen3" "mock-local qwen3" "$(served_by)"
    call "$v" dev-key-chat-ui "$hi" -H 'X-Aisa-Model: cloud-large'
    check "spoofed X-Aisa-Model overwritten by forward-auth" "mock-local qwen3" "$(served_by)"
    call "$v" dev-key-chat-ui "$llama"
    check "model without a backend is rejected" "$([[ "$v" = d ]] && echo 404 || echo 400)" "$STATUS"
    call "$v" dev-key-blocked "$hi"
    check "denied consumer still gets 429" 429 "$STATUS"
    if [[ "$v" = d ]]; then
        # Rule 1: a client-supplied X-Aisa-* header must not reach the inner route (seen in its usage event).
        call d dev-key-chat-ui "$hi" -H 'X-Aisa-Budget-Remaining: 999' -H 'X-Aisa-Spoof: yes'
        sleep 2
        leaked=$(curl -fsS "$STUB/debug/requests?kind=usage" |
            jq -r '.[-1].body | if .route_id == "d-inner-qwen3" and .consumer == "chat-ui"
                then [.budget_remaining, .spoof] | map(select(. != null and . != "")) | length
                else "wrong event: \(.route_id)" end')
        check "client X-Aisa-* headers stripped before the inner route" 0 "$leaked"
    fi

    # Streaming: 100 tokens × 5 ms from mock-local; the first byte must arrive long before the end.
    t=$(curl -sS -o "$body_file" -w '%{time_starttransfer} %{time_total}' -N \
        -H 'Authorization: Bearer dev-key-chat-ui' -H 'Content-Type: application/json' \
        -H 'X-Mock-Completion-Tokens: 100' \
        -d '{"model":"qwen3","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}' \
        "$GATEWAY/$v/v1/chat/completions")
    usage=$(grep '^data: {' "$body_file" | sed 's/^data: //' | jq -r 'select(.usage != null) | .usage.completion_tokens')
    check "streaming: usage chunk arrives" 100 "$usage"
    streamed=$(awk '{ print ($1 < $2 / 2) ? "yes" : "no" }' <<<"$t")
    check "streaming: first byte before half of total (not buffered)" yes "$streamed"
    echo "        first byte / total (s): $t"
done

echo "variant d: the internal listener is loopback-only"
# From another container on the same network (seed-consul has curl), port 9081 must be closed.
code=$("${COMPOSE[@]}" exec -T seed-consul curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H 'X-Aisa-Model: cloud-large' -H 'Content-Type: application/json' -d "$cloud" \
    http://apisix:9081/v1/chat/completions || true)
check "apisix:9081 unreachable from the network (000 = refused)" 000 "$code"

sleep 2
echo
echo "== Usage events (http-logger on the routes that call ai-proxy-multi in d and e)"
curl -fsS "$STUB/debug/requests?kind=usage" |
    jq -r '.[] | .body | "  route=\(.route_id) status=\(.status) upstream=\(.upstream // "-") consumer=\(.consumer // "-") model=\(.model // "-")"' | sort | uniq -c
check "no consumer key in the usage events" 0 \
    "$(curl -fsS "$STUB/debug/requests?kind=usage" | grep -o 'dev-key-' | wc -l | tr -d ' ')"

echo
echo "== Latency: 50 sequential non-streaming requests per variant, median and p95 of total time (ms)"
for v in a b d e; do
    for _ in $(seq 1 50); do
        curl -sS -o /dev/null -w '%{time_total}\n' -H 'Authorization: Bearer dev-key-chat-ui' \
            -H 'Content-Type: application/json' -H 'X-Mock-Completion-Tokens: 1' -d "$hi" "$GATEWAY/$v/v1/chat/completions"
    done | sort -n | awk -v v="$v" '{ a[NR] = $1 * 1000 } END { printf "  %s  p50 %.1f  p95 %.1f\n", v, a[int(NR * 0.5)], a[int(NR * 0.95)] }'
done

echo
echo "== Part 3: fail open (allow_degradation) while aisa is unreachable"
# Paused: the name still resolves (as a Kubernetes Service does) but nothing answers.
"${COMPOSE[@]}" pause stub-aisa >/dev/null 2>&1
for v in f1 f2; do
    echo "variant $v"
    call "$v" no-such-key "$hi" -H 'X-Aisa-Model: cloud-large'
    check "unknown key + client X-Aisa-Model: cloud-large is not served" "404" "$STATUS"
    echo "        status $STATUS, served by: $(served_by)"
done
call b no-such-key "$hi"
check "fail closed, forward-auth default: status" 403 "$STATUS"
call d no-such-key "$hi"
check "fail closed, d with status_on_error: 503" 503 "$STATUS"
"${COMPOSE[@]}" unpause stub-aisa >/dev/null 2>&1


echo
# Exit non-zero unless exactly the expected checks failed, so a regression in b, d or e is visible.
if [[ "$(printf '%s\n' "${failed[@]}" | sort)" = "$(printf '%s\n' "${expected_failures[@]}" | sort)" ]]; then
    echo "all checks as expected ($failures expected failures: variants c and f1)"
else
    echo "UNEXPECTED RESULT: failed checks differ from the expected ones (variants c and f1)" >&2
    printf '  failed:   %s\n' "${failed[@]}" >&2
    exit 1
fi
