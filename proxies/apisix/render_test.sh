#!/usr/bin/env bash
# Renders apisix.yaml.ctmpl against the running dev stack (dev/compose.yaml) with the same
# consul-template config a cluster uses, and checks what guard.sh promoted. It catches a
# template that does not parse and one that renders without the routes the proxy needs.
# The backends of the dev stack name no provider key, so the rendering of a key from Vault is
# not covered here.
#
#   docker compose -f dev/compose.yaml up -d --build --wait
#   ./proxies/apisix/render_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NETWORK="${NETWORK:-aisa-dev_default}"
IMAGE="${CONSUL_TEMPLATE_IMAGE:-hashicorp/consul-template:0.43.0}"

CONSUL="${CONSUL:-http://127.0.0.1:8500}"

OUT=$(mktemp -d)
# A backend that exists only for this test: model names that once gave a normal and a fail-open
# route the same ID ("qwen3-fail-open" next to the fail-open "qwen3"), one that would give an
# embeddings route the ID of another kind's ("fail-open-qwen3"), and a name too long and
# with characters a route ID cannot have.
LONG_MODEL="example-org/a-model-name-that-is-much-longer-than-a-route-id-may-be:latest"
# A dollar in a name: an APISIX variable to fault-injection, unless the model list escapes it.
DOLLAR_MODEL='vendor/$model'
cleanup() {
    for id in render-test render-test-slow render-test-bad-timeout render-test-path \
        render-test-bad-path-1 render-test-bad-path-2 render-test-bad-path-3; do
        curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/$id" || true
    done
    rm -rf "$OUT"
}
trap cleanup EXIT
curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "render-test", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "qwen3-fail-open,fail-open-qwen3,$LONG_MODEL,$DOLLAR_MODEL", "fail_policy": "open"}}
EOF
# A timeout above the cap of ai-proxy-multi, and one that is not a number: the first is capped,
# the second leaves its backend out (#18).
curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "render-test-slow", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "slow-test", "timeout": "900"}}
EOF
curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "render-test-bad-timeout", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "bad-timeout-test,qwen3", "timeout": "fast"}}
EOF
# A backend whose API is not at /v1 (#91), as Google's OpenAI-compatible endpoint, and paths
# that are not a prefix: no leading '/', a trailing '/', a '..' segment.
curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "render-test-path", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "path-test", "path": "/v1beta/openai"}}
EOF
n=0
for bad in "v1beta/openai" "/v1beta/openai/" "/v1/../admin"; do
    n=$((n + 1))
    curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "render-test-bad-path-$n", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "bad-path-test-$n,qwen3", "path": "$bad"}}
EOF
done
# consul-template runs as a user of its own in the container.
chmod 777 "$OUT"

echo "== render apisix.yaml.ctmpl"
docker run --rm --network "$NETWORK" \
    -e CONSUL_HTTP_ADDR=consul:8500 -e VAULT_ADDR=http://vault:8200 -e VAULT_TOKEN=dev-root \
    -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$OUT":/rendered \
    "$IMAGE" -config=/etc/apisix/aisa/consul-template.hcl -once

RENDERED="$OUT/apisix.yaml"
# The ID of the local model's route, as route and expect_in_route take it.
QWEN3_ROUTE='"model-qwen3"'
fail() {
    local why=$1
    echo "FAIL: $why" >&2
    exit 1
}

[[ -s "$RENDERED" ]] || fail "guard.sh promoted nothing"

expect() {
    local what=$1 pattern=$2
    grep -q -- "$pattern" "$RENDERED" || fail "$what"
    echo "ok: $what"
}
expect "the client-facing route" '^  - id: client$'
expect "the fallback route" '^  - id: no-backend$'
expect "the route for an unreachable aisa" '^  - id: aisa-unreachable$'
expect "the client route reports only requests that end there" 'filter: \[\["aisa_forwarded", "!", "~~", "."\]\]'
expect "a route for the local model" '^  - id: "model-qwen3"$'
expect "a route for the paid model" '^  - id: "model-cloud-large"$'
expect "a fail-open route for the local model" '^  - id: "fail-open-qwen3"$'
expect "the gateway's metrics for every route" '^      prometheus: {}$'
expect "the model list route" '^  - id: models$'
expect "the model list names the local model" '\\"id\\":\\"qwen3\\"'
expect "the model list names the paid model" '\\"id\\":\\"cloud-large\\"'
expect "the model list escapes a dollar in a name" '\\"id\\":\\"vendor/\\\\u0024model\\"'
if grep '^          body: ' "$RENDERED" | grep -q '[$]'; then
    fail "the model list carries a raw dollar, which APISIX would resolve as a variable"
fi
echo "ok: no raw dollar in the model list"

if grep -q '^  - id: "fail-open-cloud-large"$' "$RENDERED"; then
    fail "a paid backend has a fail-open route"
fi
echo "ok: no fail-open route for the paid model"

# The fail-open route of the local model serves only its fail-open backends.
awk '/^  - id: "fail-open-qwen3"$/{f=1;next} /^  - id: /{f=0} f' "$RENDERED" > "$OUT/fail-open"
[[ -s "$OUT/fail-open" ]] || fail "the fail-open route of the local model is empty"
grep -q 'override: {endpoint:' "$OUT/fail-open" || fail "the fail-open route of the local model has no backend"
echo "ok: the fail-open route of the local model has a backend"

# Every route ID is unique and valid for APISIX, also for the test backend's model names.
grep -E '^  - id: ' "$RENDERED" | sed -E 's/^  - id: "?([^"]*)"?$/\1/' > "$OUT/ids"
[[ -z "$(sort "$OUT/ids" | uniq -d)" ]] || fail "duplicate route IDs: $(sort "$OUT/ids" | uniq -d | tr '\n' ' ')"
if grep -Evq '^[a-zA-Z0-9_.-]{1,64}$' "$OUT/ids"; then
    fail "route IDs APISIX does not accept: $(grep -Ev '^[a-zA-Z0-9_.-]{1,64}$' "$OUT/ids" | tr '\n' ' ')"
fi
echo "ok: $(wc -l < "$OUT/ids" | tr -d ' ') route IDs, all unique and valid"
expect "a route for a model named like another model's fail-open route" '^  - id: "model-qwen3-fail-open"$'
expect "a fail-open route for that model" '^  - id: "fail-open-qwen3-fail-open"$'
# Embeddings (#34): every model on /v1/embeddings too, with the backend called on that path.
expect "the client route takes embeddings" '^    uris: \[/v1/chat/completions, /v1/embeddings\]$'
expect "an embeddings route for the local model" '^  - id: "embed-model-qwen3"$'
expect "an embeddings fail-open route for the local model" '^  - id: "embed-fail-open-qwen3"$'
expect "an embeddings route for a model named like a fail-open route" '^  - id: "embed-model-fail-open-qwen3"$'
expect "a route for the long model name, by its full name" "\"http_x_aisa_model\", \"==\", \"$LONG_MODEL\""
expect "a fail-open route for the long model name" "\"http_x_aisa_requested_model\", \"==\", \"$LONG_MODEL\""

# Timeouts (#14, #18): the default, the cap, and a backend left out for a value that is not seconds.
route() { awk -v id="$1" '$0 == "  - id: " id { f = 1; next } /^  - id: / { f = 0 } f' "$RENDERED"; }
expect_in_route() {
    local what=$1 id=$2 pattern=$3 text
    # Read whole, not piped into grep -q: with pipefail, grep's early exit on a match near the
    # start of a long route would fail the pipe (awk gets SIGPIPE).
    text=$(route "$id")
    grep -q -- "$pattern" <<<"$text" || {
        echo "route $id, in which '$pattern' was not found:" >&2
        grep -v '^ *#' <<<"$text" | head -n 60 >&2
        fail "$what"
    }
    echo "ok: $what"
}
expect_in_route "the default timeout of 300 s" "$QWEN3_ROUTE" '^        timeout: 300000$'
expect_in_route "the budget header goes upstream from the decision" 'client' 'upstream_headers: .X-Aisa-Consumer, X-Aisa-Model, X-Aisa-Budget-Remaining.$'
expect_in_route "the budget header is copied to the client's response" 'client' '^            X-Aisa-Budget-Remaining: \$http_x_aisa_budget_remaining$'
expect_in_route "a fast failure is retried on another instance (S11)" "$QWEN3_ROUTE" '^        fallback_strategy: \[http_5xx\]$'
expect_in_route "a slow failure is not retried (S11)" "$QWEN3_ROUTE" '^        retry_on_failure_within_ms: 5000$'
expect_in_route "a chat route calls the backend's chat path" "$QWEN3_ROUTE" 'endpoint: "http://mock-local:8080/v1/chat/completions"'
expect_in_route "an embeddings route matches the embeddings path" '"embed-model-qwen3"' '^    uri: /v1/embeddings$'
expect_in_route "an embeddings route calls the backend's embeddings path" '"embed-model-qwen3"' 'endpoint: "http://mock-local:8080/v1/embeddings"'
expect_in_route "a timeout above 600 s is capped" '"model-slow-test"' '^        timeout: 600000$'
expect "a backend with an invalid timeout is named in a comment" "^# Skipped backend \"render-test-bad-timeout\": invalid 'timeout' metadata \"fast\""
if grep -q '^  - id: "model-bad-timeout-test"$' "$RENDERED"; then
    fail "a backend with an invalid timeout got a route"
fi
echo "ok: no route for the model of a backend with an invalid timeout"
if route "$QWEN3_ROUTE" | grep -q 'render-test-bad-timeout'; then
    fail "a backend with an invalid timeout is among the instances of a shared model"
fi
echo "ok: a backend with an invalid timeout serves no shared model either"

expect_in_route "a backend's path replaces /v1 on its chat route" '"model-path-test"' 'endpoint: "http://mock-local:8080/v1beta/openai/chat/completions"'
expect_in_route "a backend's path replaces /v1 on its embeddings route" '"embed-model-path-test"' 'endpoint: "http://mock-local:8080/v1beta/openai/embeddings"'
expect_in_route "the client still calls the contract's path" '"model-path-test"' '^    uri: /v1/chat/completions$'
for n in 1 2 3; do
    expect "a backend with an invalid path is named in a comment ($n)" "^# Skipped backend \"render-test-bad-path-$n\": invalid 'path' metadata"
    if grep -q "^  - id: \"model-bad-path-test-$n\"$" "$RENDERED" || route "$QWEN3_ROUTE" | grep -q "render-test-bad-path-$n"; then
        fail "a backend with an invalid path ($n) got a route or serves a shared model"
    fi
done
echo "ok: a backend with an invalid path serves nothing"

echo "ok: all render tests passed"
