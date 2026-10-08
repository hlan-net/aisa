#!/usr/bin/env bash
# aisa on a local cluster, with the Ollama of the machine as its backend: for trying the whole
# path on a laptop, from an application's Secret to a model the host runs.
#
#   ./deploy/helm/local/up.sh
#
# It uses the current kubectl context (CONTEXT overrides it): Docker Desktop's Kubernetes or a
# kind cluster. It builds aisa's images and loads them into the cluster, applies the test
# fixtures (a dev-mode Vault with Kubernetes auth, a dev-mode Consul with ACLs, two mock
# backends), registers the host's Ollama in Consul with every model it serves, creates one
# consumer, installs aisa's chart with the proxy published on localhost, and writes the
# consumer's Secret into an application namespace, in the shape of KUBERNETES_OBJECTS.md.
#
# Nothing of this is for a real cluster: the fixtures keep everything in memory and have fixed
# root tokens. ./down.sh removes it all. Needs docker, kubectl, helm, curl, jq and openssl.
#
#   CONTEXT        kubectl context                          (the current one)
#   NAMESPACE      aisa's namespace                         aisa
#   RELEASE        the Helm release                         aisa
#   OLLAMA_ADDR    Ollama as the cluster reaches it         host.docker.internal:11434
#   CONSUMER       the consumer to create                   opencode
#   APP_NAMESPACE  where its Secret goes                    $CONSUMER
#   TOKENS_PER_HOUR its quota                               2000000
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../../.." && pwd)"
CONTEXT="${CONTEXT:-$(kubectl config current-context)}"
NS="${NAMESPACE:-aisa}"
RELEASE="${RELEASE:-aisa}"
OLLAMA_ADDR="${OLLAMA_ADDR:-host.docker.internal:11434}"
CONSUMER="${CONSUMER:-opencode}"
APP_NS="${APP_NAMESPACE:-$CONSUMER}"
TOKENS_PER_HOUR="${TOKENS_PER_HOUR:-2000000}"
K=(kubectl --context "$CONTEXT")
KN=("${K[@]}" -n "$NS")

ok() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
vault() { "${KN[@]}" exec -i deploy/vault -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=test-root vault "$@"; }
consul() { "${KN[@]}" exec -i deploy/consul -- env CONSUL_HTTP_TOKEN=test-root consul "$@"; }
# in_docker <args>: curl from a container, where host.docker.internal names this machine.
in_docker() { docker run --rm --add-host=host.docker.internal:host-gateway curlimages/curl:latest "$@"; }

echo "== context $CONTEXT"
"${K[@]}" cluster-info >/dev/null 2>&1 || fail "no cluster at the context $CONTEXT"

echo "== the host's Ollama at $OLLAMA_ADDR"
TAGS=$(in_docker -sf -m 5 "http://$OLLAMA_ADDR/api/tags") ||
    fail "Ollama does not answer at $OLLAMA_ADDR (is it running, and does it listen on all interfaces?)"
MODELS=$(jq -r '[.models[].name] | unique | join(",")' <<<"$TAGS")
# The smallest model answers the check at the end, and is the example for the application.
SMALL=$(jq -r '.models | min_by(.size) | .name' <<<"$TAGS")
[[ -n "$MODELS" ]] || fail "Ollama at $OLLAMA_ADDR serves no model: ollama pull one first"
echo "     models: $MODELS"

echo "== images"
docker build -q --build-arg VERSION=local -t aisa:ci "$ROOT" >/dev/null
docker build -q -f "$ROOT/dev/Dockerfile" -t aisa-dev-tools:ci "$ROOT" >/dev/null
# The chart's test values pull nothing (imagePullPolicy Never), so the images go into the
# cluster's own store. Docker Desktop's Kubernetes and kind do not share the Docker engine's.
case "$CONTEXT" in
    docker-desktop)
        for img in aisa:ci aisa-dev-tools:ci; do
            docker save "$img" | docker exec -i desktop-control-plane ctr -n k8s.io images import - >/dev/null
        done ;;
    kind-*)
        kind load docker-image --name "${CONTEXT#kind-}" aisa:ci aisa-dev-tools:ci >/dev/null ;;
    *)
        echo "     (not Docker Desktop or kind: load aisa:ci and aisa-dev-tools:ci into the cluster yourself)" ;;
esac
ok "aisa:ci and aisa-dev-tools:ci built and loaded"

echo "== fixtures: Vault, Consul, mock backends"
"${K[@]}" create namespace "$NS" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
sed "s/NAMESPACE/$NS/" "$HERE/../test/fixtures.yaml" | "${KN[@]}" apply -f - >/dev/null
for d in vault consul mock-local mock-cloud; do
    "${KN[@]}" rollout status "deploy/$d" --timeout=180s >/dev/null
done
ok "ready"

echo "== Consul: policies, a quota profile, the backends"
consul acl policy create -name aisa \
    -rules 'key_prefix "aisa/" { policy = "write" } service "aisa-backend" { policy = "read" }' >/dev/null 2>&1 || true
consul acl policy create -name aisa-render \
    -rules 'service "aisa-backend" { policy = "read" } node_prefix "" { policy = "read" }' >/dev/null 2>&1 || true
consul kv put aisa/quotas/interactive "{\"tokens_per_hour\": $TOKENS_PER_HOUR}" >/dev/null
# The host's Ollama: local, so it fails open and has no key; a long timeout for CPU inference.
# The mock backends of the fixtures are not registered: the models are the host's.
"${KN[@]}" exec -i deploy/consul -- env CONSUL_HTTP_TOKEN=test-root \
    sh -c 'cat > /tmp/ollama.hcl && consul services register /tmp/ollama.hcl' >/dev/null <<HCL
services {
  id      = "ollama-host"
  name    = "aisa-backend"
  address = "${OLLAMA_ADDR%:*}"
  port    = ${OLLAMA_ADDR##*:}
  tags    = ["ollama", "local"]
  meta    = { provider = "openai-compatible", models = "$MODELS", fail_policy = "open", timeout = "600" }
  check { http = "http://$OLLAMA_ADDR/", interval = "10s", timeout = "2s" }
}
HCL
for _ in $(seq 1 30); do
    passing=$("${KN[@]}" exec deploy/consul -- wget -q -O - --header "X-Consul-Token: test-root" \
        "http://127.0.0.1:8500/v1/health/service/aisa-backend?passing" | jq length)
    [[ "$passing" -ge 1 ]] && break
    sleep 1
done
[[ "$passing" -ge 1 ]] || fail "ollama-host did not pass its health check in Consul"
ok "ollama-host registered and passing"

echo "== Vault: Kubernetes auth, the roles, the Consul secrets engine, the consumer"
vault auth enable kubernetes >/dev/null 2>&1 || true
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
vault write auth/kubernetes/role/aisa bound_service_account_names="$RELEASE" \
    bound_service_account_namespaces="$NS" audience=vault policies=aisa ttl=1h >/dev/null
vault write auth/kubernetes/role/aisa-render bound_service_account_names="$RELEASE-apisix" \
    bound_service_account_namespaces="$NS" audience=vault policies=aisa-render ttl=1h >/dev/null
vault secrets enable consul >/dev/null 2>&1 || true
vault write consul/config/access address=consul:8500 token=test-root >/dev/null
vault write consul/roles/aisa consul_policies=aisa ttl=1h >/dev/null
vault write consul/roles/aisa-render consul_policies=aisa-render ttl=1h >/dev/null
# The consumer's key: the plaintext stays in .local/ (git-ignored), Vault gets its hash. Until
# ROADMAP v0.2.0 PR 12, which has aisa generate the key, this is done here by hand.
KEYFILE="$ROOT/.local/$CONSUMER.key"
if [[ ! -s "$KEYFILE" ]]; then
    mkdir -p "$ROOT/.local"
    (umask 077; openssl rand -hex 24 > "$KEYFILE")
fi
KEY=$(cat "$KEYFILE")
HASH=$(printf %s "$KEY" | sha256sum | cut -d' ' -f1)
vault kv put "secret/aisa/consumers/$CONSUMER" key_sha256="$HASH" quota_profile=interactive >/dev/null
ok "consumer $CONSUMER (its key is in $KEYFILE)"

echo "== aisa's chart"
helm dependency update "$ROOT/deploy/helm/aisa" >/dev/null 2>&1
# The proxy as a LoadBalancer: Docker Desktop and kind with a load balancer publish it on
# localhost. Only the client-facing Service is published; metrics and aisa stay ClusterIPs.
helm --kube-context "$CONTEXT" upgrade --install "$RELEASE" "$ROOT/deploy/helm/aisa" -n "$NS" \
    -f "$HERE/../test/values-test.yaml" --set apisix.service.type=LoadBalancer \
    --wait --timeout 5m >/dev/null 2>&1 || {
    "${KN[@]}" get pods -o wide >&2
    fail "helm install"
}
ok "aisa, Redis and the proxy are ready"

echo "== the consumer's Secret in the namespace $APP_NS"
# What ROADMAP v0.2.0 PR 12 is to write; the same shape, written here by hand until then.
"${K[@]}" create namespace "$APP_NS" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null
"${K[@]}" -n "$APP_NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: aisa
  labels:
    aisa.hlan.net/consumer: $CONSUMER-$(printf %s "$CONSUMER" | sha256sum | cut -c1-8)
  annotations:
    aisa.hlan.net/consumer-name: $CONSUMER
stringData:
  OPENAI_BASE_URL: http://$RELEASE-apisix.$NS.svc.cluster.local/v1
  OPENAI_API_KEY: $KEY
EOF
ok "Secret aisa in $APP_NS: OPENAI_BASE_URL and OPENAI_API_KEY, for envFrom"

echo "== through the proxy on localhost"
for _ in $(seq 1 30); do
    status=$(curl -s -m 5 -o /dev/null -w '%{http_code}' http://localhost/v1/models || true)
    [[ "$status" == 200 ]] && break
    sleep 2
done
[[ "$status" == 200 ]] || fail "the proxy does not answer on http://localhost (no load balancer on localhost? use kubectl port-forward svc/$RELEASE-apisix 8080:80)"
ok "GET /v1/models: $(curl -s http://localhost/v1/models | jq -r '.data | length') models"
status=$(curl -s -m 180 -o /dev/null -w '%{http_code}' http://localhost/v1/chat/completions \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$SMALL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hi.\"}]}")
[[ "$status" == 200 ]] || fail "a chat completion with $SMALL answered $status"
ok "POST /v1/chat/completions with $SMALL: 200, through aisa, from the host's Ollama"

cat <<EOF

aisa is up. Applications in the cluster use the Secret; from this machine:

  export OPENAI_BASE_URL=http://localhost/v1
  export OPENAI_API_KEY=\$(cat $KEYFILE)

OpenCode (opencode.json), with AISA_API_KEY set from that file:

  "provider": {
    "aisa": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "aisa",
      "options": {"baseURL": "http://localhost/v1", "apiKey": "{env:AISA_API_KEY}"},
      "models": {"$SMALL": {"name": "$SMALL"}}
    }
  }

aisa's metrics:  kubectl --context $CONTEXT -n $NS port-forward svc/$RELEASE 8080:8080, then http://127.0.0.1:8080/metrics
Remove it all:   $HERE/down.sh
EOF
