#!/usr/bin/env bash
# Removes what up.sh installed: the release, the fixtures, both namespaces and the
# ClusterRoleBinding of the fixtures. The consumer's key in .local/ stays.
#
#   ./deploy/helm/local/down.sh
set -euo pipefail

CONTEXT="${CONTEXT:-$(kubectl config current-context)}"
NS="${NAMESPACE:-aisa}"
RELEASE="${RELEASE:-aisa}"
CONSUMER="${CONSUMER:-opencode}"
APP_NS="${APP_NAMESPACE:-$CONSUMER}"
K=(kubectl --context "$CONTEXT")

helm --kube-context "$CONTEXT" uninstall "$RELEASE" -n "$NS" --wait --timeout 2m >/dev/null 2>&1 || true
"${K[@]}" delete clusterrolebinding aisa-test-vault-auth-delegator --ignore-not-found >/dev/null
"${K[@]}" delete namespace "$APP_NS" "$NS" --ignore-not-found --wait=true >/dev/null
echo "removed the release $RELEASE and the namespaces $NS and $APP_NS from $CONTEXT"
