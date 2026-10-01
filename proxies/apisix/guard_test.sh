#!/usr/bin/env bash
# Unit tests for proxies/apisix/guard.sh (#18).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${SCRIPT_DIR}/guard.sh"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

TARGET="${TMPDIR}/apisix.yaml"
STAGED="${TMPDIR}/staged.yaml"
echo "initial-good-config" > "$TARGET"

echo "== guard.sh: valid configuration"
cat << 'EOF' > "$STAGED"
routes:
  - id: client
    uri: /v1/chat/completions
  - id: "no-backend"
    uri: /v1/chat/completions
#END
EOF
"$GUARD" "$STAGED" "$TARGET"
if ! grep -q 'id: client' "$TARGET"; then
  echo "FAIL: valid file was not promoted" >&2
  exit 1
fi

echo "== guard.sh: incomplete file (missing #END)"
echo "initial-good-config" > "$TARGET"
cat << 'EOF' > "$STAGED"
routes:
  - id: client
  - id: no-backend
EOF
if "$GUARD" "$STAGED" "$TARGET" 2>/dev/null; then
  echo "FAIL: incomplete file should be rejected" >&2
  exit 1
fi
if [ "$(cat "$TARGET")" != "initial-good-config" ]; then
  echo "FAIL: target was modified on failure" >&2
  exit 1
fi

echo "== guard.sh: missing client route"
cat << 'EOF' > "$STAGED"
routes:
  - id: other-route
  - id: no-backend
#END
EOF
if "$GUARD" "$STAGED" "$TARGET" 2>/dev/null; then
  echo "FAIL: missing client route should be rejected" >&2
  exit 1
fi

echo "== guard.sh: duplicate route IDs"
cat << 'EOF' > "$STAGED"
routes:
  - id: client
  - id: client
  - id: no-backend
#END
EOF
if "$GUARD" "$STAGED" "$TARGET" 2>/dev/null; then
  echo "FAIL: duplicate route IDs should be rejected" >&2
  exit 1
fi

echo "== guard.sh: empty file"
: > "$STAGED"
if "$GUARD" "$STAGED" "$TARGET" 2>/dev/null; then
  echo "FAIL: empty file should be rejected" >&2
  exit 1
fi

echo "ok: all guard tests passed"
