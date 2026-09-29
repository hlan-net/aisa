#!/usr/bin/env bash
# Spikes S7 + S10: memory and CPU of APISIX, the stub aisa and consul-template under parallel
# streams, and the latency that forward-auth and the internal hop add.
#
#   ./adapters/apisix/spikes/s7-s10-footprint/run.sh
#   APISIX_WORKERS=1 STREAMS="1 10" DURATION=10 ./adapters/apisix/spikes/s7-s10-footprint/run.sh
#
# Starts the dev stack with this spike's config, measures and leaves the stack up for inspection
# (docker compose -f dev/compose.yaml -f <this directory>/compose.override.yaml down -v to stop).
# Prints tables; nothing is asserted except that the requests succeed, because the numbers are
# what this spike is meant to find out. Needs curl, jq and Docker with cgroup v2.
#
# Memory is the proportional set size (PSS) of the container's processes: memory shared between
# the nginx workers is counted once. It is read from /proc by a helper container in the host's
# PID namespace, so it also works where Docker has no memory accounting (cgroup_disable=memory,
# the default on Raspberry Pi OS). CPU is the container's cgroup cpu.stat.
set -euo pipefail

STREAMS=${STREAMS:-"1 10 50 100"}   # parallel streams per load phase
DURATION=${DURATION:-30}            # seconds per phase
TOKENS=${TOKENS:-256}               # completion tokens per stream (about 5 s at the mock's pace)
REQUESTS=${REQUESTS:-300}           # requests per variant and concurrency in the latency part
ROUNDS=3                            # the variants take turns, so drift hits them alike
MEASURED=(apisix stub-aisa consul-template)
PROBE=aisa-spike-probe

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
export APISIX_CONFIG=$here/config.yaml APISIX_ROUTES=$here/apisix.yaml
export APISIX_WORKERS=${APISIX_WORKERS:-auto}
COMPOSE=(docker compose -f "$repo/dev/compose.yaml" -f "$here/compose.override.yaml")
GATEWAY=http://127.0.0.1:9080
STUB=http://127.0.0.1:8081

tmp=$(mktemp -d)
cleanup() {
    rm -f "$tmp/sampling"
    wait 2>/dev/null || true
    docker rm -f "$PROBE" >/dev/null 2>&1 || true
    rm -rf "$tmp"
}
trap cleanup EXIT

# --force-recreate: every run starts from freshly started processes.
"${COMPOSE[@]}" up -d --build --wait --force-recreate >/dev/null
# The route file is loaded asynchronously after the port opens.
# 000: not listening yet, 404: routes not loaded yet.
for i in $(seq 1 60); do
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GATEWAY/full/v1/chat/completions" || true)
    [[ "$code" != "000" ]] && [[ "$code" != "404" ]] && break
    [[ "$i" -eq 60 ]] && { echo "gateway not ready (last status $code)" >&2; exit 1; }
    sleep 1
done
docker rm -f "$PROBE" >/dev/null 2>&1 || true
docker run -d --name "$PROBE" --pid=host --cap-add SYS_PTRACE --network none busybox:1.37 sleep 86400 >/dev/null

declare -A CGROUP
for svc in "${MEASURED[@]}"; do
    cid=$("${COMPOSE[@]}" ps -q "$svc")
    CGROUP[$svc]=$(find /sys/fs/cgroup -maxdepth 4 -type d -name "docker-$cid.scope" | head -1)
    [[ -n "${CGROUP[$svc]}" ]] || { echo "no cgroup found for $svc" >&2; exit 1; }
done

# mem_kb <service>: PSS of all processes of the container, in kB
mem_kb() {
    local pids
    pids=$(tr '\n' ' ' <"${CGROUP[$1]}/cgroup.procs")
    docker exec "$PROBE" sh -c "for p in $pids; do cat /proc/\$p/smaps_rollup 2>/dev/null; done" |
        awk '/^Pss:/ { s += $2 } END { print s + 0 }'
}
# cpu_usec <service>: CPU time used by the container so far, in microseconds
cpu_usec() { awk '/^usage_usec/ { print $2 }' "${CGROUP[$1]}/cpu.stat"; }

sampler() { # writes "<service> <kB>" every 2 s while $tmp/sampling exists
    while [[ -e "$tmp/sampling" ]]; do
        for svc in "${MEASURED[@]}"; do echo "$svc $(mem_kb "$svc")"; done >>"$tmp/samples"
        sleep 2
    done
}

# measure <command...>: runs the command and sets CPU[service] (average cores), CPU_MS[service]
# (CPU time in ms) and MEM[service] (peak MB) for that time
declare -A CPU CPU_MS MEM
measure() {
    local svc t0 t1
    local -A before
    : >"$tmp/samples"
    touch "$tmp/sampling"
    sampler &
    local sampler_pid=$!
    for svc in "${MEASURED[@]}"; do before[$svc]=$(cpu_usec "$svc"); done
    t0=$(date +%s.%N)
    "$@"
    t1=$(date +%s.%N)
    for svc in "${MEASURED[@]}"; do
        CPU_MS[$svc]=$(awk -v a="${before[$svc]}" -v b="$(cpu_usec "$svc")" 'BEGIN { printf "%.0f", (b - a) / 1000 }')
        CPU[$svc]=$(awk -v ms="${CPU_MS[$svc]}" -v t0="$t0" -v t1="$t1" 'BEGIN { printf "%.2f", ms / 1000 / (t1 - t0) }')
    done
    rm -f "$tmp/sampling"
    wait "$sampler_pid"
    for svc in "${MEASURED[@]}"; do
        MEM[$svc]=$(awk -v s="$svc" '$1 == s && $2 > m { m = $2 } END { printf "%.0f", m / 1024 }' "$tmp/samples")
    done
}

# percentile <p> <file>: the p-th percentile of the numbers in the file
percentile() { sort -n "$2" | awk -v p="$1" '{ a[NR] = $1 } END { i = int(NR * p / 100 + 0.999999); if (i < 1) i = 1; print a[i] }'; }

failures=0

# --- S7 ------------------------------------------------------------------------------------

stream_body=$(jq -cn --argjson n "$TOKENS" \
    '{model: "qwen3", messages: [{role: "user", content: "hello there"}], max_tokens: $n, stream: true}')

stream_worker() { # one streamed request after another until the deadline; a line per request
    local id=$1 end=$2 out
    while (($(date +%s) < end)); do
        out=$(curl -sS -N -m 120 -o "$tmp/body.$id" -w '%{http_code} %{time_starttransfer}' \
            -H 'Authorization: Bearer dev-key-chat-ui' -H 'Content-Type: application/json' \
            -H "X-Mock-Completion-Tokens: $TOKENS" -d "$stream_body" \
            "$GATEWAY/full/v1/chat/completions" 2>/dev/null) || out="000 0"
        if [[ "$out" == 200\ * ]] && grep -q '^data: \[DONE\]' "$tmp/body.$id"; then
            echo "ok ${out#* }"
        else
            echo "failed ${out%% *}"
        fi
    done >"$tmp/result.$id"
}

streams() { # streams <n>: n workers for DURATION seconds
    local n=$1 end pids=()
    end=$(($(date +%s) + DURATION))
    rm -f "$tmp"/result.*
    for id in $(seq 1 "$n"); do
        stream_worker "$id" "$end" &
        pids+=("$!")
    done
    # Not a bare wait: the sampler runs in the background too.
    wait "${pids[@]}"
}

header() {
    printf '%-10s %9s %7s %8s %9s  %-12s %-12s %-12s\n' "" "" "" "usage" "TTFT ms" "apisix" "stub-aisa" "consul-tpl"
    printf '%-10s %9s %7s %8s %9s  %-12s %-12s %-12s\n' "streams" "requests" "failed" "events" "p50/p95" "cores / MB" "cores / MB" "cores / MB"
}
row() { # row <label> <requests> <failed> <events> <ttft>
    printf '%-10s %9s %7s %8s %9s' "$@"
    for svc in "${MEASURED[@]}"; do printf '  %-12s' "${CPU[$svc]} / ${MEM[$svc]}"; done
    printf '\n'
}

workers=$(docker exec "$PROBE" sh -c "for p in $(tr '\n' ' ' <"${CGROUP[apisix]}/cgroup.procs"); do cat /proc/\$p/cmdline | tr '\\0' ' '; echo; done" | grep -c 'worker process' || true)
echo "== S7: footprint under parallel streams"
echo "   $(uname -m), $(nproc) cores, APISIX_WORKERS=$APISIX_WORKERS ($workers worker processes)"
echo "   each stream: $TOKENS tokens at 50 tokens/s through /full/; $DURATION s per row; memory is peak PSS"
header
measure sleep "$DURATION"
row idle - - - -
for n in $STREAMS; do
    curl -fsS -X DELETE "$STUB/debug/requests" >/dev/null
    measure streams "$n"
    sleep 3 # the logger sends an event within a second of the end of a request
    ok=$(cat "$tmp"/result.* | grep -c '^ok' || true)
    bad=$(cat "$tmp"/result.* | grep -c '^failed' || true)
    events=$(curl -fsS "$STUB/debug/requests?kind=usage" |
        jq --argjson n "$TOKENS" '[.[] | .body | select(.status == 200 and .completion_tokens == $n)] | length')
    cat "$tmp"/result.* | awk '$1 == "ok" { print $2 * 1000 }' >"$tmp/ttft"
    ttft="-"
    [[ -s "$tmp/ttft" ]] && ttft=$(printf '%.0f/%.0f' "$(percentile 50 "$tmp/ttft")" "$(percentile 95 "$tmp/ttft")")
    row "$n" "$ok" "$bad" "$events" "$ttft"
    if [[ "$bad" -ne 0 ]] || [[ "$events" -ne "$ok" ]]; then failures=$((failures + 1)); fi
done
measure sleep "$DURATION"
row "idle after" - - - -

# --- S10 -----------------------------------------------------------------------------------

jq -cn '{model: "llama3.2", messages: [{role: "user", content: "hello there"}]}' >"$tmp/latency.json"

batch() { # batch <variant> <count>: requests over one connection; "<status> <seconds>" per request
    local cfg=$tmp/curl.$BASHPID i
    for i in $(seq 1 "$2"); do
        [[ "$i" -gt 1 ]] && echo next
        printf 'url = "%s"\nheader = "Authorization: Bearer dev-key-chat-ui"\nheader = "Content-Type: application/json"\nheader = "X-Mock-Completion-Tokens: 1"\ndata = "@%s"\noutput = "/dev/null"\nwrite-out = "%%{http_code} %%{time_total}\\n"\n' \
            "$GATEWAY/$1/v1/chat/completions" "$tmp/latency.json"
    done >"$cfg"
    curl -sS -K "$cfg"
}
load() { # load <variant> <concurrency> <count per connection>
    local pids=()
    for _ in $(seq 1 "$2"); do
        batch "$1" "$3" >>"$tmp/times.$1.$BASHPID.$RANDOM" &
        pids+=("$!")
    done
    wait "${pids[@]}"
}

echo
echo "== S10: latency of a non-streamed request to a backend that answers at once (ms)"
echo "   $REQUESTS requests per row over kept-alive connections, in $ROUNDS rounds; CPU is per request"
printf '%-8s %-12s %7s %7s %7s %7s %7s  %-10s %-10s\n' "clients" "variant" "p50" "p95" "p99" "mean" "failed" "apisix ms" "stub ms"
for v in direct noauth full; do batch "$v" 50 >/dev/null; done # warm up
for conc in 1 10; do
    per=$((REQUESTS / ROUNDS / conc))
    declare -A cpu_apisix=() cpu_stub=()
    rm -f "$tmp"/times.*
    for _ in $(seq 1 "$ROUNDS"); do
        for v in direct noauth full; do
            measure load "$v" "$conc" "$per"
            cpu_apisix[$v]=$((${cpu_apisix[$v]:-0} + ${CPU_MS[apisix]}))
            cpu_stub[$v]=$((${cpu_stub[$v]:-0} + ${CPU_MS[stub-aisa]}))
        done
    done
    declare -A p50=()
    for v in direct noauth full; do
        cat "$tmp"/times."$v".* >"$tmp/all"
        awk '$1 == 200 { print $2 * 1000 }' "$tmp/all" >"$tmp/ok"
        bad=$(awk '$1 != 200' "$tmp/all" | wc -l)
        total=$(wc -l <"$tmp/all")
        p50[$v]=$(percentile 50 "$tmp/ok")
        printf '%-8s %-12s %7.2f %7.2f %7.2f %7.2f %7s  %-10s %-10s\n' "$conc" "$v" \
            "${p50[$v]}" "$(percentile 95 "$tmp/ok")" "$(percentile 99 "$tmp/ok")" \
            "$(awk '{ s += $1 } END { print s / NR }' "$tmp/ok")" "$bad" \
            "$(awk -v ms="${cpu_apisix[$v]}" -v n="$total" 'BEGIN { printf "%.2f", ms / n }')" \
            "$(awk -v ms="${cpu_stub[$v]}" -v n="$total" 'BEGIN { printf "%.2f", ms / n }')"
        [[ "$bad" -eq 0 ]] || failures=$((failures + 1))
    done
    awk -v d="${p50[direct]}" -v n="${p50[noauth]}" -v f="${p50[full]}" -v c="$conc" \
        'BEGIN { printf "%-8s p50 added by the internal hop: %.2f ms, by forward-auth: %.2f ms\n", c, n - d, f - n }'
done

echo
if [[ "$failures" -eq 0 ]]; then
    echo "all requests succeeded, and every stream has its usage event"
else
    echo "$failures rows with failed requests or missing usage events" >&2
    exit 1
fi
