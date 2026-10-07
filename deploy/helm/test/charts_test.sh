#!/usr/bin/env bash
# Lints aisa's chart and the proxy's chart, renders them for the usual combinations of values,
# and validates every rendered object against the Kubernetes schemas (kubeconform; the
# ServiceMonitor against the CRD catalog). Needs helm and kubeconform; no cluster.
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
        "$OUT/$name.yaml"
    echo "ok: $name"
}

validate "defaults" "$AISA"
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
    svc=$(awk '/^kind: Service$/{s=1} s && /^  name:/{print $2; s=0}' "$OUT/names.yaml" | grep -v -e apisix -e redis)
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
[[ "$types" == "aisa ClusterIP aisa-apisix LoadBalancer aisa-apisix-metrics ClusterIP aisa-redis ClusterIP " ]] ||
    fail "Service types with a published proxy: $types"
echo "ok: only the proxy's client Service is published: $types"

# Nothing but the proxy reaches aisa by default.
from=$(awk '/^kind:/{s = ($2 == "NetworkPolicy"); f = 0} s && /^  name: aisa$/{f=1} f && /^ *- (podSelector|namespaceSelector|ipBlock):/{c++} END {print c + 0}' \
    "$OUT/defaults.yaml")
[[ "$from" == 1 ]] || fail "aisa's NetworkPolicy has $from peers by default, want only the proxy"
echo "ok: by default only the proxy reaches aisa"

echo "ok: all chart tests passed"
