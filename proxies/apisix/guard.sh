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

set -e

STAGED="$1"
TARGET="$2"

if [ -z "$STAGED" ] || [ -z "$TARGET" ]; then
    echo "guard: error: usage: guard.sh <staged-file> <target-file>" >&2
    exit 1
fi

if [ ! -f "$STAGED" ]; then
    echo "guard: error: staged file '$STAGED' does not exist" >&2
    exit 1
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
# The IDs of the routes: the items of the top-level routes section only (global_rules has IDs too).
IDS=$(awk '/^routes:/ { f = 1; next } /^[^[:space:]#]/ { f = 0 } f && /^[[:space:]]*- id:/' "$STAGED" |
    sed -E 's/^[[:space:]]*- id:[[:space:]]*"?([^"]*)"?[[:space:]]*$/\1/')

# The first render, before APISIX starts: nothing to compare with and nothing to go back to.
if [ ! -f "$TARGET" ]; then
    mv -f "$STAGED" "$TARGET"
    echo "guard: promoted '$STAGED' -> '$TARGET' (first render, not checked against APISIX)"
    exit 0
fi

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
mv -f "$STAGED" "$TARGET"

# missing: prints the routes of the new file that APISIX has not loaded, or "unreachable".
missing() {
    if ! wget -q -O /dev/null "$CONTROL/v1/routes" 2>/dev/null; then
        echo unreachable
        return
    fi
    for id in $IDS; do
        loaded=$(wget -q -O - "$CONTROL/v1/route/$id" 2>/dev/null | sed -n 's/.*"modifiedIndex":\([0-9]*\).*/\1/p')
        [ "$loaded" = "$VERSION" ] || echo "$id"
    done
}

waited=0
while :; do
    MISSING=$(missing)
    [ -z "$MISSING" ] && break
    if [ "$waited" -ge "$TIMEOUT" ]; then
        break
    fi
    sleep 1
    waited=$((waited + 1))
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
