#!/usr/bin/env bash
# Lints aisa's chart and the proxy's chart, renders them for the usual combinations of values,
# and validates every rendered object against the Kubernetes schemas (kubeconform; the
# ServiceMonitor against the CRD catalog, the Model CRD against the schema of a CRD, which has
# no strict standalone form). Needs helm and kubeconform; no cluster.
#
#   ./deploy/helm/test/charts_test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
AISA="$ROOT/deploy/helm/aisa"
PROXY="$ROOT/proxies/apisix/chart"
REQUIRED=(--set global.vault.addr=https://vault.vault.svc:8200 --set global.consul.addr=consul.consul.svc:8500)
# The oldest Kubernetes the charts support (Chart.yaml kubeVersion).
KUBE_VERSION=1.29.0

OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

# The charts read the proxy's files through links; a broken link would render an empty file.
for f in "$PROXY"/files/*; do
    [[ -s "$f" ]] || fail "$f is a broken link or empty"
done
echo "ok: the proxy chart's files resolve"

helm dependency update "$AISA" >/dev/null
helm lint --strict "$AISA" "${REQUIRED[@]}" >/dev/null 2>&1 || helm lint --strict "$AISA" "${REQUIRED[@]}"
helm lint --strict "$PROXY" "${REQUIRED[@]}" >/dev/null 2>&1 || helm lint --strict "$PROXY" "${REQUIRED[@]}"
echo "ok: helm lint"

# validate <name> <chart> [helm args...]
validate() {
    local name=$1 chart=$2
    shift 2
    helm template aisa "$chart" --namespace aisa --kube-version "$KUBE_VERSION" "${REQUIRED[@]}" "$@" \
        >"$OUT/$name.yaml" 2>/dev/null
    kubeconform -strict -summary -kubernetes-version "$KUBE_VERSION" \
        -schema-location default \
        -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
        -schema-location 'https://raw.githubusercontent.com/yannh/kubernetes-json-schema/master/{{.NormalizedKubernetesVersion}}/{{.ResourceKind}}{{.KindSuffix}}.json' \
        "$OUT/$name.yaml"
    echo "ok: $name"
}

validate "defaults" "$AISA" --include-crds
validate "no-kubernetes" "$AISA" --set kubernetes.enabled=false
validate "monitoring" "$AISA" --set serviceMonitor.enabled=true --set apisix.serviceMonitor.enabled=true
validate "consul-without-acls" "$AISA" --set consul.token.fromVault=false --set apisix.vaultAgent.consulToken.enabled=false
validate "external-redis" "$AISA" --set redis.enabled=false --set redis.addr=redis.example.svc:6379
validate "no-quotas-no-proxy" "$AISA" --set quotas.enabled=false --set apisix.enabled=false
validate "proxy-alone" "$PROXY" --set aisa.url=https://aisa.aisa.svc:8080

# The values that must be given are asked for.
if helm template aisa "$AISA" >/dev/null 2>&1; then
    fail "the chart renders without global.vault.addr"
fi
echo "ok: global.vault.addr is required"

# The proxy finds aisa's Service under the name aisa's chart gives it: by the release name, or
# by global.aisa.fullnameOverride.
for case in "aisa" "prod" "aisa --set global.aisa.fullnameOverride=inference"; do
    read -r release extra <<<"$case"
    # shellcheck disable=SC2086 # $extra is a list of helm arguments
    helm template $release "$AISA" "${REQUIRED[@]}" $extra >"$OUT/names.yaml" 2>/dev/null
    svc=$(awk '/^kind: Service$/{s=1} s && /^  name:/{print $2; s=0}' "$OUT/names.yaml" | grep -v -e apisix -e redis -e metrics)
    port=$(awk -v svc="$svc" '/^kind:/{s = ($2 == "Service"); f = 0} s && /^  name:/ && $2 == svc {f=1} f && /port: /{print; exit}' "$OUT/names.yaml" |
        sed -E 's/.*port: ([0-9]+).*/\1/')
    url=$(awk '/name: AISA_DECIDE_URI/{getline; print $2; exit}' "$OUT/names.yaml" | tr -d '"')
    [[ "${url#*://}" == "$svc:$port/v1/decide" ]] ||
        fail "$case: the proxy asks $url, aisa's Service is $svc:$port"
    echo "ok: $case: the proxy asks aisa's Service $svc:$port"
done

# A Service type that publishes the proxy does not publish its metrics, or aisa.
helm template aisa "$AISA" "${REQUIRED[@]}" --set apisix.service.type=LoadBalancer >"$OUT/lb.yaml" 2>/dev/null
types=$(awk '/^kind: Service$/{s=1; n=""} s && /^  name:/{n=$2} s && /^  type:/{print n, $2; s=0}' "$OUT/lb.yaml" | LC_ALL=C sort | tr '\n' ' ')
[[ "$types" == "aisa ClusterIP aisa-apisix LoadBalancer aisa-apisix-metrics ClusterIP aisa-metrics ClusterIP aisa-redis ClusterIP " ]] ||
    fail "Service types with a published proxy: $types"
echo "ok: only the proxy's client Service is published: $types"

# Nothing but the proxy reaches aisa by default.
from=$(awk '/^kind:/{s = ($2 == "NetworkPolicy"); f = 0} s && /^  name: aisa$/{f=1} f && /^ *- (podSelector|namespaceSelector|ipBlock):/{c++} END {print c + 0}' \
    "$OUT/defaults.yaml")
[[ "$from" == 1 ]] || fail "aisa's NetworkPolicy has $from peers by default, want only the proxy"
echo "ok: by default only the proxy reaches aisa"

# A peer in networkPolicy.extraFrom, such as Prometheus, reaches aisa's metrics port only, never
# the port of /v1/decide and /v1/usage (#46). rules prints each ingress rule of aisa's
# NetworkPolicy as "<ports> <peers>".
rules() {
    local file=$1
    awk '/^kind:/{s = ($2 == "NetworkPolicy"); f = 0} s && /^  name: aisa$/{f=1}
         f && /^    - ports:/{if (r) print p, q; r = 1; p = ""; q = ""}
         f && r && /port: /{sub(/.*port: /, ""); sub(/,.*/, ""); p = p $0 "/"}
         f && r && /component: proxy/{q = q "proxy/"}
         f && r && /namespaceSelector:/{q = q "namespace/"}
         END {if (r) print p, q}' "$file"
}
helm template aisa "$AISA" "${REQUIRED[@]}" \
    --set-json 'networkPolicy.extraFrom=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]' \
    >"$OUT/extra.yaml" 2>/dev/null
got=$(rules "$OUT/extra.yaml" | tr '\n' ';')
[[ "$got" == "http/ proxy/;metrics/ namespace/;" ]] ||
    fail "aisa's NetworkPolicy rules with extraFrom: $got; want the proxy on http and the peer on metrics only"
echo "ok: a peer in extraFrom reaches aisa's metrics port only"
got=$(rules "$OUT/defaults.yaml" | tr '\n' ';')
[[ "$got" == "http/ proxy/;" ]] || fail "aisa's NetworkPolicy rules by default: $got"
echo "ok: by default no rule for the metrics port"

# The URL that aisa writes into the consumers' Secrets and the Models is the proxy's client
# Service of the release, whatever its name and port.
for case in "aisa" "prod --set apisix.fullnameOverride=gateway" "aisa --set apisix.service.port=8080"; do
    read -r release extra <<<"$case"
    # shellcheck disable=SC2086 # $extra is a list of helm arguments
    helm template $release "$AISA" --namespace llm "${REQUIRED[@]}" $extra >"$OUT/url.yaml" 2>/dev/null
    # The proxy's client Service: component proxy, and not its metrics.
    svc=$(awk '/^kind:/{s = ($2 == "Service"); n = ""} s && /^  name:/{n = $2} s && /component: proxy/ && n !~ /-metrics$/ {print n; s = 0}' "$OUT/url.yaml")
    port=$(awk -v svc="$svc" '/^kind:/{s = ($2 == "Service"); f = 0} s && /^  name:/ && $2 == svc {f=1} f && /port: /{print; exit}' "$OUT/url.yaml" |
        sed -E 's/.*port: ([0-9]+).*/\1/')
    url=$(awk '/name: AISA_PROXY_URL/{getline; print $2; exit}' "$OUT/url.yaml" | tr -d '"')
    want="http://$svc.llm.svc.cluster.local/v1"
    [[ "$port" == 80 ]] || want="http://$svc.llm.svc.cluster.local:$port/v1"
    [[ "$url" == "$want" ]] || fail "$case: aisa gives applications $url, the proxy's Service is $svc:$port"
    echo "ok: $case: applications get the proxy's Service, $url"
done
helm template aisa "$AISA" "${REQUIRED[@]}" --set kubernetes.proxyURL=https://llm.example.internal/v1 >"$OUT/url.yaml" 2>/dev/null
grep -A1 'name: AISA_PROXY_URL' "$OUT/url.yaml" | grep -q '"https://llm.example.internal/v1"' || fail "kubernetes.proxyURL is not used"
echo "ok: kubernetes.proxyURL replaces the proxy's Service"

# Without kubernetes.enabled aisa gets no rights in the API and no token for it. With it, the
# token is mounted into aisa's container only, never automatically into the Vault Agent's: the
# service account may read every Secret.
for f in no-kubernetes defaults; do
    roles=$(grep -c '^kind: \(Cluster\)\?Role\(Binding\)\?$' "$OUT/$f.yaml" || true)
    automount=$(awk '/^kind:/{d = ($2 == "Deployment"); a = 0} d && /^  name: aisa$/{a = 1} a && /automountServiceAccountToken:/{print $2; exit}' "$OUT/$f.yaml")
    # The containers of aisa's Deployment that mount the API token, by name.
    mounts=$(awk '/^kind:/{d = ($2 == "Deployment"); a = 0} d && /^  name: aisa$/{a = 1}
                  a && /^        - name: /{c = $3} a && /mountPath: \/var\/run\/secrets\/kubernetes.io\/serviceaccount/{print c}' "$OUT/$f.yaml" | tr '\n' ' ')
    case "$f" in
        no-kubernetes) [[ "$roles" == 0 && "$automount" == false && -z "$mounts" ]] || fail "$f: $roles RBAC objects, automount $automount, token in: $mounts" ;;
        defaults) [[ "$roles" == 6 && "$automount" == false && "$mounts" == "aisa " ]] || fail "$f: $roles RBAC objects, automount $automount, token in: $mounts" ;;
        *) fail "no expectation for $f" ;;
    esac
    echo "ok: $f: $roles RBAC objects, the API token in: ${mounts:-no container}"
done
grep -q '^kind: CustomResourceDefinition$' "$OUT/defaults.yaml" || fail "the Model CRD is not in the chart"
echo "ok: the Model CRD is in the chart"

echo "ok: all chart tests passed"
