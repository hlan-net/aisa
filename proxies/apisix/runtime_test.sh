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
# The mock backend that serves qwen3: what it received is how a provider sees the gateway.
MOCK_LOCAL="${MOCK_LOCAL:-http://127.0.0.1:8082}"
JSON='Content-Type: application/json'
CHAT_UI='Authorization: Bearer dev-key-chat-ui'
STATUS='%{http_code}'
PORT="${PROXY_PORT:-19080}"
PROXY="http://127.0.0.1:$PORT"
CONTAINER=aisa-proxy-runtime-test
CONSUL="${CONSUL:-http://127.0.0.1:8500}"
# A backend that exists only for this test, with a dollar in its model's name: fault-injection
# resolves $name in a body as an APISIX variable, so the model list must escape it. It is
# deregistered half way, to see the model leave the list with the next render.
DOLLAR_MODEL='vendor/$model'

OUT=$(mktemp -d)
cleanup() {
    curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/runtime-test" || true
    curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/runtime-test-down" || true
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    rm -rf "$OUT"
}
trap cleanup EXIT

pass=0
ok() {
    local what=$*
    pass=$((pass + 1))
    echo "ok   $what"
}
fail() {
    local why=$*
    echo "FAIL $why" >&2
    docker logs "$CONTAINER" 2>&1 | tail -n 20 >&2 || true
    exit 1
}
expect() {
    local what=$1 want=$2 got=$3
    if [[ "$got" == "$want" ]]; then ok "$what"; else fail "$what: want '$want', got '$got'"; fi
}

# render <dir> <decide uri>: renders the template the way a cluster does, with the decision
# sent to <decide uri> and usage to the stub.
render() {
    local dir=$1 decide=$2
    mkdir -p "$dir" "$dir/apisix.yaml.reports"
    chmod 777 "$dir" "$dir/apisix.yaml.reports" # consul-template runs as a user of its own in the container
    docker run --rm --network "$NETWORK" \
        -e CONSUL_HTTP_ADDR=consul:8500 -e VAULT_ADDR=http://vault:8200 -e VAULT_TOKEN=dev-root \
        -e AISA_DECIDE_URI="$decide" -e AISA_USAGE_URI=http://stub-aisa:8080/v1/usage \
        -e AISA_RENDER_URI=http://stub-aisa:8080/v1/render \
        -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$dir":/rendered \
        "$CT_IMAGE" -config=/etc/apisix/aisa/consul-template.hcl -once
    [[ -s "$dir/apisix.yaml" ]] || fail "guard.sh promoted nothing in $dir"
}

# renders: the render reports the stub received since the last clear, as "result backends models".
renders() {
    curl -fsS "$STUB/debug/requests?kind=render" | jq -r '.[].body | "\(.result) \(.backends) \(.models)"'
}

# proxy <dir>: (re)starts APISIX on the config rendered into <dir> and waits for its routes.
proxy() {
    local dir=$1
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    # As in the pod (chart/templates/deployment.yaml): the directory is mounted, and the config
    # path is a link into it, so a file that guard.sh renames over the old one is seen.
    docker run -d --name "$CONTAINER" --network "$NETWORK" -p "127.0.0.1:$PORT:9080" \
        -v "$SCRIPT_DIR/config.yaml":/usr/local/apisix/conf/config.yaml:ro \
        -v "$dir":/rendered:ro \
        "$APISIX_IMAGE" sh -c 'ln -sf /rendered/apisix.yaml /usr/local/apisix/conf/apisix.yaml && exec /docker-entrypoint.sh docker-start' >/dev/null
    for _ in $(seq 1 30); do
        code=$(curl -s -o /dev/null -w "$STATUS" -X POST "$PROXY/v1/chat/completions" || true)
        if [[ "$code" != "000" && "$code" != "404" ]]; then
            sleep 2 # the usage event of this probe arrives before the checks clear the stub
            return 0
        fi
        sleep 1
    done
    fail "APISIX did not load the rendered routes"
}

# chat <key> <model>: sends one request and prints its status.
chat() {
    local key=$1 model=$2
    curl -sS -o /dev/null -w "$STATUS" -H "Authorization: Bearer $key" -H "$JSON" \
        -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"hello there\"}]}" \
        "$PROXY/v1/chat/completions"
}

# models <description> <expected ids>: GET /v1/models answers 200 with these model ids, in this
# order, as JSON, and no usage event reports it.
models() {
    local what=$1 want=$2
    curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
    local body
    body=$(curl -sS -w '\n%{http_code} %{content_type}' "$PROXY/v1/models")
    expect "$what: status and content type" "200 application/json" "$(tail -n1 <<<"$body")"
    expect "$what: the models" "$want" "$(head -n -1 <<<"$body" | jq -r '[.data[] | select(.object == "model") | .id] | join(" ")')"
    sleep 2
    expect "$what: no usage event" "0" "$(events | grep -c . || true)"
}

# events: the usage events the stub received since the last clear, one JSON object per line.
events() {
    curl -fsS "$STUB/debug/requests?kind=usage" | jq -c '.[].body | if type == "array" then .[] else . end'
}

# outcomes: the status and consumer of each usage event since the last clear, one per line.
OUTCOME='"\(.status) \(.consumer // "")"'
outcomes() {
    events | jq -r "$OUTCOME"
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
    expect "$what: event status and consumer" "$status $consumer" "$(jq -r "$OUTCOME" <<<"$got")"
    if [[ "$status" == 200 ]]; then
        # Served: the event is the internal hop's, with the backend's token counts.
        expect "$what: event has the token counts" "true" "$(jq -r '(.prompt_tokens | tonumber) > 0' <<<"$got")"
    fi
}

# isolated <description> <message> <status> <type>: sends a request that carries what a client
# could use to reach into the provider's account, with <message> as its content, and checks
# that the client gets <status> and an error of <type> (none: a completion), no account header
# and no text of the provider's, and that the provider got none of the client's headers
# (PROXY_CONTRACT.md, Account isolation).
isolated() {
    local what=$1 message=$2 status=$3 type=$4
    curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
    local got
    got=$(curl -sS -D "$OUT/headers" -o "$OUT/body" -w "$STATUS" \
        -H "$CHAT_UI" -H "$JSON" \
        -H 'OpenAI-Organization: org-client' -H 'OpenAI-Project: proj-client' \
        -H 'X-Custom: from-the-client' -H 'Cookie: session=client' -H 'User-Agent: client-sdk/1.0' \
        -d "{\"model\":\"qwen3\",\"messages\":[{\"role\":\"user\",\"content\":\"$message\"}]}" \
        "$PROXY/v1/chat/completions")
    expect "$what: status" "$status" "$got"
    expect "$what: error type" "$type" "$(jq -r '.error.type // "none"' "$OUT/body")"
    expect "$what: no account header to the client" "" \
        "$(grep -i -E '^(openai-|x-ratelimit-|x-request-id|x-mock-)' "$OUT/headers" | tr -d '\r' || true)"
    expect "$what: no text of the provider's to the client" "0" "$(grep -c -E 'sk-mock|org-mock' "$OUT/body" || true)"
    local sent
    sent=$(curl -fsS "$MOCK_LOCAL/debug/last-request" | jq -r '.headers | keys[] | ascii_downcase')
    expect "$what: none of the client's headers to the provider" "" \
        "$(grep -E '^(openai-|x-custom$|cookie$|authorization$|x-aisa-)' <<<"$sent" || true)"
    expect "$what: not the client's User-Agent" "0" \
        "$(curl -fsS "$MOCK_LOCAL/debug/last-request" | jq -r '.headers["User-Agent"][0] // ""' | grep -c client-sdk || true)"
    sleep 2
    expect "$what: one usage event, with the client's status and consumer" "$status chat-ui" \
        "$(outcomes | tr '\n' ' ' | sed 's/ $//')"
}

curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "runtime-test", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "$DOLLAR_MODEL", "fail_policy": "open"}}
EOF
# A backend that nothing answers on: the gateway's own error, not a provider's.
curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "runtime-test-down", "Name": "aisa-backend", "Address": "mock-local", "Port": 1,
 "Meta": {"provider": "openai-compatible", "models": "down-model"}}
EOF

echo "== aisa reachable"
curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
render "$OUT/reachable" http://stub-aisa:8080/v1/decide
# The first render is promoted unchecked, and reported: the dev backends and the test's.
expect "the first render is reported as promoted" "promoted 4 5" "$(renders)"
proxy "$OUT/reachable"
one "allowed" dev-key-chat-ui qwen3 200 chat-ui
one "denied by a quota" dev-key-blocked qwen3 429 ""
one "unknown key" wrong-key qwen3 401 ""
models "the model list, a dollar in a name included" "cloud-large down-model llama3.2 qwen3 $DOLLAR_MODEL"

echo "== embeddings"
# POST /v1/embeddings takes the same path as a chat: the decision, then the model's route, which
# calls the backend's /v1/embeddings (#34). An embedding has prompt tokens only.
curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
code=$(curl -sS -o "$OUT/body" -w "$STATUS" -H "$CHAT_UI" -H "$JSON" \
    -d '{"model":"qwen3","input":["hello there","general kenobi"]}' "$PROXY/v1/embeddings")
expect "embeddings: status" "200" "$code"
expect "embeddings: one vector per input" "2 list" "$(jq -r '"\(.data | length) \(.object)"' "$OUT/body")"
sleep 2
got=$(events)
expect "embeddings: one usage event" "1" "$(grep -c . <<<"$got" || true)"
expect "embeddings: event status and consumer" "200 chat-ui" "$(jq -r "$OUTCOME" <<<"$got")"
expect "embeddings: the event has the prompt tokens" "4" "$(jq -r '.prompt_tokens' <<<"$got")"
curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
expect "embeddings, unknown key: refused" "401" "$(curl -sS -o /dev/null -w "$STATUS" \
    -H 'Authorization: Bearer wrong-key' -H "$JSON" -d '{"model":"qwen3","input":"hello"}' "$PROXY/v1/embeddings")"

echo "== account isolation"
isolated "a completion" "hello there" 200 none
isolated "the provider refuses the key" "mock:error:401" 502 provider_error
isolated "the provider limits the account" "mock:error:429" 429 provider_rate_limited
isolated "the provider rejects the request" "mock:error:400" 400 invalid_request_error
isolated "the provider fails" "mock:error:500" 500 provider_error
# APISIX reads at most 100 request headers, its own included: a request with more is refused,
# so none can hide behind the others; one with nearly that many passes, and the hop's headers
# on top of them, which take it past 100, do not hide one of the client's from the model route.
many=()
for i in $(seq 1 110); do many+=(-H "X-Filler-$i: $i"); done
curl -fsS -o /dev/null -X DELETE "$STUB/debug/requests"
expect "more than 100 request headers: refused" "431" "$(curl -sS -o /dev/null -w "$STATUS" \
    -H "$CHAT_UI" -H "$JSON" -H 'X-Aisa-Consumer: smuggled' "${many[@]}" \
    -d '{"model":"qwen3","messages":[{"role":"user","content":"hello there"}]}' "$PROXY/v1/chat/completions")"
sleep 2
expect "more than 100 request headers: reported, with no consumer of the client's" "431 " \
    "$(outcomes)"
many=()
for i in $(seq 1 85); do many+=(-H "X-Filler-$i: $i"); done
curl -fsS -o /dev/null -X DELETE "$MOCK_LOCAL/debug/last-request"
expect "nearly 100 request headers: status" "200" "$(curl -sS -o /dev/null -w "$STATUS" \
    -H "$CHAT_UI" -H "$JSON" "${many[@]}" \
    -H 'OpenAI-Organization: org-client' -H 'X-Aisa-Consumer: smuggled' \
    -d '{"model":"qwen3","messages":[{"role":"user","content":"hello there"}]}' "$PROXY/v1/chat/completions")"
expect "nearly 100 request headers: none of them reaches the provider" "" \
    "$(curl -fsS "$MOCK_LOCAL/debug/last-request" | jq -r '.headers | keys[] | ascii_downcase | select(test("^(openai-|x-filler-|x-aisa-)"))')"
sleep 2
expect "nearly 100 request headers: reported under the consumer aisa named" "200 chat-ui" \
    "$(outcomes | tail -n1)"

# A stream passes the response filters whole: every model route's response goes through them.
curl -sS -D "$OUT/headers" -o "$OUT/body" -H "$CHAT_UI" -H "$JSON" \
    -d '{"model":"qwen3","stream":true,"messages":[{"role":"user","content":"hello there"}]}' "$PROXY/v1/chat/completions"
expect "a stream: event stream" "text/event-stream" "$(grep -i '^content-type:' "$OUT/headers" | tr -d '\r' | awk '{print $2}' | cut -d';' -f1)"
expect "a stream: ends with [DONE]" "data: [DONE]" "$(grep -v '^$' "$OUT/body" | tail -n1)"
# The gateway cannot reach a provider: its own error, its status kept, and no text of its, which
# can name a backend or an address.
code=$(curl -sS -o "$OUT/body" -w "$STATUS" -H "$CHAT_UI" -H "$JSON" \
    -d '{"model":"down-model","messages":[{"role":"user","content":"hello there"}]}' "$PROXY/v1/chat/completions")
expect "no provider answers: the gateway's error" "500 gateway_error" "$code $(jq -r '.error.type' "$OUT/body")"
expect "no provider answers: no backend or address named" "0" "$(grep -c -E 'mock-local|refused' "$OUT/body" || true)"
logs=$(docker logs "$CONTAINER" 2>&1)
grep -q 'provider error 401, answered as 502 provider_error' <<<"$logs" ||
    fail "the provider's error is not in the gateway's log"
ok "account isolation: what the provider's error was is in the gateway's log, for the holder"
expect "account isolation: no text of the provider's in the gateway's log" "0" "$(grep -c -E 'sk-mock|org-mock' <<<"$logs" || true)"

echo "== a model loses its last backend"
curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/runtime-test"
curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/runtime-test-down"
render "$OUT/reachable-again" http://stub-aisa:8080/v1/decide
proxy "$OUT/reachable-again"
models "the model list after the next render" "cloud-large llama3.2 qwen3"

echo "== guard.sh against APISIX"
# guard <dir>: runs guard.sh on <dir>/apisix.yaml.staged in the consul-template image, with
# APISIX's Control API on its localhost, as in the pod.
guard() {
    local dir=$1
    docker run --rm --network "container:$CONTAINER" --entrypoint sh \
        -e AISA_RENDER_URI=http://stub-aisa:8080/v1/render \
        -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$dir":/rendered \
        "$CT_IMAGE" /etc/apisix/aisa/guard.sh /rendered/apisix.yaml.staged /rendered/apisix.yaml
}
# The directory APISIX runs on now: the one of the last render above.
dir="$OUT/reachable-again"
cp "$dir/apisix.yaml" "$OUT/previous.yaml"
# A model route that APISIX's schema rejects: a provider ai-proxy-multi does not know.
awk '/^  - id: "model-qwen3"$/ { f = 1 } /^  - id: / && !/"model-qwen3"/ { f = 0 }
     f && /provider: "openai-compatible"/ { sub(/openai-compatible/, "no-such-provider") } { print }' \
    "$dir/apisix.yaml" > "$dir/apisix.yaml.staged"
grep -q no-such-provider "$dir/apisix.yaml.staged" || fail "the test could not break the route of qwen3"
chmod 666 "$dir/apisix.yaml.staged"
sleep 1 # a modification time APISIX has not seen
if out=$(guard "$dir" 2>&1); then
    fail "guard.sh promoted a file whose route APISIX left out: $out"
fi
grep -q 'model-qwen3' <<<"$out" || fail "guard.sh did not name the route APISIX left out: $out"
ok "guard.sh: a route APISIX rejects is found, and the file is not kept"
cmp -s "$dir/apisix.yaml" "$OUT/previous.yaml" || fail "guard.sh did not put the previous file back"
ok "guard.sh: the previous file is back"
expect "guard.sh: the rejected render is reported" "rejected 2 3" "$(renders | tail -n1)"
sleep 2 # APISIX loads the restored file
one "served after the rejected file" dev-key-chat-ui qwen3 200 chat-ui
cp "$OUT/previous.yaml" "$dir/apisix.yaml.staged"
chmod 666 "$dir/apisix.yaml.staged"
sleep 1
out=$(guard "$dir" 2>&1) || fail "guard.sh rejected a file APISIX loads: $out"
ok "guard.sh: a file APISIX loads in full is kept"
expect "guard.sh: the promoted render is reported" "promoted 2 3" "$(renders | tail -n1)"

echo "== aisa unreachable"
# Nothing listens on this port: forward-auth fails and degrades to the internal hop.
render "$OUT/unreachable" http://stub-aisa:1/v1/decide
proxy "$OUT/unreachable"
one "a model that fails open" any-key qwen3 200 ""
one "a model that fails closed" any-key cloud-large 503 ""
models "the model list while aisa is unreachable" "cloud-large llama3.2 qwen3"

echo "all $pass runtime checks passed"
