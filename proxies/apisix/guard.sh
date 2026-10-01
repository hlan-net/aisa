#!/bin/sh
# Guard script for APISIX configuration rendered by consul-template (#18).
# Validates the staged configuration file before promoting it to the active apisix.yaml path.
#
# Usage: guard.sh <staged-file> <target-file>

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

# Validation passed: atomically promote the staged file to the target path.
mv -f "$STAGED" "$TARGET"
echo "guard: promoted '$STAGED' -> '$TARGET' successfully"
