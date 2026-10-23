#!/usr/bin/env bash
# consul-template survives the rotation of its Consul token (#90): with the shipped consul block
# (unlimited retries) and consul-token-watch.sh, it keeps rendering the catalog after the token
# it started with is replaced in the file and deleted in Consul, as the Vault Agent and Vault's
# Consul secrets engine do. Before #90 it kept the deleted token, was answered 403 until its
# retries ran out and exited with code 14.
#
# Runs its own Consul with ACLs and consul-template in Docker; needs docker, nothing else.
#
#   ./proxies/apisix/token_rotation_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CT_IMAGE="${CONSUL_TEMPLATE_IMAGE:-hashicorp/consul-template:0.43.0}"
CONSUL_IMAGE="${CONSUL_IMAGE:-hashicorp/consul:2.0.4}"
NAME="aisa-token-rotation-$$"
ROTATIONS=3

WORK=$(mktemp -d)
cleanup() {
    docker rm -f "$NAME-consul" "$NAME-ct" >/dev/null 2>&1 || true
    docker network rm "$NAME" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT
fail() {
    echo "FAIL: $1" >&2
    docker logs "$NAME-ct" 2>&1 | tail -20 >&2 || true
    exit 1
}

docker network create "$NAME" >/dev/null
docker run -d --name "$NAME-consul" --network "$NAME" "$CONSUL_IMAGE" agent -dev -client 0.0.0.0 \
    -hcl 'acl { enabled = true default_policy = "deny" tokens { initial_management = "root" } }' >/dev/null
consul() { docker exec -e CONSUL_HTTP_TOKEN=root "$NAME-consul" consul "$@"; }
for _ in $(seq 1 30); do consul members >/dev/null 2>&1 && break; sleep 1; done
consul members >/dev/null || fail "Consul did not start"

# The render role's rules (docs/concepts/CONSUL.md, ACLs).
consul acl policy create -name aisa-render \
    -rules 'service "aisa-backend" { policy = "read" } node_prefix "" { policy = "read" }' >/dev/null
# new_token prints "<accessor> <secret>" of a new token of the render role.
new_token() {
    consul acl token create -policy-name aisa-render -format json |
        awk -F'"' '/"AccessorID"/{a = $4} /"SecretID"/{s = $4} END {print a, s}'
}

mkdir -p "$WORK/etc" "$WORK/vault" "$WORK/out"
# consul-template runs as a user of its own in the container.
chmod 777 "$WORK/vault" "$WORK/out"
# The shipped consul block, with a template that renders the catalog the way the proxy's does.
sed -n '/^consul {/,/^}/p' "$SCRIPT_DIR/consul-template.hcl" >"$WORK/etc/ct.hcl"
grep -q 'attempts *= *0' "$WORK/etc/ct.hcl" || fail "the shipped consul-template.hcl retries Consul a limited number of times"
cat >>"$WORK/etc/ct.hcl" <<'EOF'
template {
  contents    = "{{ range service \"aisa-backend\" }}{{ .ID }} {{ end }}"
  destination = "/out/backends"
}
EOF
cp "$SCRIPT_DIR/consul-token-watch.sh" "$WORK/etc/"

# write_token replaces the token file the way the Vault Agent does: a new file renamed over it.
write_token() { printf %s "$1" >"$WORK/vault/consul-token.new" && mv "$WORK/vault/consul-token.new" "$WORK/vault/consul-token"; }
read -r accessor secret < <(new_token)
write_token "$secret"
consul services register -name aisa-backend -id backend-0 -address 192.0.2.1 -port 8080 >/dev/null

# Started as the chart starts it (chart/templates/deployment.yaml).
docker run -d --name "$NAME-ct" --network "$NAME" -e CONSUL_HTTP_ADDR="$NAME-consul:8500" \
    -e CONSUL_TOKEN_WATCH_INTERVAL=1 \
    -v "$WORK/etc":/etc/apisix/aisa:ro -v "$WORK/vault":/vault:ro -v "$WORK/out":/out \
    --entrypoint sh "$CT_IMAGE" -c '/etc/apisix/aisa/consul-token-watch.sh /vault/consul-token $$ &
        exec /bin/consul-template "$@"' consul-template \
    -config=/etc/apisix/aisa/ct.hcl -consul-token-file=/vault/consul-token >/dev/null

# wait_for <backend>: the rendered file names it within 30 s.
wait_for() {
    local backend=$1
    for _ in $(seq 1 30); do
        grep -qw "$backend" "$WORK/out/backends" 2>/dev/null && return 0
        sleep 1
    done
    fail "the render does not name $backend after 30 s: $(cat "$WORK/out/backends" 2>/dev/null)"
}
wait_for backend-0
echo "ok: rendered with the first token"

for i in $(seq 1 "$ROTATIONS"); do
    old=$accessor
    read -r accessor secret < <(new_token)
    write_token "$secret"
    # The old token is deleted at once, the worst case: a query in flight with it gets 403.
    consul acl token delete -accessor-id "$old" >/dev/null
    consul services register -name aisa-backend -id "backend-$i" -address 192.0.2.1 -port 8080 >/dev/null
    wait_for "backend-$i"
    [[ "$(docker inspect -f '{{.State.Running}}' "$NAME-ct")" == true ]] ||
        fail "consul-template exited after rotation $i"
    echo "ok: rotation $i: the catalog renders with the new token"
done

reloads=$(docker logs "$NAME-ct" 2>&1 | grep -c 'the Consul token changed' || true)
[[ "$reloads" == "$ROTATIONS" ]] || fail "$reloads reloads for $ROTATIONS rotations"
echo "ok: one reload per rotation"
echo "ok: all token rotation tests passed"
