#!/usr/bin/env bash
# Spike S11: a backend that is gone (#52). How long does a request to it wait on APISIX's
# ai-proxy-multi, and which of the plugin's options shorten it: a retry on another instance
# (fallback_strategy), the retry limited to fast failures (retry_on_failure_within_ms), or
# active health checks on the instances (checks)?
#
#   ./proxies/apisix/spikes/s11-gone-backend/run.sh [requests per route, default 10] [phases, default "gone 5xx"]
#
# Starts the dev stack with this spike's apisix.yaml (compose.override.yaml), sends the requests,
# prints a table and leaves the stack up (docker compose -f dev/compose.yaml -f <this
# directory>/compose.override.yaml down -v to stop). Needs curl.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
n=${1:-10}
phases=${2:-gone 5xx}
COMPOSE=(docker compose -f "$repo/dev/compose.yaml" -f "$here/compose.override.yaml")
GATEWAY=http://127.0.0.1:9080
BODY='{"model":"qwen3","messages":[{"role":"user","content":"hi"}],"max_tokens":5}'

"${COMPOSE[@]}" up -d --wait --force-recreate apisix >/dev/null

# request <route> [body]: one request; prints "<status> <seconds>".
request() {
    curl -s -o /dev/null -m 30 -w '%{http_code} %{time_total}\n' \
        -H 'Content-Type: application/json' -d "${2:-$BODY}" "$GATEWAY/$1/v1/chat/completions"
}

# run <route> [body]: n requests in a row; prints one line of the table.
run() {
    local route=$1 body=${2:-$BODY} line="" status secs ok=0 slow=0 max=0 total=0
    for _ in $(seq 1 "$n"); do
        read -r status secs < <(request "$route" "$body")
        line+="$status:${secs%.*}s "
        total=$(awk -v t="$total" -v s="$secs" 'BEGIN{print t+s}')
        max=$(awk -v m="$max" -v s="$secs" 'BEGIN{print (s>m)?s:m}')
        [[ "$status" = 200 ]] && ok=$((ok + 1))
        awk -v s="$secs" 'BEGIN{exit !(s>=2)}' && slow=$((slow + 1))
    done
    printf '%-26s %3d/%-3d ok  %2d slow (>=2 s)  max %5.1f s  total %6.1f s   %s\n' \
        "$route" "$ok" "$n" "$slow" "$max" "$total" "$line"
}

for phase in $phases; do
    case $phase in
    gone)
        echo "== a backend that is gone: $n requests per route; status:seconds of each"
        for v in plain retry retry-fast checks; do run "unroutable/$v"; done
        run unroutable/single-checks
        for v in plain retry retry-fast checks; do run "refused/$v"; done
        "${COMPOSE[@]}" pause mock-cloud >/dev/null
        trap '"${COMPOSE[@]}" unpause mock-cloud >/dev/null 2>&1 || true' EXIT
        for v in plain retry retry-fast checks; do run "hung/$v"; done
        "${COMPOSE[@]}" unpause mock-cloud >/dev/null
        trap - EXIT
        ;;
    5xx)
        echo "== a backend that answers 503: $n requests per route; status:seconds of each"
        body='{"model":"qwen3","messages":[{"role":"user","content":"mock:error:503"}],"max_tokens":5}'
        for v in single-plain single-retry both-retry; do run "5xx/$v" "$body"; done
        echo "   attempts the gateway made, from its log (falling back = a retry):"
        "${COMPOSE[@]}" logs --no-log-prefix apisix 2>/dev/null | grep -o 'ai instance [a-z0-9]* returned status 503, falling back to [a-z0-9]*' | sort | uniq -c | sed 's/^/   /'
        ;;
    esac
done
echo "== done; the stack is still up"
