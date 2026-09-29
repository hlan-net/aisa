#!/usr/bin/env bash
# Spike S5: APISIX in standalone mode on Kubernetes, with the official chart and with plain
# manifests that put consul-template next to it. Which one fits the adapter?
#
#   OLLAMA_ADDR=gpu-box.internal:11434 ./adapters/apisix/spikes/s5-helm-chart/run.sh
#   OLLAMA_ADDR=gpu-box.internal:11434 ./adapters/apisix/spikes/s5-helm-chart/run.sh manifests
#   kubectl delete namespace aisa-spike      # when done
#
# Uses the current kubectl context and the namespace aisa-spike (NAMESPACE), which it creates
# and leaves for inspection. The backend is a real OpenAI-compatible server that the cluster can
# reach at OLLAMA_ADDR and that serves MODEL. Everything else comes from public images.
# The parts are "chart" and "manifests"; without an argument both run. Exits non-zero on any
# unexpected result. Needs kubectl, helm and jq.
set -euo pipefail

: "${OLLAMA_ADDR:?set OLLAMA_ADDR to host:port of an OpenAI-compatible backend the cluster can reach}"
NAMESPACE=${NAMESPACE:-aisa-spike}
MODEL=${MODEL:-llama3.2:3b}
CHART_VERSION=2.17.0
PARTS=${1:-"chart manifests"}

here=$(cd "$(dirname "$0")" && pwd)
s9=$(cd "$here/../s9-config-rendering" && pwd)
K=(kubectl -n "$NAMESPACE")
GATEWAY=http://apisix-gateway
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

failures=0
check() { # check <label> <expected> <actual>
    local label=$1 expected=$2 actual=$3
    if [[ "$expected" == "$actual" ]]; then
        printf '  ok    %-62s %s\n' "$label" "$actual"
    else
        printf '  FAIL  %-62s %s (expected %s)\n' "$label" "$actual" "$expected"
        failures=$((failures + 1))
    fi
}
note() { # note <label> <value>
    local label=$1 value=$2
    printf '        %-62s %s\n' "$label" "$value"
}

in_probe() { "${K[@]}" exec probe -- "$@"; }
status_of() { # status_of <path> [curl args...]: the HTTP status of a request through the gateway
    local path=$1
    shift
    in_probe curl -s -o /dev/null -m 120 -w '%{http_code}' "$@" "$GATEWAY$path" || true
}
chat() { # chat <model> <stream: true|false>: the body of a chat request
    local model=$1 stream=$2
    jq -cn --arg m "$model" --argjson s "$stream" \
        '{model: $m, messages: [{role: "user", content: "Reply with exactly the word: ok"}], max_tokens: 8, temperature: 0, seed: 1, stream: $s}'
}
ask() { # ask <model> <stream> → sets STATUS; the answer is in $tmp/answer
    local model=$1 stream=$2
    in_probe curl -sN -m 300 -w '\n%{http_code}' -H 'Authorization: Bearer spike-key' \
        -H 'Content-Type: application/json' -d "$(chat "$model" "$stream")" "$GATEWAY/v1/chat/completions" >"$tmp/answer" || true
    STATUS=$(tail -1 "$tmp/answer")
}
wait_until() { # wait_until <seconds> <command...>
    local seconds=$1 end
    shift
    end=$(($(date +%s) + seconds))
    until "$@" >/dev/null 2>&1; do
        (($(date +%s) < end)) || return 1
        sleep 1
    done
}
usage_of() { # usage_of <container>: "<cores> / <MiB>" as the kubelet reports it
    local container=$1
    "${K[@]}" top pod -l app.kubernetes.io/name=apisix --containers --no-headers 2>/dev/null |
        awk -v c="$container" '$2 == c { print $3 " / " $4 }'
    "${K[@]}" top pod -l app=apisix --containers --no-headers 2>/dev/null |
        awk -v c="$container" '$2 == c { print $3 " / " $4 }'
}
hardware() { # hardware <pod selector> <container>
    local selector=$1 container=$2
    "${K[@]}" exec "$("${K[@]}" get pod -l "$selector" -o name | head -1)" -c "$container" -- \
        sh -c 'grep -m1 "^Model" /proc/cpuinfo | cut -d: -f2- | sed "s/^ //"; nproc' | tr '\n' ' ' | sed 's/ $//'
}

kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || kubectl create namespace "$NAMESPACE" >/dev/null
if ! "${K[@]}" get pod probe >/dev/null 2>&1; then
    "${K[@]}" run probe --image=curlimages/curl:8.16.0 --restart=Never --command -- sleep 86400 >/dev/null
fi
"${K[@]}" wait --for=condition=Ready pod/probe --timeout=180s >/dev/null
check "the backend answers from the cluster" 200 \
    "$(in_probe curl -s -o /dev/null -m 10 -w '%{http_code}' "http://$OLLAMA_ADDR/v1/models" || true)"

# remove_gateway removes the gateway of an earlier run or part, and waits until its pods are
# gone: they would answer in place of the new ones.
remove_gateway() {
    helm uninstall apisix -n "$NAMESPACE" --wait >/dev/null 2>&1 || true
    "${K[@]}" delete deployment apisix --ignore-not-found --wait >/dev/null 2>&1
    "${K[@]}" delete service apisix-gateway --ignore-not-found >/dev/null 2>&1
    "${K[@]}" wait --for=delete pod -l app=apisix --timeout=180s >/dev/null 2>&1 || true
    "${K[@]}" wait --for=delete pod -l app.kubernetes.io/name=apisix --timeout=180s >/dev/null 2>&1 || true
}

# --- the official chart ---------------------------------------------------------------------

part_chart() {
    echo
    echo "== The official chart apisix/apisix $CHART_VERSION in standalone mode"
    remove_gateway
    helm repo add apisix https://charts.apiseven.com >/dev/null 2>&1 || true
    helm repo update apisix >/dev/null 2>&1

    # The address of the backend is the only value that is not in the repository.
    sed "s#gpu-box.internal:11434#$OLLAMA_ADDR#" "$here/values-standalone.yaml" >"$tmp/values.yaml"
    helm template apisix apisix/apisix --version "$CHART_VERSION" -n "$NAMESPACE" -f "$tmp/values.yaml" >"$tmp/rendered.yaml"
    check "kinds that the chart renders" "ConfigMap ConfigMap Deployment Secret Service" \
        "$(grep '^kind:' "$tmp/rendered.yaml" | awk '{ print $2 }' | sort | tr '\n' ' ' | sed 's/ $//')"
    check "  ... left of etcd: a Secret and an environment variable" "1 1" \
        "$(grep -c '^  name: etcd-apisix$' "$tmp/rendered.yaml" || true) $(grep -c 'name: APISIX_ETCD_PASSWORD' "$tmp/rendered.yaml" || true)"

    local began
    began=$(date +%s)
    helm upgrade --install apisix apisix/apisix --version "$CHART_VERSION" -n "$NAMESPACE" -f "$tmp/values.yaml" \
        --wait --timeout 300s >/dev/null
    note "installed and ready after" "$(($(date +%s) - began)) s"
    note "node" "$(hardware app.kubernetes.io/name=apisix apisix) cores"
    check "pods of etcd" 0 "$("${K[@]}" get pods --no-headers 2>/dev/null | grep -c etcd || true)"
    check "nginx workers" 1 "$("${K[@]}" exec deploy/apisix -- sh -c \
        'for p in /proc/[0-9]*; do tr "\0" " " <$p/cmdline; echo; done' 2>/dev/null | grep -c 'worker process' || true)"
    check "the internal listener of the two-hop route" 1 "$("${K[@]}" exec deploy/apisix -- \
        grep -c 'listen 127.0.0.1:9081' /usr/local/apisix/conf/nginx.conf || true)"

    ask "$MODEL" false
    check "a request, not streamed" 200 "$STATUS"
    ask "$MODEL" true
    check "a request, streamed" 200 "$STATUS"
    check "  ... to its end, with usage" "1 1" "$(grep -c '^data: \[DONE\]' "$tmp/answer" || true) $(grep -c '"usage":{' "$tmp/answer" || true)"
    sleep 20 # the kubelet's numbers lag
    note "APISIX, cores / memory" "$(usage_of apisix)"

    echo
    echo "== A changed route, through helm upgrade"
    local pod
    pod=$("${K[@]}" get pod -l app.kubernetes.io/name=apisix -o name)
    python3 - "$tmp/values.yaml" "$tmp/values-2.yaml" <<'EOF'
import sys
values = open(sys.argv[1]).read()
marker = "        routes:\n"
assert values.count(marker) == 1
route = """          - id: s5-probe
            uri: /s5-probe
            plugins:
              fault-injection:
                abort: {http_status: 200, body: "changed"}
"""
open(sys.argv[2], "w").write(values.replace(marker, marker + route))
EOF
    check "the new route before the change" 404 "$(status_of /s5-probe)"
    began=$(date +%s)
    helm upgrade apisix apisix/apisix --version "$CHART_VERSION" -n "$NAMESPACE" -f "$tmp/values-2.yaml" \
        --wait --timeout 300s >/dev/null
    note "helm upgrade returned after" "$(($(date +%s) - began)) s"
    reached() { [[ "$(status_of /s5-probe)" == 200 ]]; }
    if wait_until 180 reached; then
        note "the change reached APISIX after" "$(($(date +%s) - began)) s"
    else
        check "the change reached APISIX within 180 s" yes no
    fi
    check "the pod was replaced" no "$([[ "$("${K[@]}" get pod -l app.kubernetes.io/name=apisix -o name)" == "$pod" ]] && echo no || echo yes)"

    echo
    echo "== Can the chart take a file that a sidecar renders?"
    cat >"$tmp/sidecar.yaml" <<'EOF'
extraVolumes:
  - name: rendered
    emptyDir: {medium: Memory}
extraVolumeMounts:
  - name: rendered
    mountPath: /apisix-config
EOF
    helm template apisix apisix/apisix --version "$CHART_VERSION" -n "$NAMESPACE" -f "$tmp/values.yaml" -f "$tmp/sidecar.yaml" |
        python3 -c 'import re, sys
docs = [d for d in re.split(r"(?m)^---\s*$", sys.stdin.read()) if re.search(r"(?m)^kind: Deployment", d)]
sys.stdout.write(docs[0].replace("  name: apisix\n", "  name: apisix-with-sidecar\n", 1))' >"$tmp/deployment.yaml"
    check "a shared directory at the path of apisix.yaml" "mountPath: Invalid value: \"/apisix-config\": must be unique" \
        "$("${K[@]}" create --dry-run=server -f "$tmp/deployment.yaml" 2>&1 | grep -o 'mountPath: .*must be unique' | head -1)"

    remove_gateway
}

# --- plain manifests with consul-template ---------------------------------------------------

rendered() { "${K[@]}" exec deploy/apisix -c apisix -- cat /rendered/apisix.yaml; }
instances() { # instances <backend>: how often the rendered file names it
    local backend=$1
    rendered | grep -c "name: \"$backend\"" || true
}
in_rotation() {
    local backend=$1
    [[ "$(instances "$backend")" -ge 1 ]]
}
out_of_rotation() {
    local backend=$1
    [[ "$(instances "$backend")" -eq 0 ]]
}
register() { # register <id> <models>: a backend at OLLAMA_ADDR in the spike's Consul
    local id=$1 models=$2
    "${K[@]}" exec deploy/consul -- consul services register -id="$id" -name=aisa-backend \
        -address="${OLLAMA_ADDR%:*}" -port="${OLLAMA_ADDR##*:}" \
        -meta=provider=openai-compatible -meta="models=$models" >/dev/null
}
maintenance() { # maintenance <backend> <enable|disable>
    local backend=$1 mode=$2
    "${K[@]}" exec deploy/consul -- consul maint "-$mode" -service="$backend" >/dev/null
}
stub_log() { "${K[@]}" logs deploy/stub-aisa --since=10m 2>/dev/null; }

part_manifests() {
    echo
    echo "== Plain manifests: APISIX and consul-template in one pod"
    "${K[@]}" apply -f "$here/support.yaml" >/dev/null
    "${K[@]}" rollout restart deploy/stub-aisa >/dev/null # an empty log to count in
    "${K[@]}" rollout status deploy/consul --timeout=300s >/dev/null
    "${K[@]}" rollout status deploy/stub-aisa --timeout=300s >/dev/null
    register ollama-1 "$MODEL, no-colon-model"

    configmap() { "${K[@]}" create configmap "$@" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null; }
    configmap apisix-config --from-file="$here/config.yaml"
    configmap apisix-render --from-file="$s9/apisix.yaml.ctmpl" --from-file="$here/consul-template.hcl"
    remove_gateway
    local began
    began=$(date +%s)
    "${K[@]}" apply -f "$here/adapter.yaml" >/dev/null
    "${K[@]}" rollout status deploy/apisix --timeout=300s >/dev/null
    note "applied and ready after" "$(($(date +%s) - began)) s"
    note "node" "$(hardware app=apisix apisix) cores"
    sleep 2 # APISIX looks at the file once a second

    check "containers ready" "true true" "$("${K[@]}" get pod -l app=apisix \
        -o jsonpath='{.items[0].status.containerStatuses[*].ready}')"
    check "the rendered file is in memory" tmpfs "$("${K[@]}" exec deploy/apisix -c apisix -- df /rendered | awk 'NR == 2 { print $1 }')"
    check "nginx workers" 1 "$("${K[@]}" exec deploy/apisix -c apisix -- sh -c \
        'for p in /proc/[0-9]*; do tr "\0" " " <$p/cmdline; echo; done' 2>/dev/null | grep -c 'worker process' || true)"
    # A model name with other characters than a route id may have gets them replaced, and a hash.
    local safe id
    safe=$(sed 's/[^a-zA-Z0-9_.-]/-/g' <<<"$MODEL")
    id=model-$safe
    [[ "$safe" == "$MODEL" ]] || id="model-$safe-$(printf '%s' "$MODEL" | sha256sum | cut -c1-8)"
    check "routes" "$(printf '%s\n' client "$id" model-no-colon-model no-backend | sort | tr '\n' ' ' | sed 's/ $//')" \
        "$(rendered | sed -n 's/^  - id: "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' | sort | tr '\n' ' ' | sed 's/ $//')"
    check "routes that APISIX rejected" 0 "$("${K[@]}" logs deploy/apisix -c apisix 2>/dev/null | grep -c 'failed to check item' || true)"

    echo
    echo "== Traffic: client, decision, backend, usage event"
    ask "$MODEL" false
    check "a request, not streamed" 200 "$STATUS"
    check "  ... with usage in the answer" true "$(sed '$d' "$tmp/answer" | jq '.usage.completion_tokens > 0' 2>/dev/null)"
    ask "$MODEL" true
    check "a request, streamed" 200 "$STATUS"
    check "  ... to its end, with usage" "1 1" "$(grep -c '^data: \[DONE\]' "$tmp/answer" || true) $(grep -c '"usage":{' "$tmp/answer" || true)"
    ask no-such-model false
    check "a model that no backend serves" 503 "$STATUS"
    sleep 3
    check "decisions that the stub answered" 3 "$(stub_log | grep -c '/v1/decide 200' || true)"
    check "usage events that the stub received" 3 "$(stub_log | grep -c '/v1/usage 204' || true)"
    sleep 20
    note "APISIX, cores / memory (requests 100m / 96Mi, limit 192Mi)" "$(usage_of apisix)"
    note "consul-template, cores / memory (requests 10m / 32Mi, limit 64Mi)" "$(usage_of consul-template)"

    echo
    echo "== A backend leaves and returns 5 times, under a request loop"
    # The loop asks for a model that stays served; the reloads must not disturb it. It gets the
    # decision and the 503 of the no-backend route, which need no backend.
    register ollama-2 "$MODEL"
    wait_until 60 in_rotation ollama-2
    in_probe sh -c "rm -f /tmp/loop /tmp/stop; (while [ ! -e /tmp/stop ]; do
        curl -s -o /dev/null -m 5 -w '%{http_code}\n' -H 'Authorization: Bearer spike-key' -H 'Content-Type: application/json' \
            -d '{\"model\":\"no-such-model\"}' $GATEWAY/v1/chat/completions >>/tmp/loop; sleep 0.2; done) >/dev/null 2>&1 &"
    local out=() back=() t
    for _ in 1 2 3 4 5; do
        t=$(date +%s.%N)
        maintenance ollama-2 enable
        wait_until 60 out_of_rotation ollama-2
        out+=("$(awk -v a="$t" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f s", b - a }')")
        sleep 2
        t=$(date +%s.%N)
        maintenance ollama-2 disable
        wait_until 60 in_rotation ollama-2
        back+=("$(awk -v a="$t" -v b="$(date +%s.%N)" 'BEGIN { printf "%.1f s", b - a }')")
        sleep 2
    done
    read -r sent other <<<"$(in_probe sh -c "touch /tmp/stop; sleep 1; echo \$(wc -l </tmp/loop) \$(grep -cv '^503\$' /tmp/loop)")"
    check "requests answered otherwise than expected during 10 reloads" 0 "$other"
    note "requests sent meanwhile" "$sent"
    note "from the change in Consul to the new file, leaving" "${out[*]}"
    note "from the change in Consul to the new file, returning" "${back[*]}"
    ask "$MODEL" false
    check "a request after the reloads" 200 "$STATUS"

    echo
    echo "== The pod is replaced"
    local old
    old=$("${K[@]}" get pod -l app=apisix -o name)
    began=$(date +%s)
    "${K[@]}" delete "$old" --wait=false >/dev/null
    new_pod_ready() {
        "${K[@]}" get pod -l app=apisix -o json | jq -e --arg old "${old#pod/}" '
            [.items[] | select(.metadata.name != $old and .metadata.deletionTimestamp == null)
             | select([.status.containerStatuses[]?.ready] | length == 2 and all)] | length == 1'
    }
    if wait_until 300 new_pod_ready; then
        note "a new pod is ready after" "$(($(date +%s) - began)) s"
    else
        check "a new pod is ready within 300 s" yes no
    fi
    "${K[@]}" wait --for=delete "$old" --timeout=180s >/dev/null 2>&1 || true
    sleep 2
    ask "$MODEL" false
    check "a request to the new pod" 200 "$STATUS"
}

for part in $PARTS; do
    case $part in
    chart) part_chart ;;
    manifests) part_manifests ;;
    *)
        echo "unknown part: $part (want chart or manifests)" >&2
        exit 2
        ;;
    esac
done

echo
if [[ "$failures" -eq 0 ]]; then
    echo "all checks as expected"
else
    echo "$failures unexpected results" >&2
    exit 1
fi
