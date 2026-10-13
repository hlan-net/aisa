#!/usr/bin/env bash
# Removes what up.sh installed: the consumer and its Secret, the release, the Model objects of
# this install, the fixtures and their ClusterRoleBinding, and the namespaces up.sh created.
# Namespaces and objects that up.sh did not create (no label aisa.hlan.net/local-setup) are left
# in place, and so is the Model CRD, which other installs may use.
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
owned() { local ns=$1; [[ "$("${K[@]}" get namespace "$ns" -o jsonpath='{.metadata.labels.aisa\.hlan\.net/local-setup}' 2>/dev/null)" == true ]]; }

# The consumer first, while aisa runs: it deletes the Secret it wrote, and the key's hashes.
"${K[@]}" -n "$NS" exec deploy/consul -- env CONSUL_HTTP_TOKEN=test-root consul kv delete "aisa/consumers/$CONSUMER" >/dev/null 2>&1 || true
for _ in $(seq 1 30); do
    "${K[@]}" -n "$APP_NS" get secret aisa >/dev/null 2>&1 || break
    sleep 1
done
helm --kube-context "$CONTEXT" uninstall "$RELEASE" -n "$NS" --wait --timeout 2m >/dev/null 2>&1 || true
# What aisa wrote and could not remove: the Secret if aisa was not running, and the Models.
"${K[@]}" -n "$APP_NS" delete secret --ignore-not-found --field-selector metadata.name=aisa \
    -l "app.kubernetes.io/managed-by=aisa,aisa.hlan.net/install=$NS" >/dev/null 2>&1 || true
"${K[@]}" delete models.aisa.hlan.net --ignore-not-found \
    -l "app.kubernetes.io/managed-by=aisa,aisa.hlan.net/install=$NS" >/dev/null 2>&1 || true
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
        # Not ours: the fixtures and the Secret are gone already.
        echo "left the namespace $ns, which existed before up.sh; removed only this setup's objects"
    fi
done
echo "removed the release $RELEASE from $CONTEXT"
