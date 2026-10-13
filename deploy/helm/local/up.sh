#!/usr/bin/env bash
# aisa on a local cluster, with the Ollama of the machine as its backend: for trying the whole
# path on a laptop, from an application's Secret to a model the host runs.
#
#   ./deploy/helm/local/up.sh
#
# It uses the current kubectl context (CONTEXT overrides it): Docker Desktop's Kubernetes or a
# kind cluster. It builds aisa's images and loads them into the cluster, applies the test
# fixtures (a dev-mode Vault with Kubernetes auth, a dev-mode Consul with ACLs, two mock
# backends), registers the host's Ollama in Consul with every model it serves, declares one
# consumer with the namespace of its application, and installs aisa's chart with the proxy
# published on localhost. aisa then issues the consumer's key and writes it into a Secret in
# that namespace, and a Model object per model (KUBERNETES_OBJECTS.md).
#
# Nothing of this is for a real cluster: the fixtures keep everything in memory and have fixed
# root tokens. ./down.sh removes it all. Needs docker, kubectl, helm, curl and jq.
#
#   CONTEXT        kubectl context                          (the current one)
#   NAMESPACE      aisa's namespace                         aisa
#   RELEASE        the Helm release                         aisa
#   OLLAMA_ADDR    Ollama as the cluster reaches it         host.docker.internal:11434
#   CONSUMER       the consumer to create                   opencode
#   APP_NAMESPACE  where its Secret goes                    $CONSUMER
#   TOKENS_PER_HOUR its quota                               2000000
#   PUBLISH        how the proxy reaches localhost: "lb" (a LoadBalancer on localhost, Docker
#                  Desktop) or "forward" (a ClusterIP and kubectl port-forward, kind)
#                                                           by the context
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
case "${PUBLISH:-}" in
    lb|forward) ;;
    "") case "$CONTEXT" in docker-desktop) PUBLISH=lb ;; *) PUBLISH=forward ;; esac ;;
    *) echo "PUBLISH: want lb or forward, got '$PUBLISH'" >&2; exit 1 ;;
esac
# The consumer's name is a path in Consul and Vault.
[[ "$CONSUMER" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "CONSUMER: letters, digits, '.', '_' and '-' only" >&2; exit 1; }
K=(kubectl --context "$CONTEXT")
KN=("${K[@]}" -n "$NS")
LABEL=aisa.hlan.net/local-setup
# aisa's Service and ServiceAccount, by the chart's rule (aisa.fullname): the release's name if it
# has "aisa" in it, else <release>-aisa. The proxy's are always <release>-apisix.
if [[ "$RELEASE" == *aisa* ]]; then FULLNAME=$RELEASE; else FULLNAME=$RELEASE-aisa; fi
FULLNAME=$(printf %s "$FULLNAME" | cut -c1-63 | sed 's/-*$//')

ok() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
pf_pid=""
cleanup() { [[ -n "$pf_pid" ]] && kill "$pf_pid" 2>/dev/null; true; }
trap cleanup EXIT
# owned_ns <name>: whether up.sh created the namespace, and everything in it is this setup's.
owned_ns() { local ns=$1; [[ "$("${K[@]}" get namespace "$ns" -o jsonpath='{.metadata.labels.aisa\.hlan\.net/local-setup}' 2>/dev/null)" == true ]]; }
# ensure_ns <name>: creates the namespace if it is missing, labelled as this setup's, so that
# down.sh removes only what up.sh created.
ensure_ns() {
    local ns=$1
    "${K[@]}" get namespace "$ns" >/dev/null 2>&1 && return 0
    "${K[@]}" create namespace "$ns" >/dev/null
    "${K[@]}" label namespace "$ns" aisa.hlan.net/local-setup=true >/dev/null
}
# fixtures: the install test's fixtures, for this namespace.
fixtures() { sed "s/NAMESPACE/$NS/" "$HERE/../test/fixtures.yaml"; }
# foreign: of the objects on stdin, those that exist and are not this setup's: without its label
# (in a namespace up.sh did not create), or, for the cluster-wide ClusterRoleBinding, bound to
# another namespace's Vault. apply would take them over, and down.sh would delete them.
foreign() {
    local owned=false
    owned_ns "$NS" && owned=true
    "${KN[@]}" get -f - --ignore-not-found -o json | jq -r --arg l "$LABEL" --arg ns "$NS" --argjson owned "$owned" '
        (.items // [.])[] | select(.kind != null)
        | select(if .kind == "ClusterRoleBinding"
                 then ([.subjects[]?.namespace] | index($ns) | not) or (($owned | not) and .metadata.labels[$l] != "true")
                 else ($owned | not) and .metadata.labels[$l] != "true" end)
        | "\(.kind)/\(.metadata.name)"'
}
vault() { "${KN[@]}" exec -i deploy/vault -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=test-root vault "$@"; }
consul() { "${KN[@]}" exec -i deploy/consul -- env CONSUL_HTTP_TOKEN=test-root consul "$@"; }
# in_docker <args>: curl from a container, where host.docker.internal names this machine.
in_docker() { docker run --rm --add-host=host.docker.internal:host-gateway curlimages/curl:latest "$@"; }

echo "== context $CONTEXT"
"${K[@]}" cluster-info >/dev/null 2>&1 || fail "no cluster at the context $CONTEXT"

echo "== the host's Ollama at $OLLAMA_ADDR"
TAGS=$(in_docker -sf -m 5 "http://$OLLAMA_ADDR/api/tags") ||
    fail "Ollama does not answer at $OLLAMA_ADDR (is it running, and does it listen on all interfaces?)"
# What Consul and the proxy get is an address every pod can reach. host.docker.internal is a
# name Docker gives containers it starts with that mapping; a kind node on Linux has no such
# name, so it is turned into the host's address on Docker's network here.
OLLAMA_HOST=${OLLAMA_ADDR%:*}
OLLAMA_PORT=${OLLAMA_ADDR##*:}
if [[ "$OLLAMA_HOST" == host.docker.internal ]]; then
    OLLAMA_HOST=$(docker run --rm --add-host=host.docker.internal:host-gateway busybox \
        awk '$2 == "host.docker.internal" {print $1; exit}' /etc/hosts)
    [[ -n "$OLLAMA_HOST" ]] || fail "could not find the host's address on Docker's network"
    echo "     as the cluster reaches it: $OLLAMA_HOST:$OLLAMA_PORT"
fi
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
ensure_ns "$NS"
taken=$(fixtures | foreign)
[[ -z "$taken" ]] || fail "these exist already and are not this setup's (another NAMESPACE, or remove them):" $taken
fixtures | "${KN[@]}" apply -f - >/dev/null
# The label is what down.sh deletes by, in a namespace up.sh did not create.
fixtures | "${KN[@]}" label -f - --overwrite "$LABEL=true" >/dev/null
for d in vault consul mock-local mock-cloud; do
    "${KN[@]}" rollout status "deploy/$d" --timeout=180s >/dev/null
done
ok "ready"

echo "== Consul: policies, a quota profile, the backends"
consul acl policy create -name aisa \
    -rules 'key_prefix "aisa/" { policy = "write" } service "aisa-backend" { policy = "read" } node_prefix "" { policy = "read" }' >/dev/null 2>&1 ||
    consul acl policy update -name aisa \
        -rules 'key_prefix "aisa/" { policy = "write" } service "aisa-backend" { policy = "read" } node_prefix "" { policy = "read" }' >/dev/null
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
  address = "$OLLAMA_HOST"
  port    = $OLLAMA_PORT
  tags    = ["ollama", "local"]
  meta    = { provider = "openai-compatible", models = "$MODELS", fail_policy = "open", timeout = "600" }
  check { http = "http://$OLLAMA_HOST:$OLLAMA_PORT/", interval = "10s", timeout = "2s" }
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
path "secret/data/aisa/consumers/*" { capabilities = ["create", "read", "update"] }
path "secret/metadata/aisa/consumers" { capabilities = ["list"] }
path "secret/metadata/aisa/consumers/*" { capabilities = ["list"] }
path "consul/creds/aisa" { capabilities = ["read"] }
HCL
vault policy write aisa-render - >/dev/null <<'HCL'
path "secret/data/aisa/providers/*" { capabilities = ["read"] }
path "consul/creds/aisa-render" { capabilities = ["read"] }
HCL
vault write auth/kubernetes/role/aisa bound_service_account_names="$FULLNAME" \
    bound_service_account_namespaces="$NS" audience=vault policies=aisa ttl=1h >/dev/null
vault write auth/kubernetes/role/aisa-render bound_service_account_names="$RELEASE-apisix" \
    bound_service_account_namespaces="$NS" audience=vault policies=aisa-render ttl=1h >/dev/null
vault secrets enable consul >/dev/null 2>&1 || true
vault write consul/config/access address=consul:8500 token=test-root >/dev/null
vault write consul/roles/aisa consul_policies=aisa ttl=1h >/dev/null
vault write consul/roles/aisa-render consul_policies=aisa-render ttl=1h >/dev/null
ok "Vault configured"

echo "== the consumer $CONSUMER"
# Declared in Consul with the namespace and Secret of its application. aisa issues its key and
# writes the Secret; it never takes over a Secret of that name that is not its own.
ensure_ns "$APP_NS"
# A Secret of that name is this setup's only with aisa's labels, this install's namespace and this
# consumer's exact name: aisa leaves any other alone, and its key would not be this consumer's.
taken=$("${K[@]}" -n "$APP_NS" get secret aisa --ignore-not-found -o json |
    jq -r --arg ns "$NS" --arg consumer "$CONSUMER" 'select(.metadata.labels["app.kubernetes.io/managed-by"] != "aisa"
        or .metadata.labels["aisa.hlan.net/install"] != $ns
        or .metadata.annotations["aisa.hlan.net/consumer-name"] != $consumer) | .metadata.name')
[[ -z "$taken" ]] || fail "the namespace $APP_NS has a Secret aisa that is not this consumer's of this install (another APP_NAMESPACE, or remove it)"
consul kv put "aisa/consumers/$CONSUMER" "$(jq -cn --arg ns "$APP_NS" '{quota_profile: "interactive", namespace: $ns, secret: "aisa"}')" >/dev/null
ok "consumer $CONSUMER declared, its Secret to be aisa in $APP_NS"

echo "== aisa's chart"
helm dependency update "$ROOT/deploy/helm/aisa" >/dev/null 2>&1
# PUBLISH=lb: the proxy as a LoadBalancer, which Docker Desktop puts on localhost:80. Only the
# client-facing Service is published; metrics and aisa stay ClusterIPs. PUBLISH=forward: a
# ClusterIP, reached with kubectl port-forward (a plain kind cluster has no load balancer).
if [[ "$PUBLISH" == lb ]]; then service_type=LoadBalancer; BASE_URL=http://localhost; else service_type=ClusterIP; BASE_URL=http://localhost:18080; fi
helm --kube-context "$CONTEXT" upgrade --install "$RELEASE" "$ROOT/deploy/helm/aisa" -n "$NS" \
    -f "$HERE/../test/values-test.yaml" --set "apisix.service.type=$service_type" \
    --wait --timeout 5m >/dev/null 2>&1 || {
    "${KN[@]}" get pods -o wide >&2
    fail "helm install"
}
ok "aisa, Redis and the proxy are ready"

echo "== the consumer's Secret and the Model objects, written by aisa"
for _ in $(seq 1 60); do
    KEY=$("${K[@]}" -n "$APP_NS" get secret aisa -o jsonpath='{.data.OPENAI_API_KEY}' 2>/dev/null | base64 -d || true)
    [[ -n "$KEY" ]] && break
    sleep 1
done
[[ -n "$KEY" ]] || fail "aisa wrote no Secret aisa in $APP_NS: see kubectl -n $NS logs deploy/$FULLNAME"
ok "Secret aisa in $APP_NS: OPENAI_BASE_URL and OPENAI_API_KEY, for envFrom"
ok "Model objects: $("${K[@]}" get models.aisa.hlan.net -o jsonpath='{range .items[*]}{.spec.model} {end}')"

echo "== through the proxy at $BASE_URL"
if [[ "$PUBLISH" == forward ]]; then
    "${KN[@]}" port-forward "svc/$RELEASE-apisix" 18080:80 >/dev/null 2>&1 &
    pf_pid=$!
fi
for _ in $(seq 1 30); do
    status=$(curl -s -m 5 -o /dev/null -w '%{http_code}' "$BASE_URL/v1/models" || true)
    [[ "$status" == 200 ]] && break
    sleep 2
done
[[ "$status" == 200 ]] || fail "the proxy does not answer at $BASE_URL (PUBLISH=$PUBLISH)"
ok "GET /v1/models: $(curl -s "$BASE_URL/v1/models" | jq -r '.data | length') models"
status=$(curl -s -m 180 -o /dev/null -w '%{http_code}' "$BASE_URL/v1/chat/completions" \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$SMALL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hi.\"}]}")
[[ "$status" == 200 ]] || fail "a chat completion with $SMALL answered $status"
ok "POST /v1/chat/completions with $SMALL: 200, through aisa, from the host's Ollama"

cleanup
pf_pid=""
cat <<EOF

aisa is up. Applications in the cluster use the Secret; from this machine:
EOF
if [[ "$PUBLISH" == forward ]]; then cat <<EOF

  kubectl --context $CONTEXT -n $NS port-forward svc/$RELEASE-apisix 18080:80 &   # keep it running
EOF
fi
cat <<EOF

  export OPENAI_BASE_URL=$BASE_URL/v1
  export OPENAI_API_KEY=\$(kubectl --context $CONTEXT -n $APP_NS get secret aisa -o jsonpath='{.data.OPENAI_API_KEY}' | base64 -d)

The key is aisa's: it replaces it every 30 days (kubernetes.keys in the chart's values), so read it
from the Secret again then. OpenCode (opencode.json), with AISA_API_KEY set the same way:

  "provider": {
    "aisa": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "aisa",
      "options": {"baseURL": "$BASE_URL/v1", "apiKey": "{env:AISA_API_KEY}"},
      "models": {"$SMALL": {"name": "$SMALL"}}
    }
  }

aisa's metrics:  kubectl --context $CONTEXT -n $NS port-forward svc/$FULLNAME-metrics 9090:9090, then http://127.0.0.1:9090/metrics
Remove it all:   $HERE/down.sh
EOF
