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
# route the same ID ("qwen3-fail-open" next to the fail-open "qwen3"), and a name too long and
# with characters a route ID cannot have.
LONG_MODEL="example-org/a-model-name-that-is-much-longer-than-a-route-id-may-be:latest"
cleanup() {
    curl -fsS -o /dev/null -X PUT "$CONSUL/v1/agent/service/deregister/render-test" || true
    rm -rf "$OUT"
}
trap cleanup EXIT
curl -fsS -o /dev/null -X PUT -d @- "$CONSUL/v1/agent/service/register" <<EOF
{"ID": "render-test", "Name": "aisa-backend", "Address": "mock-local", "Port": 8080,
 "Meta": {"provider": "openai-compatible", "models": "qwen3-fail-open,$LONG_MODEL", "fail_policy": "open"}}
EOF
# consul-template runs as a user of its own in the container.
chmod 777 "$OUT"

echo "== render apisix.yaml.ctmpl"
docker run --rm --network "$NETWORK" \
    -e CONSUL_HTTP_ADDR=consul:8500 -e VAULT_ADDR=http://vault:8200 -e VAULT_TOKEN=dev-root \
    -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$OUT":/rendered \
    "$IMAGE" -config=/etc/apisix/aisa/consul-template.hcl -once

RENDERED="$OUT/apisix.yaml"
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
expect "a route for the long model name, by its full name" "\"http_x_aisa_model\", \"==\", \"$LONG_MODEL\""
expect "a fail-open route for the long model name" "\"http_x_aisa_requested_model\", \"==\", \"$LONG_MODEL\""

echo "ok: all render tests passed"
