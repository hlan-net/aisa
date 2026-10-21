#!/usr/bin/env bash
# Installs aisa's chart, with its proxy, into the current kubectl context and sends requests
# through the proxy. Everything the chart expects to find is set up first (fixtures.yaml): Vault
# with Kubernetes auth, the roles of docs/concepts/VAULT.md and Vault's Consul secrets engine;
# Consul with ACLs and the policies of docs/concepts/CONSUL.md; two mock backends.
#
# It checks what only a cluster shows: the logins with Kubernetes auth, the Consul tokens from
# Vault, a provider key rendered from Vault, the consumer's Secret and the Model objects that
# aisa writes, the NetworkPolicy in front of aisa, and that helm uninstall removes the proxy
# with aisa.
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
# Where the consumer batch-jobs runs: aisa writes its Secret there.
APP_NS="${APP_NAMESPACE:-aisa-app}"
K=(kubectl -n "$NS")
JSON_TYPE="Content-Type: application/json"

pass=0
ok() { pass=$((pass + 1)); echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
expect() {
    local what=$1 want=$2 got=$3
    if [[ "$got" = "$want" ]]; then ok "$what"; else fail "$what: want '$want', got '$got'"; fi
}

pf_pids=()
cleanup() {
    for pid in "${pf_pids[@]}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

# port_forward <service> <local port> <service port>: forwards in the background and waits.
port_forward() {
    local service=$1 local_port=$2 service_port=$3
    "${K[@]}" port-forward "svc/$service" "$local_port:$service_port" >/dev/null 2>&1 &
    pf_pids+=($!)
    for _ in $(seq 1 30); do
        curl -s -o /dev/null "http://127.0.0.1:$local_port/" && return 0
        sleep 1
    done
    fail "port-forward to $service"
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
# aisa reads the backends' health for the Model objects; Consul filters the health of a
# service by the nodes the token may read.
consul acl policy create -name aisa \
    -rules 'key_prefix "aisa/" { policy = "write" } service "aisa-backend" { policy = "read" } node_prefix "" { policy = "read" }' >/dev/null
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
path "secret/data/aisa/consumers/*" { capabilities = ["create", "read", "update"] }
path "secret/metadata/aisa/consumers" { capabilities = ["list"] }
path "secret/metadata/aisa/consumers/*" { capabilities = ["list", "delete"] }
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
sha256() {
    local text=$1
    printf %s "$text" | sha256sum | cut -d' ' -f1
}
# The consumer: declared in Consul, its key's hash in Vault.
consul kv put aisa/consumers/chat-ui '{"quota_profile": "interactive"}' >/dev/null
vault kv put secret/aisa/consumers/chat-ui key_sha256="$(sha256 test-key-chat-ui)" >/dev/null
# A consumer whose key aisa issues and writes into a Secret in its application's namespace.
kubectl create namespace "$APP_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
consul kv put aisa/consumers/batch-jobs "{\"quota_profile\": \"interactive\", \"namespace\": \"$APP_NS\", \"secret\": \"aisa\"}" >/dev/null
# A description of one model, for its Model object.
consul kv put aisa/models/qwen3 '{"contextLength": 32768, "capabilities": ["tools"]}' >/dev/null
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
# /metrics and the probes are on a port and a Service of their own (#46).
port_forward "$RELEASE-metrics" 18090 9090
readyz=$(curl -fsS http://127.0.0.1:18090/readyz)
echo "     readyz: $readyz"
metrics=$(curl -fsS http://127.0.0.1:18090/metrics)
grep -q '^aisa_consumer_loads_total{result="ok"}' <<<"$metrics" ||
    fail "aisa loaded its consumers' keys from Vault (Kubernetes auth as the role aisa)"
ok "aisa loaded its consumers' keys from Vault (Kubernetes auth as the role aisa)"
declarations=$(awk '/^aisa_consumer_declarations /{print $2}' <<<"$metrics")
expect "aisa read its consumer declarations from Consul (a token from Vault): chat-ui and batch-jobs" 2 "$declarations"
profiles=$(awk '/^aisa_quota_profiles /{print $2}' <<<"$metrics")
expect "aisa read its quota profiles from Consul (a token from Vault)" 1 "$profiles"
# The proxy reports its renders to the URI the chart gives it (AISA_RENDER_URI). The first
# render comes before aisa is up, so the proxy's loop sends the report again within 30 s.
promoted=0
for _ in $(seq 1 45); do
    promoted=$(curl -fsS http://127.0.0.1:18090/metrics | awk '/^aisa_config_renders_total\{result="promoted"\}/{print $2}')
    [[ "${promoted:-0}" != 0 ]] && break
    sleep 2
done
[[ "${promoted:-0}" != 0 ]] || fail "the proxy reported a promoted render to aisa's /v1/render"
ok "the proxy reported a promoted render to aisa's /v1/render ($promoted)"
models=$(curl -fsS http://127.0.0.1:18090/metrics | awk '/^aisa_config_models /{print $2}')
expect "aisa knows the models of the rendered config" 2 "$models"

echo "== requests through the proxy"
port_forward "$RELEASE-apisix" 19080 80
chat() {
    local key=$1 model=$2
    curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $key" \
        -H "$JSON_TYPE" -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
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
    tokens=$(curl -fsS http://127.0.0.1:18090/metrics |
        awk '/^aisa_tokens_total\{.*consumer="chat-ui"/{s += $2} END {print s + 0}')
    [[ "$tokens" != 0 ]] && break
    sleep 1
done
[[ "$tokens" != 0 ]] || fail "usage events reached aisa"
ok "usage events reached aisa ($tokens tokens for chat-ui)"

# aisa times its own decisions (#71): every request above was decided, and a decision takes a
# small part of a request, well under a second, even on a runner.
decide=$(curl -fsS http://127.0.0.1:18090/metrics |
    awk '/^aisa_decide_duration_seconds_count/{n += $2} /^aisa_decide_duration_seconds_sum/{s += $2} END {printf "%d %f", n, s}')
read -r decide_count decide_sum <<<"$decide"
(( decide_count >= 3 )) || fail "aisa timed its decisions: want at least 3, got $decide_count"
awk -v n="$decide_count" -v s="$decide_sum" 'BEGIN { exit !(s / n < 1) }' ||
    fail "aisa's decisions take under a second on average: $decide_sum s for $decide_count"
ok "aisa timed its decisions ($decide_count in $decide_sum s)"

echo "== Kubernetes objects"
# The consumer's Secret: written by aisa, with a key that it issued.
for _ in $(seq 1 60); do
    key=$(kubectl -n "$APP_NS" get secret aisa -o jsonpath='{.data.OPENAI_API_KEY}' 2>/dev/null | base64 -d || true)
    [[ -n "$key" ]] && break
    sleep 1
done
[[ -n "$key" ]] || fail "aisa wrote the Secret of batch-jobs in $APP_NS"
ok "aisa wrote the Secret of batch-jobs in $APP_NS"
expect "the Secret's base URL is the proxy's Service" "http://$RELEASE-apisix.$NS.svc.cluster.local/v1" \
    "$(kubectl -n "$APP_NS" get secret aisa -o jsonpath='{.data.OPENAI_BASE_URL}' | base64 -d)"
expect "the Secret names its consumer" batch-jobs \
    "$(kubectl -n "$APP_NS" get secret aisa -o jsonpath='{.metadata.annotations.aisa\.hlan\.net/consumer-name}')"
# aisa learns the new key's hash from Vault on the first request that carries it.
for _ in $(seq 1 15); do
    status=$(chat "$key" qwen3 || true)
    [[ "$status" = 200 ]] && break
    sleep 1
done
expect "the key in the Secret passes the proxy" 200 "$status"
issued=$(curl -fsS http://127.0.0.1:18090/metrics | awk '/^aisa_consumer_keys_issued_total\{reason="new"\}/{print $2}')
expect "aisa counted the key it issued" 1 "$issued"

# aisa's Kubernetes client is not rate limited by client-go (#70); the log is at debug, where
# client-go reports its waits. Its requests are in aisa's metrics.
# Read whole, not piped into grep -q: with pipefail, grep's early exit would fail the pipe.
aisa_log=$("${K[@]}" logs deploy/"$RELEASE" -c aisa)
if grep -q 'client-side throttling' <<<"$aisa_log"; then
    fail "aisa's Kubernetes client does not throttle itself"
fi
ok "aisa's Kubernetes client does not throttle itself"
metrics=$(curl -fsS http://127.0.0.1:18090/metrics)
grep -q '^rest_client_requests_total{' <<<"$metrics" ||
    fail "aisa's metrics have its Kubernetes client's requests"
ok "aisa's metrics have its Kubernetes client's requests"

# The Model objects: one per model on offer, found by what they can do.
for _ in $(seq 1 60); do
    models=$(kubectl get models.aisa.hlan.net -l feature.aisa.hlan.net/TextGeneration -o jsonpath='{range .items[*]}{.spec.model} {end}' 2>/dev/null || true)
    [[ "$models" == *qwen3* && "$models" == *cloud-large* ]] && break
    sleep 1
done
[[ "$models" == *qwen3* && "$models" == *cloud-large* ]] || fail "a Model object for each model on offer: got '$models'"
ok "a Model object for each model on offer: $models"
expect "a model found by its capability" qwen3 \
    "$(kubectl get models.aisa.hlan.net -l capability.aisa.hlan.net/tools -o jsonpath='{.items[*].spec.model}')"
qwen=$(kubectl get models.aisa.hlan.net -l capability.aisa.hlan.net/tools -o json | jq -c '.items[0] | [.spec.contextLength, .spec.endpoint, .status.available]')
expect "qwen3's description, endpoint and health" "[32768,\"http://$RELEASE-apisix.$NS.svc.cluster.local/v1\",true]" "$qwen"
expect "any service account reads the models" yes \
    "$(kubectl auth can-i list models.aisa.hlan.net --as=system:serviceaccount:default:default)"
expect "no other service account writes them" no \
    "$(kubectl auth can-i create models.aisa.hlan.net --as=system:serviceaccount:default:default)"

# A consumer removed from Consul loses its Secret, and its key no longer passes.
consul kv delete aisa/consumers/batch-jobs >/dev/null
for _ in $(seq 1 60); do
    kubectl -n "$APP_NS" get secret aisa >/dev/null 2>&1 || break
    sleep 1
done
if kubectl -n "$APP_NS" get secret aisa >/dev/null 2>&1; then
    fail "the Secret of a removed consumer is deleted"
fi
ok "the Secret of a removed consumer is deleted"
expect "the key of a removed consumer" 401 "$(chat "$key" qwen3)"

echo "== a backend registered in the catalog"
# A backend outside the cluster is registered through the catalog, on a node without a Consul
# agent, as CONSUL.md's Terraform example does (#69). Nothing runs its check, so its status is
# what the registration wrote, and the catalog's default is critical.
port_forward consul 18500 8500
catalog() {
    local path=$1 body=$2
    curl -fsS -o /dev/null -X PUT -H "X-Consul-Token: test-root" -d "$body" "http://127.0.0.1:18500/v1/catalog/$path"
}
static_backend() {
    local status=$1
    jq -cn --arg addr "mock-local.$NS.svc.cluster.local" --arg status "$status" '{
        Node: "static-box", Address: $addr, NodeMeta: {"external-node": "true"},
        Service: {ID: "mock-static", Service: "aisa-backend", Address: $addr, Port: 8080,
                  Meta: {provider: "openai-compatible", models: "static-model"}},
        Check: ({CheckID: "service:mock-static", Name: "mock-static", ServiceID: "mock-static"}
                + if $status == "" then {} else {Status: $status} end)}'
}
listed() {
    curl -fsS http://127.0.0.1:19080/v1/models | jq -r '[.data[].id] | index("static-model") != null'
}
check_output() {
    curl -fsS -H "X-Consul-Token: test-root" "http://127.0.0.1:18500/v1/health/checks/aisa-backend" |
        jq -r '[.[] | select(.CheckID == "service:mock-static")][0] | [.Status, .Output] | @tsv'
}
catalog register "$(static_backend "")"
# Give the proxy the time it takes to render a change, and more, before saying it did not.
sleep 10
expect "a catalog registration without a status is critical, with no output" $'critical\t' "$(check_output)"
expect "a critical backend's model is not on offer" false "$(listed)"
catalog register "$(static_backend passing)"
for _ in $(seq 1 60); do
    [[ "$(listed)" = true ]] && break
    sleep 1
done
expect "a catalog registration with status passing is passing, with no output" $'passing\t' "$(check_output)"
expect "a passing backend's model is on offer" true "$(listed)"
catalog deregister '{"Node": "static-box"}'
for _ in $(seq 1 60); do
    [[ "$(listed)" = false ]] && break
    sleep 1
done
expect "a deregistered backend's model is no longer on offer" false "$(listed)"

echo "== admin interface"
port_forward "$RELEASE" 18080 8080
overview=$(curl -fsS http://127.0.0.1:18080/admin/api/overview)
expect "admin: overview status" ok "$(jq -r .status <<<"$overview")"
# Refuse rotation for a consumer whose Secret is managed by Kubernetes
consul kv put aisa/consumers/k8s-managed "{\"quota_profile\":\"interactive\",\"namespace\":\"$APP_NS\",\"secret\":\"test\"}" >/dev/null
expect "admin: rotate refused for consumer with Secret" 400 \
    "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "$JSON_TYPE" http://127.0.0.1:18080/admin/api/consumers/k8s-managed/rotate-key)"
consul kv delete aisa/consumers/k8s-managed >/dev/null
# Create a standalone consumer with an issued key
admin_created=$(curl -fsS -X POST -H "$JSON_TYPE" \
    -d '{"name":"admin-user","quota_profile":"interactive","generate_key":true}' \
    http://127.0.0.1:18080/admin/api/consumers)
admin_key=$(jq -r .plaintext_key <<<"$admin_created")
if [[ "$admin_key" =~ ^aisa-[0-9a-f]{64}$ ]]; then
    ok "admin: created standalone key format valid"
else
    fail "admin: created standalone key invalid: '$admin_key'"
fi
# Rotate the standalone consumer's key (CAS update with grace period preservation)
rotated=$(curl -fsS -X POST -H "$JSON_TYPE" \
    http://127.0.0.1:18080/admin/api/consumers/admin-user/rotate-key)
rot_key=$(jq -r .plaintext_key <<<"$rotated")
if [[ "$rot_key" =~ ^aisa-[0-9a-f]{64}$ ]]; then
    ok "admin: rotated standalone key format valid"
else
    fail "admin: rotated standalone key invalid: '$rot_key'"
fi
# Delete the consumer (metadata and data purged from Vault with delete capability, deleted from Consul)
del_code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE http://127.0.0.1:18080/admin/api/consumers/admin-user)
expect "admin: deleted consumer status" 200 "$del_code"

echo "== NetworkPolicy"
# probe_aisa <name> <url> [labels]: asks url from a new pod with these labels. /healthz answers on
# both of aisa's ports, so it shows whether the pod reaches the port at all.
probe_aisa() {
    local name=$1 url=$2 labels=${3:-}
    "${K[@]}" run "$name" --rm -i --restart=Never --quiet --labels="$labels" \
        --image=aisa-dev-tools:ci --image-pull-policy=Never \
        --command -- /usr/local/bin/mockbackend -healthcheck "$url" >/dev/null 2>&1
}
api="http://$RELEASE:8080/healthz"
ops="http://$RELEASE-metrics:9090/healthz"
probe_aisa np-proxy "$api" "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=proxy" ||
    fail "a pod with the proxy's labels reaches aisa's API"
ok "a pod with the proxy's labels reaches aisa's API"
if probe_aisa np-other "$api" "app=np-other"; then
    fail "a pod that is not the proxy is kept away from aisa's API"
fi
ok "a pod that is not the proxy is kept away from aisa's API"
# A scraper in networkPolicy.extraFrom (values-test.yaml) reaches the metrics port, and not the
# port of /v1/usage and /v1/decide (#46).
probe_aisa np-scraper "$ops" "app=np-scraper" ||
    fail "a pod in extraFrom reaches aisa's metrics port"
ok "a pod in extraFrom reaches aisa's metrics port"
if probe_aisa np-scraper-api "$api" "app=np-scraper"; then
    fail "a pod in extraFrom is kept away from aisa's API, and cannot send usage events"
fi
ok "a pod in extraFrom is kept away from aisa's API, and cannot send usage events"
if probe_aisa np-other-ops "$ops" "app=np-other"; then
    fail "a pod in no peer is kept away from aisa's metrics port"
fi
ok "a pod in no peer is kept away from aisa's metrics port"

echo "== helm uninstall"
cleanup
pf_pids=()
helm uninstall "$RELEASE" -n "$NS" --wait --timeout 2m >/dev/null
left=$("${K[@]}" get deploy,svc,cm,sa,networkpolicy,role,rolebinding -l app.kubernetes.io/part-of=aisa -o name | wc -l)
expect "helm uninstall removes aisa, Redis and the proxy" 0 "$left"
left=$(kubectl get clusterrole,clusterrolebinding -l app.kubernetes.io/part-of=aisa -o name | wc -l)
expect "helm uninstall removes aisa's ClusterRoles and their bindings" 0 "$left"

echo "ok: $pass checks passed"
