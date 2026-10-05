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

OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
# consul-template runs as a user of its own in the container.
chmod 777 "$OUT"

echo "== render apisix.yaml.ctmpl"
docker run --rm --network "$NETWORK" \
    -e CONSUL_HTTP_ADDR=consul:8500 -e VAULT_ADDR=http://vault:8200 -e VAULT_TOKEN=dev-root \
    -v "$SCRIPT_DIR":/etc/apisix/aisa:ro -v "$OUT":/rendered \
    "$IMAGE" -config=/etc/apisix/aisa/consul-template.hcl -once

RENDERED="$OUT/apisix.yaml"
fail() { echo "FAIL: $1" >&2; exit 1; }

[ -s "$RENDERED" ] || fail "guard.sh promoted nothing"

expect() {
    grep -q -- "$2" "$RENDERED" || fail "$1"
    echo "ok: $1"
}
expect "the client-facing route" '^  - id: client$'
expect "the fallback route" '^  - id: no-backend$'
expect "the route for an unreachable aisa" '^  - id: aisa-unreachable$'
expect "the client route reports only requests that end there" 'filter: \[\["http_x_aisa_consumer", "!", "~~", "."\], \["upstream_addr", "!", "~~", "."\]\]'
expect "a route for the local model" '^  - id: "model-qwen3"$'
expect "a route for the paid model" '^  - id: "model-cloud-large"$'
expect "a fail-open route for the local model" '^  - id: "model-qwen3-fail-open"$'
expect "the gateway's metrics for every route" '^      prometheus: {}$'

if grep -q '^  - id: "model-cloud-large-fail-open"$' "$RENDERED"; then
    fail "a paid backend has a fail-open route"
fi
echo "ok: no fail-open route for the paid model"

# The fail-open route of the local model serves only its fail-open backends.
awk '/^  - id: "model-qwen3-fail-open"$/{f=1;next} /^  - id: /{f=0} f' "$RENDERED" > "$OUT/fail-open"
[ -s "$OUT/fail-open" ] || fail "the fail-open route of the local model is empty"
grep -q 'override: {endpoint:' "$OUT/fail-open" || fail "the fail-open route of the local model has no backend"
echo "ok: the fail-open route of the local model has a backend"

echo "ok: all render tests passed"
