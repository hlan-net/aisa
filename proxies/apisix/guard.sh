#!/bin/sh
# Guard script for APISIX configuration rendered by consul-template (#18).
# Checks the staged configuration file, promotes it to the active apisix.yaml path, and then
# checks that APISIX loaded every route of it; if not, it puts the previous file back.
#
# Usage: guard.sh <staged-file> <target-file>
#
# APISIX validates each route against its schemas when it loads the file and leaves out one
# that fails, with an error in its log only. The guard asks APISIX's Control API, which lists
# the routes APISIX loaded, each with the modification time of the file it came from.
#
#   GUARD_CONTROL_URL     APISIX's Control API, from the same pod     http://127.0.0.1:9090
#   GUARD_VERIFY_TIMEOUT  seconds to wait for APISIX to load the file  10
#   GUARD_EMPTY_GRACE     seconds a file without model routes waits    300
#                         before it replaces one that has them
#   GUARD_RECHECK         set when the pod runs this script again during a hold (see 6.)
#   GUARD_LOCK_WAIT       seconds to wait for another guard to finish   20
#                         (consul-template.hcl sets command_timeout to 60 s for the whole guard)
#   AISA_RENDER_URI       where each render is reported, aisa's         unset: no report
#                         POST /v1/render (PROXY_CONTRACT.md, Reporting a render)
#
# guard.sh --resend <target-file> delivers the reports aisa has not taken yet, in order (the
# pod's loop calls it every 30 s; see report below).
#
# consul-template exits when this script fails, and Kubernetes restarts its container, which
# renders again: that is the retry of a rejected file. A hold (below) is not a failure.

set -e

# The lock: one guard at a time, whether consul-template's render command, the pod's recheck
# loop (6.) or a resend, since all of them work on the same files. It is a directory next to
# the target, which mkdir creates atomically, with the owner's PID in it. A guard removes only
# the lock it owns. One whose owner is gone (killed with its container; the loop and
# consul-template share the PID namespace) is taken over; so is one without an owner for more
# than 120 s. take_lock fails after GUARD_LOCK_WAIT seconds.
take_lock() {
    LOCK="$TARGET.lock"
    waited=0
    until mkdir "$LOCK" 2>/dev/null; do
        owner=$(cat "$LOCK/pid" 2>/dev/null || true)
        if [ -n "$owner" ] && ! kill -0 "$owner" 2>/dev/null; then
            echo "guard: warning: taking over the lock '$LOCK' of guard $owner, which is gone" >&2
            rm -f "$LOCK/pid" && rmdir "$LOCK" 2>/dev/null || true
            continue
        fi
        if [ -z "$owner" ] && [ -d "$LOCK" ] && [ $(( $(date +%s) - $(stat -c %Y "$LOCK") )) -gt 120 ]; then
            echo "guard: warning: taking over the lock '$LOCK', without an owner for more than 120 s" >&2
            rmdir "$LOCK" 2>/dev/null || true
            continue
        fi
        if [ "$waited" -ge "${GUARD_LOCK_WAIT:-20}" ]; then
            echo "guard: error: another guard (${owner:-unknown}) has held '$LOCK' for ${GUARD_LOCK_WAIT:-20}s" >&2
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo $$ > "$LOCK/pid"
    trap release EXIT
}
# release: the lock, if still this guard's, and the snapshot of a recheck (below).
SNAPSHOT=""
release() {
    if [ "$(cat "$LOCK/pid" 2>/dev/null)" = "$$" ]; then
        rm -f "$LOCK/pid"
        rmdir "$LOCK" 2>/dev/null || true
    fi
    if [ -n "$SNAPSHOT" ]; then
        rm -f "$SNAPSHOT"
    fi
    # The trap must not change the exit status of the guard.
    return 0
}

# The reports aisa has not taken yet wait in <target>.reports/, one file each, named by a
# sequence so that they go in order; at most 100 of them, the oldest giving way. A render
# while aisa is down (the first one, as the pod and aisa start together) is reported when the
# pod's loop sends the queue again. Each report carries an id, so a retry of one aisa took but
# did not answer counts once.
QUEUE_MAX=100
# send_queue: delivers the queued reports in order and stops at the first aisa does not take,
# so that they stay in order. Each try is bounded, since a render waits for it.
send_queue() {
    [ -n "$AISA_RENDER_URI" ] && [ -d "$TARGET.reports" ] || return 0
    for f in $(ls "$TARGET.reports" 2>/dev/null | grep '\.json$' | sort); do
        if timeout 3 wget -q -O /dev/null -T 3 --header 'Content-Type: application/json' \
            --post-data "$(cat "$TARGET.reports/$f")" "$AISA_RENDER_URI" 2>/dev/null; then
            rm -f "$TARGET.reports/$f"
        else
            echo "guard: warning: the render was not reported to $AISA_RENDER_URI ($(ls "$TARGET.reports" | grep -c '\.json$') waiting); the pod tries again" >&2
            return 0
        fi
    done
}

if [ "$1" = "--resend" ]; then
    TARGET="$2"
    [ -n "$TARGET" ] || { echo "guard: error: usage: guard.sh --resend <target-file>" >&2; exit 1; }
    # Nothing to send: no lock, no noise.
    [ -n "$(ls "$TARGET.reports" 2>/dev/null | grep '\.json$')" ] || exit 0
    # Another guard is running: it sends the queue itself when it reports.
    take_lock || exit 0
    send_queue
    exit 0
fi

STAGED="$1"
TARGET="$2"

if [ -z "$STAGED" ] || [ -z "$TARGET" ]; then
    echo "guard: error: usage: guard.sh <staged-file> <target-file>" >&2
    exit 1
fi

if [ ! -f "$STAGED" ]; then
    if [ -n "$GUARD_RECHECK" ]; then
        # A recheck, and a newer render was promoted meanwhile: nothing to do.
        exit 0
    fi
    echo "guard: error: staged file '$STAGED' does not exist" >&2
    exit 1
fi

take_lock || exit 1

# A recheck works on a snapshot of the staged file: consul-template may replace the staged
# file at any moment outside the lock, and the guard reads it more than once. It checks only
# the render that is held, which the mark of the hold names by its hash (6.): a newer render
# is consul-template's own command's to check, which waits on the lock, and a recheck that
# finds one leaves quietly. The original is removed after a promotion only if it is still what
# was promoted.
EMPTY_SINCE="$TARGET.empty-since"
# hash <file>: names a rendered file.
hash() {
    sha256sum < "$1" | cut -c1-64
}
ORIGINAL="$STAGED"
if [ -n "$GUARD_RECHECK" ]; then
    SNAPSHOT="$STAGED.recheck"
    cp "$STAGED" "$SNAPSHOT"
    STAGED="$SNAPSHOT"
    if [ "$(cut -d' ' -f2 "$EMPTY_SINCE" 2>/dev/null)" != "$(hash "$STAGED")" ]; then
        exit 0
    fi
fi

# promote: the staged file (a recheck's snapshot) becomes the target; the original staged file
# goes too, unless a newer render replaced it meanwhile.
promote() {
    mv -f "$STAGED" "$TARGET"
    SNAPSHOT=""
    if [ "$ORIGINAL" != "$TARGET" ] && [ -f "$ORIGINAL" ] && cmp -s "$ORIGINAL" "$TARGET"; then
        rm -f "$ORIGINAL"
    fi
}
# route_ids <file>: the IDs of the routes, from the items of the top-level routes section only
# (global_rules has IDs too).
route_ids() {
    awk '/^routes:/ { f = 1; next } /^[^[:space:]#]/ { f = 0 } f && /^[[:space:]]*- id:/' "$1" |
        sed -E 's/^[[:space:]]*- id:[[:space:]]*"?([^"]*)"?[[:space:]]*$/\1/'
}
# model_routes <file>: how many routes serve a model (the template prefixes them with "model-").
model_routes() {
    route_ids "$1" | grep -c '^model-' || true
}
# backends <file>: how many backends the routes name (the instances of ai-proxy-multi).
backends() {
    grep -E '^[[:space:]]+- name: ' "$1" | sort -u | wc -l | tr -d ' '
}

# report <result> <reason> <file>: tells aisa what became of this render, with the backends and
# models of <file>, when AISA_RENDER_URI is set. The report is queued and the queue sent, which
# waits for aisa 3 s at most. Reporting never fails the render: whatever goes wrong here is a
# warning. The reason is a fixed phrase and route IDs, quoted for JSON anyway and cut to the
# 1024 bytes aisa takes: a file with many rejected routes names the first ones.
report() {
    [ -n "$AISA_RENDER_URI" ] || return 0
    queue="$TARGET.reports"
    if ! mkdir -p "$queue" 2>/dev/null; then
        echo "guard: warning: the render could not be queued for reporting: cannot create '$queue'" >&2
        return 0
    fi
    seq=$(cat "$queue/.seq" 2>/dev/null || echo 0)
    case "$seq" in ''|*[!0-9]*) seq=0 ;; esac
    seq=$((seq + 1))
    name=$(printf '%010d' "$seq")
    id="$name-$(head -c 6 /dev/urandom 2>/dev/null | od -An -tx1 | tr -d ' \n')"
    reason=$(printf '%.1000s' "$2" | tr '\n' ' ' | sed 's/\\/\\\\/g; s/"/\\"/g')
    if ! printf '{"id":"%s","ts":"%s","result":"%s","reason":"%s","backends":%s,"models":%s}\n' \
        "$id" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" "$reason" "$(backends "$3")" "$(model_routes "$3")" > "$queue/$name.tmp" 2>/dev/null ||
        ! mv -f "$queue/$name.tmp" "$queue/$name.json" 2>/dev/null; then
        echo "guard: warning: the render could not be queued for reporting: cannot write in '$queue'" >&2
        rm -f "$queue/$name.tmp"
        return 0
    fi
    echo "$seq" > "$queue/.seq" 2>/dev/null || true
    # The oldest give way beyond QUEUE_MAX.
    for old in $(ls "$queue" | grep '\.json$' | sort | head -n -"$QUEUE_MAX"); do
        rm -f "$queue/$old"
    done
    send_queue
}
# reject <message>: the staged file is not promoted; the current one stays.
reject() {
    echo "guard: error: $1" >&2
    report rejected "$1" "$STAGED" || true
    exit 1
}

if [ ! -s "$STAGED" ]; then
    reject "staged file '$STAGED' is empty"
fi

# 1. Ensure file write is complete (ends with #END marker).
if ! tail -n 5 "$STAGED" | grep -q '^#END'; then
    reject "staged file '$STAGED' is incomplete (missing #END marker)"
fi

# 2. Ensure routes block exists.
if ! grep -q '^[[:space:]]*routes:' "$STAGED"; then
    reject "staged file '$STAGED' has no routes section"
fi

# 3. Ensure client-facing route exists (prevents dropping client route on empty/broken catalog).
if ! grep -E -q '^[[:space:]]*- id: *"client"' "$STAGED" && ! grep -E -q '^[[:space:]]*- id: *client' "$STAGED"; then
    reject "staged file '$STAGED' is missing required client route (id: client)"
fi

# 4. Ensure fallback no-backend route exists.
if ! grep -E -q '^[[:space:]]*- id: *"no-backend"' "$STAGED" && ! grep -E -q '^[[:space:]]*- id: *no-backend' "$STAGED"; then
    reject "staged file '$STAGED' is missing required fallback route (id: no-backend)"
fi

# 5. Check for duplicate route IDs.
DUPLICATES=$(grep -E '^[[:space:]]*- id:' "$STAGED" | awk '{print $NF}' | sort | uniq -d || true)
if [ -n "$DUPLICATES" ]; then
    reject "staged file '$STAGED' contains duplicate route IDs: $(echo "$DUPLICATES" | tr '\n' ' ')"
fi

CONTROL="${GUARD_CONTROL_URL:-http://127.0.0.1:9090}"
TIMEOUT="${GUARD_VERIFY_TIMEOUT:-10}"
GRACE="${GUARD_EMPTY_GRACE:-300}"
IDS=$(route_ids "$STAGED")

# The first render, before APISIX starts: nothing to compare with and nothing to go back to.
if [ ! -f "$TARGET" ]; then
    promote
    echo "guard: promoted '$ORIGINAL' -> '$TARGET' (first render, not checked against APISIX)"
    report promoted "" "$TARGET" || true
    exit 0
fi

# 6. A file without model routes replaces one that has them only after a grace period
# (PROXY_CONTRACT.md, Checking the rendered config). The catalog can come back empty although
# the backends are fine: a render token that may not read the service, a Consul that lost its
# data. Until the period is over the current file stays, marked by the time the hold began.
# consul-template runs this script again only when the rendered file changes, so the pod runs
# it again every 30 s while the mark exists (chart/templates/deployment.yaml), with
# GUARD_RECHECK set; outside the pod, the tests do. A hold is not a failure: exit 0.
# The mark of a hold holds when it began and the hash of the held render; a render during the
# hold keeps the time and names itself.
NOW=$(date +%s)
if [ "$(model_routes "$STAGED")" -eq 0 ] && [ "$(model_routes "$TARGET")" -gt 0 ]; then
    SINCE=$(cut -d' ' -f1 "$EMPTY_SINCE" 2>/dev/null || true)
    case "$SINCE" in
        ''|*[!0-9]*) SINCE=$NOW ;;
    esac
    if [ -z "$GUARD_RECHECK" ]; then
        echo "$SINCE $(hash "$STAGED")" > "$EMPTY_SINCE"
    fi
    REMAINING=$((GRACE - (NOW - SINCE)))
    if [ "$REMAINING" -gt 0 ]; then
        if [ -z "$GUARD_RECHECK" ]; then
            # A recheck that finds the hold still on is quiet, and reports nothing: one line and
            # one report per render, not per 30 s.
            HOLD="the rendered file has no model route; the current file with $(model_routes "$TARGET") stays for another ${REMAINING}s (GUARD_EMPTY_GRACE=$GRACE)"
            echo "guard: warning: $HOLD" >&2
            report held "$HOLD" "$STAGED" || true
        fi
        exit 0
    fi
    echo "guard: warning: the rendered file has had no model route for $((NOW - SINCE))s; promoting it, every model now gets the gateway's 503" >&2
fi
rm -f "$EMPTY_SINCE"

# APISIX reloads the file when its modification time, in seconds, changes, and reports that
# time as the modifiedIndex of every route it loaded from it. Two renders within a second
# would look alike, so the new file is made newer than the current one. (Not more than twice:
# a current file dated in the future is not waited for; the check below then finds the routes
# not loaded and puts that file back.)
for _ in 1 2; do
    [ "$(stat -c %Y "$STAGED")" -gt "$(stat -c %Y "$TARGET")" ] && break
    sleep 1
    touch "$STAGED"
done
VERSION=$(stat -c %Y "$STAGED")
LAST_GOOD="$TARGET.last-good"
cp "$TARGET" "$LAST_GOOD"
promote

# The whole verification, every call of the Control API included, ends by this time: a slow
# Control API cannot keep the guard past consul-template's command_timeout.
DEADLINE=$(( $(date +%s) + TIMEOUT ))
# remaining: the seconds left to the deadline, at least 1.
remaining() {
    r=$(( DEADLINE - $(date +%s) ))
    [ "$r" -gt 0 ] && echo "$r" || echo 1
}
# missing: prints the routes of the new file that APISIX has not loaded, or "unreachable".
# Each call is bounded by the time left, and none is made once the deadline has passed: the
# routes not asked about by then count as missing.
missing() {
    if ! timeout "$(remaining)" wget -q -O /dev/null -T "$(remaining)" "$CONTROL/v1/routes" 2>/dev/null; then
        echo unreachable
        return
    fi
    for id in $IDS; do
        if [ "$(date +%s)" -ge "$DEADLINE" ]; then
            echo "$id"
            continue
        fi
        loaded=$(timeout "$(remaining)" wget -q -O - -T "$(remaining)" "$CONTROL/v1/route/$id" 2>/dev/null | sed -n 's/.*"modifiedIndex":\([0-9]*\).*/\1/p')
        [ "$loaded" = "$VERSION" ] || echo "$id"
    done
}

while :; do
    MISSING=$(missing)
    [ -z "$MISSING" ] && break
    if [ "$(date +%s)" -ge "$DEADLINE" ]; then
        break
    fi
    sleep 1
done

if [ -z "$MISSING" ]; then
    rm -f "$LAST_GOOD"
    echo "guard: promoted '$TARGET'; APISIX loaded all $(echo "$IDS" | wc -w) routes"
    report promoted "" "$TARGET" || true
    exit 0
fi

if [ "$MISSING" = "unreachable" ]; then
    # APISIX may be restarting. Without an answer there is no evidence against the new file,
    # which passed the checks above, so it stays.
    rm -f "$LAST_GOOD"
    echo "guard: warning: promoted '$TARGET', but APISIX's Control API at $CONTROL did not answer; the routes are not checked" >&2
    report promoted "" "$TARGET" || true
    exit 0
fi

# APISIX left routes out: go back to the previous file. A copy gets a new modification time,
# so APISIX loads it again.
cp "$TARGET" "$TARGET.rejected"
cp "$LAST_GOOD" "$TARGET.restore"
mv -f "$TARGET.restore" "$TARGET"
rm -f "$LAST_GOOD"
WHY="APISIX did not load these routes of the new file: $(echo "$MISSING" | tr '\n' ' '); the previous file is back in place (see APISIX's log for the schema error)"
echo "guard: error: $WHY" >&2
report rejected "$WHY" "$TARGET.rejected" || true
rm -f "$TARGET.rejected"
exit 1
