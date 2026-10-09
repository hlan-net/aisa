#!/usr/bin/env bash
# Removes what up.sh installed: the release, the consumer's Secret, the fixtures and their
# ClusterRoleBinding, and the namespaces up.sh created. Namespaces and objects that up.sh did not
# create (no label aisa.hlan.net/local-setup) are left in place. The consumer's key in .local/
# stays.
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
LABEL=aisa.hlan.net/local-setup

# ours: of the fixtures, those that are this setup's: labelled by up.sh, or in a namespace up.sh
# created; the ClusterRoleBinding only while it binds this namespace's Vault.
ours() {
    local owned=false
    owned "$NS" && owned=true
    sed "s/NAMESPACE/$NS/" "$HERE/../test/fixtures.yaml" |
        "${K[@]}" -n "$NS" get -f - --ignore-not-found -o json | jq -r --arg l "$LABEL" --arg ns "$NS" --argjson owned "$owned" '
            (.items // [.])[] | select(.kind != null)
            | select($owned or .metadata.labels[$l] == "true")
            | select(.kind != "ClusterRoleBinding" or ([.subjects[]?.namespace] | index($ns)))
            | "\(.kind)/\(.metadata.name)"'
}
owned() { [[ "$("${K[@]}" get namespace "$1" -o jsonpath='{.metadata.labels.aisa\.hlan\.net/local-setup}' 2>/dev/null)" == true ]]; }

helm --kube-context "$CONTEXT" uninstall "$RELEASE" -n "$NS" --wait --timeout 2m >/dev/null 2>&1 || true
# Before the namespaces go: the ClusterRoleBinding outlives them.
mapfile -t fixture_objects < <(ours)
if [[ ${#fixture_objects[@]} -gt 0 ]]; then
    "${K[@]}" -n "$NS" delete --ignore-not-found "${fixture_objects[@]}" >/dev/null
fi
for ns in "$APP_NS" "$NS"; do
    "${K[@]}" get namespace "$ns" >/dev/null 2>&1 || continue
    if owned "$ns"; then
        "${K[@]}" delete namespace "$ns" --wait=true >/dev/null
        echo "removed the namespace $ns"
    else
        # Not ours: take out only what up.sh put in, which carries its label; the fixtures are
        # gone already.
        if [[ "$ns" == "$APP_NS" ]]; then
            "${K[@]}" -n "$APP_NS" delete secret -l "$LABEL=true" --field-selector metadata.name=aisa --ignore-not-found >/dev/null
        fi
        echo "left the namespace $ns, which existed before up.sh; removed only this setup's objects"
    fi
done
echo "removed the release $RELEASE from $CONTEXT"
