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

validate "defaults" "$AISA"
validate "no-kubernetes" "$AISA" --set kubernetes.enabled=false
validate "monitoring" "$AISA" --set serviceMonitor.enabled=true --set apisix.serviceMonitor.enabled=true \
    --set prometheusRule.enabled=true
validate "consul-without-acls" "$AISA" --set consul.token.fromVault=false --set apisix.vaultAgent.consulToken.enabled=false
validate "external-redis" "$AISA" --set redis.enabled=false --set redis.addr=redis.example.svc:6379
validate "no-quotas-no-proxy" "$AISA" --set quotas.enabled=false --set apisix.enabled=false \
    --set kubernetes.proxyURL=https://llm.example.internal/v1
validate "admin-ingress" "$AISA" --set admin.enabled=true --set admin.ingress.enabled=true
grep -q '^kind: Ingress$' "$OUT/admin-ingress.yaml" || fail "admin-ingress case does not render the Ingress"
validate "proxy-alone" "$PROXY" --set aisa.url=https://aisa.aisa.svc:8080

# The values that must be given are asked for.
if helm template aisa "$AISA" >/dev/null 2>&1; then
    fail "the chart renders without global.vault.addr"
fi
echo "ok: global.vault.addr is required"

# Without the proxy's chart there is no Service to derive the proxy's URL from.
if helm template aisa "$AISA" "${REQUIRED[@]}" --set apisix.enabled=false >/dev/null 2>&1; then
    fail "the chart renders without the proxy's chart and kubernetes.proxyURL"
fi
helm template aisa "$AISA" "${REQUIRED[@]}" --set apisix.enabled=false --set kubernetes.enabled=false >/dev/null ||
    fail "the chart needs kubernetes.proxyURL with kubernetes.enabled=false"
echo "ok: kubernetes.proxyURL is required without the proxy's chart"

# The token that aisa and its Vault Agent log in to Vault with has Vault's audience, so that the
# Kubernetes API does not accept it from the Vault Agent: the service account may read Secrets.
awk '/^kind:/{d = ($2 == "Deployment"); a = 0} d && /^  name: aisa$/{a = 1}
     a && /- name: serviceaccount$/{v = 1} v && /audience: "vault"/{f = 1; exit} END {exit !f}' "$OUT/defaults.yaml" ||
    fail "the Vault token is not projected with the audience vault by default"
if helm template aisa "$AISA" "${REQUIRED[@]}" --set vault.auth.audience= >/dev/null 2>&1; then
    fail "the chart renders a Vault token of the API server's audience with kubernetes.enabled"
fi
helm template aisa "$AISA" "${REQUIRED[@]}" --set vault.auth.audience= --set kubernetes.enabled=false >/dev/null ||
    fail "the chart needs vault.auth.audience with kubernetes.enabled=false"
echo "ok: the Vault token has Vault's audience, required with kubernetes.enabled"

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

# The Models wait out an empty catalog as long as the proxy's guard keeps its model routes.
grace() { local file=$1; awk '/name: AISA_MODELS_EMPTY_GRACE/{getline; print $2; exit}' "$file" | tr -d '"'; }
[[ "$(grace "$OUT/defaults.yaml")" == 300s ]] || fail "AISA_MODELS_EMPTY_GRACE by default: $(grace "$OUT/defaults.yaml")"
helm template aisa "$AISA" "${REQUIRED[@]}" --set apisix.consulTemplate.emptyCatalogGrace=60 >"$OUT/grace.yaml" 2>/dev/null
[[ "$(grace "$OUT/grace.yaml")" == 60s ]] || fail "AISA_MODELS_EMPTY_GRACE does not follow the proxy's grace: $(grace "$OUT/grace.yaml")"
echo "ok: the Models' empty-catalog grace follows the proxy's"

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
# The CRD is a template, so that an upgrade applies it, and kept when the chart is uninstalled.
awk '/^kind:/{c = ($2 == "CustomResourceDefinition")} c && /helm.sh\/resource-policy: keep/{f = 1} END {exit !f}' "$OUT/defaults.yaml" ||
    fail "the Model CRD is not in the chart's templates, or not kept"
helm template aisa "$AISA" "${REQUIRED[@]}" --set crds.install=false >"$OUT/no-crds.yaml" 2>/dev/null
! grep -q '^kind: CustomResourceDefinition$' "$OUT/no-crds.yaml" || fail "crds.install=false still renders the CRD"
# Admin interface is off by default, and turned on with admin.enabled=true.
! grep -q 'name: AISA_ADMIN_ENABLED' "$OUT/defaults.yaml" || fail "AISA_ADMIN_ENABLED is set by default"
helm template aisa "$AISA" "${REQUIRED[@]}" --set admin.enabled=true >"$OUT/admin.yaml" 2>/dev/null
admin_env=$(awk '/name: AISA_ADMIN_ENABLED/{getline; print $2; exit}' "$OUT/admin.yaml" | tr -d '"')
[[ "$admin_env" == "true" ]] || fail "admin.enabled=true did not set AISA_ADMIN_ENABLED=true: got '$admin_env'"
echo "ok: admin.enabled=true sets AISA_ADMIN_ENABLED=true in deployment"

# The alerts of docs/features/usage-metrics.md ship as a PrometheusRule, on aisa's metrics only;
# AisaDown watches the job of the metrics Service. The groups of a feature that is off are left out.
alerts() { awk '/^kind:/{r = ($2 == "PrometheusRule")} r && /- alert: /{print $3}' "$1" | LC_ALL=C sort | tr '\n' ' '; }
want="AisaBudgetExhausted AisaBudgetSoftLimit AisaConfigEmpty AisaConfigHeld AisaConfigRenderRejected AisaConsumerDeclarationInvalid AisaConsumerSecretFailing AisaConsumersStale AisaConsumersUnreadable AisaDown AisaKubeNoLeader AisaKubeWritesFailing AisaModelUnavailable AisaQuotaCountersFailing AisaQuotaProfileInvalid AisaSlowDecisions AisaSlowFirstToken AisaUsageEventsRejected AisaUsageMissing AisaUsageRequestsRejected "
[[ "$(alerts "$OUT/monitoring.yaml")" == "$want" ]] || fail "alerts in the PrometheusRule: $(alerts "$OUT/monitoring.yaml")"
grep -q 'up{job="aisa-metrics",namespace="aisa"}' "$OUT/monitoring.yaml" || fail "AisaDown does not watch the job aisa-metrics"
grep -qE '^ +expr: .*(apisix|consul_)' "$OUT/monitoring.yaml" && fail "an alert depends on a gateway or Consul metric"
! grep -q '^kind: PrometheusRule$' "$OUT/defaults.yaml" || fail "the PrometheusRule renders by default"
helm template aisa "$AISA" "${REQUIRED[@]}" --set prometheusRule.enabled=true --set kubernetes.enabled=false --set quotas.enabled=false \
    >"$OUT/rule-min.yaml" 2>/dev/null
case "$(alerts "$OUT/rule-min.yaml")" in
    *AisaKube*|*AisaConsumerSecret*|*AisaModel*|*AisaQuota*) fail "alerts of features that are off: $(alerts "$OUT/rule-min.yaml")" ;;
esac
echo "ok: the PrometheusRule has the documented alerts, on aisa's metrics only"

echo "ok: all chart tests passed"
