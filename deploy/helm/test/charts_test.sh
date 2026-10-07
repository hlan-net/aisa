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
REQUIRED=(--set global.vault.addr=http://vault.vault.svc:8200 --set global.consul.addr=consul.consul.svc:8500)
# The oldest Kubernetes the charts support (Chart.yaml kubeVersion).
KUBE_VERSION=1.29.0

OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT
fail() { echo "FAIL: $1" >&2; exit 1; }

# The charts read the proxy's files through links; a broken link would render an empty file.
for f in "$PROXY"/files/*; do
    [ -s "$f" ] || fail "$f is a broken link or empty"
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
validate "proxy-alone" "$PROXY" --set aisa.url=http://aisa.aisa.svc:8080

# The values that must be given are asked for.
if helm template aisa "$AISA" >/dev/null 2>&1; then
    fail "the chart renders without global.vault.addr"
fi
echo "ok: global.vault.addr is required"

# The proxy finds aisa's Service under the name aisa's chart gives it.
for release in aisa prod; do
    svc=$(helm template "$release" "$AISA" "${REQUIRED[@]}" 2>/dev/null |
        awk '/^kind: Service$/{s=1} s && /^  name:/{print $2; s=0}' | grep -v -e apisix -e redis)
    url=$(helm template "$release" "$AISA" "${REQUIRED[@]}" 2>/dev/null |
        awk '/name: AISA_DECIDE_URI/{getline; print $2; exit}' | tr -d '"')
    [ "$url" = "http://$svc:8080/v1/decide" ] || fail "release $release: the proxy asks $url, aisa's Service is $svc"
    echo "ok: release $release: the proxy asks aisa's Service $svc"
done

echo "ok: all chart tests passed"
