#!/usr/bin/env bash
# Removes what up.sh installed: the release, the consumer's Secret, the fixtures and their
# ClusterRoleBinding, and the namespaces up.sh created. A namespace that existed before up.sh
# (no label aisa.hlan.net/local-setup) is left in place with whatever else is in it. The
# consumer's key in .local/ stays.
#
#   ./deploy/helm/local/down.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTEXT="${CONTEXT:-$(kubectl config current-context)}"
NS="${NAMESPACE:-aisa}"
RELEASE="${RELEASE:-aisa}"
CONSUMER="${CONSUMER:-opencode}"
APP_NS="${APP_NAMESPACE:-$CONSUMER}"
K=(kubectl --context "$CONTEXT")

owned() { [[ "$("${K[@]}" get namespace "$1" -o jsonpath='{.metadata.labels.aisa\.hlan\.net/local-setup}' 2>/dev/null)" == true ]]; }

helm --kube-context "$CONTEXT" uninstall "$RELEASE" -n "$NS" --wait --timeout 2m >/dev/null 2>&1 || true
"${K[@]}" delete clusterrolebinding aisa-test-vault-auth-delegator --ignore-not-found >/dev/null
for ns in "$APP_NS" "$NS"; do
    "${K[@]}" get namespace "$ns" >/dev/null 2>&1 || continue
    if owned "$ns"; then
        "${K[@]}" delete namespace "$ns" --wait=true >/dev/null
        echo "removed the namespace $ns"
    else
        # Not ours: take out only what up.sh put in.
        if [[ "$ns" == "$NS" ]]; then
            sed "s/NAMESPACE/$NS/" "$HERE/../test/fixtures.yaml" | "${K[@]}" -n "$NS" delete --ignore-not-found -f - >/dev/null
        fi
        if [[ "$ns" == "$APP_NS" ]]; then
            "${K[@]}" -n "$APP_NS" delete secret aisa --ignore-not-found >/dev/null
        fi
        echo "left the namespace $ns, which existed before up.sh; removed only this setup's objects"
    fi
done
echo "removed the release $RELEASE from $CONTEXT"
