#!/usr/bin/env bash
# Spike S9: consul-template renders apisix.yaml from Consul and Vault, and APISIX reloads it.
# Does a change reach the gateway, and does a reload drop requests?
#
#   ./adapters/apisix/spikes/s9-config-rendering/run.sh
#
# Starts the dev stack with this spike's consul-template, runs the checks and leaves the stack up
# for inspection (docker compose -f dev/compose.yaml -f <this directory>/compose.override.yaml
# down -v to stop). Exits non-zero on any unexpected result. Needs curl and jq.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
COMPOSE=(docker compose -f "$repo/dev/compose.yaml" -f "$here/compose.override.yaml")
GATEWAY=http://127.0.0.1:9080
STUB=http://127.0.0.1:8081
CONSUL=http://127.0.0.1:8500
VAULT=http://127.0.0.1:8200

tmp=$(mktemp -d)
cleanup() {
    rm -f "$tmp/loading"
    wait 2>/dev/null || true
    rm -rf "$tmp"
}
trap cleanup EXIT

"${COMPOSE[@]}" up -d --build --wait --force-recreate >/dev/null

failures=0
check() { # check <label> <expected> <actual>
    if [[ "$2" == "$3" ]]; then
        printf '  ok    %-66s %s\n' "$1" "$3"
    else
        printf '  FAIL  %-66s %s (expected %s)\n' "$1" "$3" "$2"
        failures=$((failures + 1))
    fi
}
note() { printf '        %-66s %s\n' "$1" "$2"; }

rendered() { "${COMPOSE[@]}" exec -T consul-template cat /rendered/apisix.yaml; }
overwrite() { "${COMPOSE[@]}" exec -T consul-template sh -c 'cat >/rendered/apisix.yaml'; }
instances() { rendered | grep -c "name: \"$1\"" || true; }
reloads() { "${COMPOSE[@]}" logs apisix 2>/dev/null | grep -c 'apisix.yaml reloaded' || true; }
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f s", b - a }'; }

# wait_until <seconds> <command...>: until the command succeeds; fails after the time is up
wait_until() {
    local end=$(($(date +%s) + $1))
    shift
    until "$@" >/dev/null 2>&1; do
        (($(date +%s) < end)) || return 1
        sleep 0.2
    done
}
in_rotation() { [[ "$(instances "$1")" -ge 1 ]]; }
out_of_rotation() { [[ "$(instances "$1")" -eq 0 ]]; }

# register <id> <models> [key]: a backend in the Consul catalog, as the Terraform module will do.
# The key names the Vault secret under secret/aisa/providers/ that holds the backend's API key.
register() {
    curl -fsS -X PUT "$CONSUL/v1/agent/service/register" -d "$(jq -cn --arg id "$1" --arg models "$2" --arg key "${3:-}" \
        '{ID: $id, Name: "aisa-backend", Address: $id, Port: 8080,
          Meta: ({provider: "openai-compatible", models: $models} + (if $key != "" then {key: $key} else {} end)),
          Check: {CheckID: "service:\($id)", HTTP: "http://\($id):8080/healthz", Interval: "2s", Timeout: "1s"}}')"
}
maintenance() { curl -fsS -X PUT "$CONSUL/v1/agent/service/maintenance/$1?enable=$2&reason=spike-s9" >/dev/null; }

# ask <model> [curl args...] → sets STATUS; the body is in $tmp/body
ask() {
    local model=$1
    shift
    STATUS=$(curl -sS -m 60 -o "$tmp/body" -w '%{http_code}' -H 'Authorization: Bearer dev-key-chat-ui' \
        -H 'Content-Type: application/json' -H 'X-Mock-Completion-Tokens: 2' "$@" \
        -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"hello there\"}]}" \
        "$GATEWAY/v1/chat/completions" || true)
}
events() { curl -fsS "$STUB/debug/requests?kind=usage" | jq -c '[.[] | .body]'; }
clear_events() { curl -fsS -X DELETE "$STUB/debug/requests" >/dev/null; }

# Load: workers that ask for qwen3 until $tmp/loading is removed, a line
# "<status> <time it began> <seconds it took>" per request.
worker() { # worker <id> <stream: true|false> <tokens>
    local code began
    while [[ -e "$tmp/loading" ]]; do
        began=$(now)
        code=$(curl -sS -N -m 60 -o "$tmp/load.body.$1" -w '%{http_code}' -H 'Authorization: Bearer dev-key-chat-ui' \
            -H 'Content-Type: application/json' -H "X-Mock-Completion-Tokens: $3" \
            -d "{\"model\":\"qwen3\",\"messages\":[{\"role\":\"user\",\"content\":\"hello there\"}],\"stream\":$2}" \
            "$GATEWAY/v1/chat/completions" 2>/dev/null) || code=000
        if [[ "$2" == true ]] && [[ "$code" == 200 ]] && ! grep -q '^data: \[DONE\]' "$tmp/load.body.$1"; then
            code=incomplete
        fi
        echo "$code $began $(awk -v a="$began" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }')"
    done >"$tmp/load.result.$1"
}
LOAD_PIDS=()
start_load() { # 4 workers with short answers, 2 with streams of 2 s
    rm -f "$tmp"/load.result.*
    touch "$tmp/loading"
    LOAD_PIDS=()
    for id in 1 2 3 4; do
        worker "$id" false 2 &
        LOAD_PIDS+=("$!")
    done
    for id in 5 6; do
        worker "$id" true 100 &
        LOAD_PIDS+=("$!")
    done
}
stop_load() {
    rm -f "$tmp/loading"
    wait "${LOAD_PIDS[@]}"
    cat "$tmp"/load.result.* >"$tmp/load.all"
}
load_total() { wc -l <"$tmp/load.all" | tr -d ' '; }
load_failed() { awk '$1 != 200' "$tmp/load.all" | wc -l | tr -d ' '; }

echo "== Rendering: backends from the Consul catalog, the provider key from Vault"
register mock-local "qwen3, llama3.2"
register mock-local-2 "qwen3"
register mock-cloud "cloud-large" cloud
wait_until 30 in_rotation mock-local-2
sleep 2 # APISIX looks at the file once a second
check "routes (client, one per model, no-backend)" "client model-cloud-large model-llama3.2 model-qwen3 no-backend" \
    "$(rendered | sed -n 's/^  - id: "\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' | sort | tr '\n' ' ' | sed 's/ $//')"
check "backends of qwen3" "mock-local mock-local-2" \
    "$(rendered | awk '/id: "model-qwen3"/ { on = 1 } on && /name:/ { gsub(/[" ]|-? *name:/, ""); printf "%s ", $0 } on && /http-logger/ { exit }' | sed 's/^- //; s/ -/ /g; s/ $//')"
check "the key of mock-cloud comes from Vault" 1 "$(rendered | grep -c 'Authorization: "Bearer dev-provider-key"' || true)"
check "backends without a key get none" 0 "$(rendered | grep -B3 'options: {model: "qwen3"}' | grep -c Bearer || true)"
check "the file ends with #END" "#END" "$(rendered | tail -1)"

echo
echo "== Routing through the rendered config"
clear_events
for m in qwen3 qwen3 qwen3 qwen3 llama3.2 cloud-large; do
    ask "$m"
    [[ "$STATUS" == 200 ]] || check "request for $m" 200 "$STATUS"
done
ask no-such-model
check "a model that no backend serves" 503 "$STATUS"
check "  ... with an error a client can read" no_backend "$(jq -r '.error.type' "$tmp/body")"
sleep 2
check "who served what" "cloud-large:mock-cloud llama3.2:mock-local qwen3:mock-local qwen3:mock-local-2" \
    "$(events | jq -r '[.[] | select(.status == 200) | "\(.requested_model):\(.backend)"] | unique | join(" ")')"

echo
echo "== Reload under load: a backend leaves and returns 10 times (Consul maintenance mode)"
# Maintenance mode takes a backend out of the healthy list while it still answers, so a failed
# request here is caused by the reload, not by the backend.
reloads_before=$(reloads)
start_load
out_times=() in_times=()
for _ in $(seq 1 10); do
    t=$(now)
    maintenance mock-local-2 true
    wait_until 30 out_of_rotation mock-local-2
    out_times+=("$(since "$t")")
    sleep 2
    t=$(now)
    maintenance mock-local-2 false
    wait_until 30 in_rotation mock-local-2
    in_times+=("$(since "$t")")
    sleep 2
done
stop_load
check "failed requests during 20 reloads" 0 "$(load_failed)"
note "requests sent meanwhile" "$(load_total)"
note "reloads APISIX logged (4 workers and the master log each)" "$(($(reloads) - reloads_before))"
note "from the change in Consul to the new file, leaving" "${out_times[*]}"
note "from the change in Consul to the new file, returning" "${in_times[*]}"

echo
echo "== After a backend has left, nothing is sent to it"
maintenance mock-local-2 true
wait_until 30 out_of_rotation mock-local-2
sleep 2
clear_events
for _ in $(seq 1 10); do ask qwen3; done
sleep 2
check "backends that served 10 requests for qwen3" "mock-local" "$(events | jq -r '[.[] | .backend] | unique | join(" ")')"
maintenance mock-local-2 false
wait_until 30 in_rotation mock-local-2
sleep 2

echo
echo "== Streams in flight when their backend leaves"
clear_events
for id in 1 2 3 4; do
    curl -sS -N -m 60 -o "$tmp/stream.$id" -H 'Authorization: Bearer dev-key-chat-ui' -H 'Content-Type: application/json' \
        -H 'X-Mock-Completion-Tokens: 400' \
        -d '{"model":"qwen3","messages":[{"role":"user","content":"hello there"}],"stream":true}' \
        "$GATEWAY/v1/chat/completions" &
done
sleep 1
t=$(now)
maintenance mock-local-2 true
wait_until 30 out_of_rotation mock-local-2
note "the backend left the config, 1 s into streams of 8 s, after" "$(since "$t")"
wait
complete=0
for id in 1 2 3 4; do
    if grep -q '^data: \[DONE\]' "$tmp/stream.$id"; then complete=$((complete + 1)); fi
done
sleep 2
check "streams that ran to their end" 4 "$complete"
check "  ... with 400 tokens in the usage event" 4 "$(events | jq '[.[] | select(.completion_tokens == 400)] | length')"
check "  ... of which on the backend that left, at least one" true "$(events | jq '[.[] | select(.backend == "mock-local-2")] | length >= 1')"
maintenance mock-local-2 false
wait_until 30 in_rotation mock-local-2
sleep 2

echo
echo "== A backend that dies (container stopped), under load"
start_load
sleep 2
t=$(now)
"${COMPOSE[@]}" stop -t 0 mock-local-2 >/dev/null 2>&1
wait_until 60 out_of_rotation mock-local-2
note "from the stop to the new file" "$(since "$t")"
sleep 5
stop_load
note "requests sent / failed" "$(load_total) / $(load_failed)"
note "statuses of the failed requests" "$(awk '$1 != 200 { print $1 }' "$tmp/load.all" | sort | uniq -c | awk '{ printf "%s x %s  ", $2, $1 }')"
if [[ "$(load_failed)" -gt 0 ]]; then
    note "the last request that failed began, after the stop" \
        "$(awk -v t="$t" '$1 != 200 && $2 > m { m = $2 } END { printf "%.1f s", m - t }' "$tmp/load.all")"
    note "the failed requests took, shortest / longest" \
        "$(awk '$1 != 200 { print $3 }' "$tmp/load.all" | sort -n | awk 'NR == 1 { a = $1 } { b = $1 } END { printf "%.1f s / %.1f s", a, b }')"
fi
failed=0
for _ in $(seq 1 10); do
    ask qwen3
    [[ "$STATUS" == 200 ]] || failed=$((failed + 1))
done
check "afterwards: 10 requests for qwen3, failed" 0 "$failed"
"${COMPOSE[@]}" start mock-local-2 >/dev/null 2>&1
t=$(now)
wait_until 60 in_rotation mock-local-2
note "back in the config after its start" "$(since "$t")"
sleep 2

echo
echo "== A rotated provider key"
t=$(now)
curl -fsS -X POST -H 'X-Vault-Token: dev-root' "$VAULT/v1/secret/data/aisa/providers/cloud" -d '{"data":{"api_key":"rotated-provider-key"}}' >/dev/null
has_new_key() { rendered | grep -q 'Bearer rotated-provider-key'; }
if wait_until 60 has_new_key; then
    note "in the rendered file after (default_lease_duration is 10 s)" "$(since "$t")"
fi
check "the old key is gone from the file" 0 "$(rendered | grep -c 'Bearer dev-provider-key' || true)"
sleep 2
ask cloud-large
check "requests for cloud-large still work" 200 "$STATUS"
curl -fsS -X POST -H 'X-Vault-Token: dev-root' "$VAULT/v1/secret/data/aisa/providers/cloud" -d '{"data":{"api_key":"dev-provider-key"}}' >/dev/null
has_old_key() { rendered | grep -q 'Bearer dev-provider-key'; }
wait_until 60 has_old_key

echo
echo "== Consul or Vault unreachable: the last config stays"
for svc in consul vault; do
    sum=$(rendered | sha256sum)
    "${COMPOSE[@]}" pause "$svc" >/dev/null 2>&1
    sleep 15
    check "$svc paused for 15 s: the rendered file is unchanged" true "$([[ "$(rendered | sha256sum)" == "$sum" ]] && echo true || echo false)"
    ask qwen3
    check "  ... and requests work" 200 "$STATUS"
    check "  ... and consul-template is still running" running "$("${COMPOSE[@]}" ps --format '{{.State}}' consul-template)"
    "${COMPOSE[@]}" unpause "$svc" >/dev/null 2>&1
    sleep 3
done

echo
echo "== A broken file: what APISIX does with it"
good=$(rendered)
broken() { # broken <label> <expected status of a request for qwen3> ; the file comes from stdin
    overwrite
    sleep 3
    ask qwen3
    check "$1: request for qwen3" "$2" "$STATUS"
}
printf 'routes:\n  - id: [unclosed\n#END\n' | broken "invalid YAML" 200
{ echo "$good" | sed '$d'; } | broken "no #END at the end (a file cut short)" 200
printf 'routes: []\n#END\n' | broken "no routes at all (APISIX keeps the ones it has)" 200
{ echo "$good" | sed 's/request_method: GET/request_method: TELEPORT/'; } | broken "one route invalid (the client route)" 404
echo "$good" | overwrite
sleep 3
ask qwen3
check "the good file again: request for qwen3" 200 "$STATUS"

echo
if [[ "$failures" -eq 0 ]]; then
    echo "all checks as expected"
else
    echo "$failures unexpected results" >&2
    exit 1
fi
