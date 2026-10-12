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
#
# consul-template exits when this script fails, and Kubernetes restarts its container, which
# renders again: that is the retry of a rejected file. A hold (below) is not a failure.

set -e

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

# One guard at a time: consul-template's render command and the pod's recheck loop (6.) may
# both run this script, on the same files. The lock is a directory next to the target, which
# mkdir creates atomically, with the owner's PID in it. A guard removes only the lock it owns.
# One whose owner is gone (killed with its container; the loop and consul-template share the
# PID namespace) is taken over; so is one without an owner for more than 120 s.
LOCK="$TARGET.lock"
LOCK_WAIT="${GUARD_LOCK_WAIT:-20}"
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
    if [ "$waited" -ge "$LOCK_WAIT" ]; then
        echo "guard: error: another guard (${owner:-unknown}) has held '$LOCK' for ${LOCK_WAIT}s" >&2
        exit 1
    fi
    sleep 1
    waited=$((waited + 1))
done
echo $$ > "$LOCK/pid"
# release: the lock, if still this guard's, and the snapshot of a recheck (below).
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
SNAPSHOT=""
trap release EXIT

# A recheck works on a snapshot of the staged file: consul-template may replace the staged
# file at any moment outside the lock, and the guard reads it more than once. The original is
# removed after a promotion only if it is still what was promoted; otherwise it is a newer
# render, which consul-template's own command, waiting on the lock, checks next.
ORIGINAL="$STAGED"
if [ -n "$GUARD_RECHECK" ]; then
    SNAPSHOT="$STAGED.recheck"
    cp "$STAGED" "$SNAPSHOT"
    STAGED="$SNAPSHOT"
fi

if [ ! -s "$STAGED" ]; then
    echo "guard: error: staged file '$STAGED' is empty" >&2
    exit 1
fi

# 1. Ensure file write is complete (ends with #END marker).
if ! tail -n 5 "$STAGED" | grep -q '^#END'; then
    echo "guard: error: staged file '$STAGED' is incomplete (missing #END marker)" >&2
    exit 1
fi

# 2. Ensure routes block exists.
if ! grep -q '^[[:space:]]*routes:' "$STAGED"; then
    echo "guard: error: staged file '$STAGED' has no routes section" >&2
    exit 1
fi

# 3. Ensure client-facing route exists (prevents dropping client route on empty/broken catalog).
if ! grep -E -q '^[[:space:]]*- id: *"client"' "$STAGED" && ! grep -E -q '^[[:space:]]*- id: *client' "$STAGED"; then
    echo "guard: error: staged file '$STAGED' is missing required client route (id: client)" >&2
    exit 1
fi

# 4. Ensure fallback no-backend route exists.
if ! grep -E -q '^[[:space:]]*- id: *"no-backend"' "$STAGED" && ! grep -E -q '^[[:space:]]*- id: *no-backend' "$STAGED"; then
    echo "guard: error: staged file '$STAGED' is missing required fallback route (id: no-backend)" >&2
    exit 1
fi

# 5. Check for duplicate route IDs.
DUPLICATES=$(grep -E '^[[:space:]]*- id:' "$STAGED" | awk '{print $NF}' | sort | uniq -d || true)
if [ -n "$DUPLICATES" ]; then
    echo "guard: error: staged file '$STAGED' contains duplicate route IDs: $DUPLICATES" >&2
    exit 1
fi

CONTROL="${GUARD_CONTROL_URL:-http://127.0.0.1:9090}"
TIMEOUT="${GUARD_VERIFY_TIMEOUT:-10}"
GRACE="${GUARD_EMPTY_GRACE:-300}"
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
IDS=$(route_ids "$STAGED")

# The first render, before APISIX starts: nothing to compare with and nothing to go back to.
if [ ! -f "$TARGET" ]; then
    promote
    echo "guard: promoted '$ORIGINAL' -> '$TARGET' (first render, not checked against APISIX)"
    exit 0
fi

# 6. A file without model routes replaces one that has them only after a grace period
# (PROXY_CONTRACT.md, Checking the rendered config). The catalog can come back empty although
# the backends are fine: a render token that may not read the service, a Consul that lost its
# data. Until the period is over the current file stays, marked by the time the hold began.
# consul-template runs this script again only when the rendered file changes, so the pod runs
# it again every 30 s while the mark exists (chart/templates/deployment.yaml), with
# GUARD_RECHECK set; outside the pod, the tests do. A hold is not a failure: exit 0.
EMPTY_SINCE="$TARGET.empty-since"
NOW=$(date +%s)
if [ "$(model_routes "$STAGED")" -eq 0 ] && [ "$(model_routes "$TARGET")" -gt 0 ]; then
    if [ ! -f "$EMPTY_SINCE" ]; then
        echo "$NOW" > "$EMPTY_SINCE"
    fi
    SINCE=$(cat "$EMPTY_SINCE")
    case "$SINCE" in
        ''|*[!0-9]*) SINCE=$NOW; echo "$NOW" > "$EMPTY_SINCE" ;;
    esac
    REMAINING=$((GRACE - (NOW - SINCE)))
    if [ "$REMAINING" -gt 0 ]; then
        if [ -z "$GUARD_RECHECK" ]; then
            # A recheck that finds the hold still on is quiet: one line per render, not per 30 s.
            echo "guard: warning: the rendered file has no model route; the current file with $(model_routes "$TARGET") stays for another ${REMAINING}s (GUARD_EMPTY_GRACE=$GRACE)" >&2
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
    exit 0
fi

if [ "$MISSING" = "unreachable" ]; then
    # APISIX may be restarting. Without an answer there is no evidence against the new file,
    # which passed the checks above, so it stays.
    rm -f "$LAST_GOOD"
    echo "guard: warning: promoted '$TARGET', but APISIX's Control API at $CONTROL did not answer; the routes are not checked" >&2
    exit 0
fi

# APISIX left routes out: go back to the previous file. A copy gets a new modification time,
# so APISIX loads it again.
cp "$LAST_GOOD" "$TARGET.restore"
mv -f "$TARGET.restore" "$TARGET"
rm -f "$LAST_GOOD"
echo "guard: error: APISIX did not load these routes of the new file: $(echo "$MISSING" | tr '\n' ' '); the previous file is back in place (see APISIX's log for the schema error)" >&2
exit 1
