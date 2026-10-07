#!/usr/bin/env bash
# Installs aisa's chart, with its proxy, into the current kubectl context and sends requests
# through the proxy. Everything the chart expects to find is set up first (fixtures.yaml): Vault
# with Kubernetes auth, the roles of docs/concepts/VAULT.md and Vault's Consul secrets engine;
# Consul with ACLs and the policies of docs/concepts/CONSUL.md; two mock backends.
#
# It checks what only a cluster shows: the logins with Kubernetes auth, the Consul tokens from
# Vault, a provider key rendered from Vault, the NetworkPolicy in front of aisa, and that
# helm uninstall removes the proxy with aisa.
#
#   kind create cluster --name aisa
#   docker build -t aisa:ci . && docker build -f dev/Dockerfile -t aisa-dev-tools:ci .
#   kind load docker-image --name aisa aisa:ci aisa-dev-tools:ci
#   ./deploy/helm/test/install_test.sh
#
# Needs kubectl, helm, curl and jq.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
NS="${NAMESPACE:-aisa}"
RELEASE=aisa
K=(kubectl -n "$NS")

pass=0
ok() { pass=$((pass + 1)); echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
expect() {
    if [[ "$2" = "$3" ]]; then ok "$1"; else fail "$1: want '$2', got '$3'"; fi
}

pf_pids=()
cleanup() {
    for pid in "${pf_pids[@]}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

# port_forward <service> <local port> <service port>: forwards in the background and waits.
port_forward() {
    "${K[@]}" port-forward "svc/$1" "$2:$3" >/dev/null 2>&1 &
    pf_pids+=($!)
    for _ in $(seq 1 30); do
        curl -s -o /dev/null "http://127.0.0.1:$2/" && return 0
        sleep 1
    done
    fail "port-forward to $1"
}

vault() { "${K[@]}" exec -i deploy/vault -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=test-root vault "$@"; }
consul() { "${K[@]}" exec deploy/consul -- env CONSUL_HTTP_TOKEN=test-root consul "$@"; }

echo "== fixtures"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
sed "s/NAMESPACE/$NS/" "$HERE/fixtures.yaml" | "${K[@]}" apply -f - >/dev/null
for d in vault consul mock-local mock-cloud; do
    "${K[@]}" rollout status "deploy/$d" --timeout=180s >/dev/null
done
ok "Vault, Consul and the mock backends are ready"

echo "== Consul: ACL policies, quota profiles, backends"
consul acl policy create -name aisa \
    -rules 'key_prefix "aisa/" { policy = "write" } service "aisa-backend" { policy = "read" }' >/dev/null
consul acl policy create -name aisa-render \
    -rules 'service "aisa-backend" { policy = "read" } node_prefix "" { policy = "read" }' >/dev/null
consul kv put aisa/quotas/interactive '{"tokens_per_hour": 1000000}' >/dev/null
"${K[@]}" exec -i deploy/consul -- env CONSUL_HTTP_TOKEN=test-root sh -c 'cat > /tmp/backends.hcl && consul services register /tmp/backends.hcl' >/dev/null <<HCL
services {
  id      = "mock-local"
  name    = "aisa-backend"
  address = "mock-local.$NS.svc.cluster.local"
  port    = 8080
  meta    = { provider = "openai-compatible", models = "qwen3", fail_policy = "open" }
  check {
    http     = "http://mock-local.$NS.svc.cluster.local:8080/healthz"
    interval = "2s"
  }
}
services {
  id      = "mock-cloud"
  name    = "aisa-backend"
  address = "mock-cloud.$NS.svc.cluster.local"
  port    = 8080
  # A paid backend: its key is rendered from Vault (secret/aisa/providers/cloud).
  meta    = { provider = "openai-compatible", models = "cloud-large", key = "cloud", scheme = "http" }
  check {
    http     = "http://mock-cloud.$NS.svc.cluster.local:8080/healthz"
    interval = "2s"
  }
}
HCL
for _ in $(seq 1 30); do
    passing=$("${K[@]}" exec deploy/consul -- wget -q -O - --header "X-Consul-Token: test-root" \
        "http://127.0.0.1:8500/v1/health/service/aisa-backend?passing" | jq length)
    [[ "$passing" = 2 ]] && break
    sleep 1
done
expect "backends passing in Consul" 2 "$passing"

echo "== Vault: Kubernetes auth, roles, secrets, Consul secrets engine"
vault auth enable kubernetes >/dev/null
# Vault runs in the cluster: it reviews tokens with its own service account and CA.
vault write auth/kubernetes/config kubernetes_host=https://kubernetes.default.svc >/dev/null
vault policy write aisa - >/dev/null <<'HCL'
path "secret/data/aisa/consumers/*" { capabilities = ["read"] }
path "secret/metadata/aisa/consumers" { capabilities = ["list"] }
path "secret/metadata/aisa/consumers/*" { capabilities = ["list"] }
path "consul/creds/aisa" { capabilities = ["read"] }
HCL
vault policy write aisa-render - >/dev/null <<'HCL'
path "secret/data/aisa/providers/*" { capabilities = ["read"] }
path "consul/creds/aisa-render" { capabilities = ["read"] }
HCL
# The charts project the service account tokens with the audience "vault" (values-test.yaml).
vault write auth/kubernetes/role/aisa bound_service_account_names="$RELEASE" \
    bound_service_account_namespaces="$NS" audience=vault policies=aisa ttl=1h >/dev/null
vault write auth/kubernetes/role/aisa-render bound_service_account_names="$RELEASE-apisix" \
    bound_service_account_namespaces="$NS" audience=vault policies=aisa-render ttl=1h >/dev/null
sha256() { printf %s "$1" | sha256sum | cut -d' ' -f1; }
vault kv put secret/aisa/consumers/chat-ui key_sha256="$(sha256 test-key-chat-ui)" quota_profile=interactive >/dev/null
vault kv put secret/aisa/providers/cloud api_key=test-provider-key >/dev/null
vault secrets enable consul >/dev/null
vault write consul/config/access address=consul:8500 token=test-root >/dev/null
vault write consul/roles/aisa consul_policies=aisa ttl=1h >/dev/null
vault write consul/roles/aisa-render consul_policies=aisa-render ttl=1h >/dev/null
ok "Vault configured"

echo "== helm install"
helm dependency update "$ROOT/deploy/helm/aisa" >/dev/null
helm install "$RELEASE" "$ROOT/deploy/helm/aisa" -n "$NS" -f "$HERE/values-test.yaml" \
    --wait --timeout 5m >/dev/null || {
    "${K[@]}" get pods -o wide >&2
    "${K[@]}" describe pods >&2
    fail "helm install"
}
ok "aisa and its proxy installed and ready"

echo "== aisa"
port_forward "$RELEASE" 18080 8080
readyz=$(curl -fsS http://127.0.0.1:18080/readyz)
echo "     readyz: $readyz"
metrics=$(curl -fsS http://127.0.0.1:18080/metrics)
grep -q '^aisa_consumer_loads_total{result="ok"}' <<<"$metrics" ||
    fail "aisa loaded its consumers from Vault (Kubernetes auth as the role aisa)"
ok "aisa loaded its consumers from Vault (Kubernetes auth as the role aisa)"
profiles=$(awk '/^aisa_quota_profiles /{print $2}' <<<"$metrics")
expect "aisa read its quota profiles from Consul (a token from Vault)" 1 "$profiles"

echo "== requests through the proxy"
port_forward "$RELEASE-apisix" 19080 80
chat() {
    curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" \
        -H 'Content-Type: application/json' -d "{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
        http://127.0.0.1:19080/v1/chat/completions
}
# The proxy loads its routes a moment after it is ready; wait for the first answer from aisa.
for _ in $(seq 1 30); do
    status=$(chat test-key-chat-ui qwen3 || true)
    [[ "$status" = 200 ]] && break
    sleep 1
done
expect "a known key, a local model" 200 "$status"
expect "a known key, a paid model (its key rendered from Vault)" 200 "$(chat test-key-chat-ui cloud-large)"
expect "an unknown key" 401 "$(chat test-key-unknown qwen3)"

"${K[@]}" logs deploy/mock-cloud | grep -q '"authorization_present":true' ||
    fail "the paid backend got the provider key"
ok "the paid backend got the provider key"
if "${K[@]}" logs deploy/mock-local | grep -q '"authorization_present":true'; then
    fail "the local backend got no credential: it got one"
fi
ok "the local backend got no credential"

# The usage events reach aisa a moment after the answers.
for _ in $(seq 1 15); do
    tokens=$(curl -fsS http://127.0.0.1:18080/metrics |
        awk '/^aisa_tokens_total\{.*consumer="chat-ui"/{s += $2} END {print s + 0}')
    [[ "$tokens" != 0 ]] && break
    sleep 1
done
[[ "$tokens" != 0 ]] || fail "usage events reached aisa"
ok "usage events reached aisa ($tokens tokens for chat-ui)"

echo "== NetworkPolicy"
# probe_aisa <name> [labels]: asks aisa's /healthz from a new pod with these labels.
probe_aisa() {
    "${K[@]}" run "$1" --rm -i --restart=Never --quiet --labels="${2:-}" \
        --image=aisa-dev-tools:ci --image-pull-policy=Never \
        --command -- /usr/local/bin/mockbackend -healthcheck "http://$RELEASE:8080/healthz" >/dev/null 2>&1
}
probe_aisa np-proxy "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=proxy" ||
    fail "a pod with the proxy's labels reaches aisa"
ok "a pod with the proxy's labels reaches aisa"
if probe_aisa np-other "app=np-other"; then
    fail "a pod that is not the proxy is kept away from aisa"
fi
ok "a pod that is not the proxy is kept away from aisa"

echo "== helm uninstall"
cleanup
pf_pids=()
helm uninstall "$RELEASE" -n "$NS" --wait --timeout 2m >/dev/null
left=$("${K[@]}" get deploy,svc,cm,sa,networkpolicy -l app.kubernetes.io/part-of=aisa -o name | wc -l)
expect "helm uninstall removes aisa, Redis and the proxy" 0 "$left"

echo "ok: $pass checks passed"
