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

# No APISIX here: the checks of the file itself come first. The Control API part is tested at
# the end, against a fake one.
export GUARD_CONTROL_URL=http://127.0.0.1:1 GUARD_VERIFY_TIMEOUT=1

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
if [[ "$(cat "$TARGET")" != "initial-good-config" ]]; then
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

echo "== guard.sh: first render, before APISIX runs"
rm -f "$TARGET"
cat << 'EOF' > "$STAGED"
routes:
  - id: client
  - id: no-backend
#END
EOF
"$GUARD" "$STAGED" "$TARGET" >/dev/null
grep -q 'id: client' "$TARGET" || { echo "FAIL: the first render was not promoted" >&2; exit 1; }

# A fake Control API that serves one file per route, as APISIX's does: a route it loaded, with
# the modification time of the file it came from as modifiedIndex.
CONTROL="${TMPDIR}/control"
mkdir -p "$CONTROL/v1/route"
echo '[]' > "$CONTROL/v1/routes"
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$CONTROL" >/dev/null 2>&1 &
SERVER=$!
trap 'kill $SERVER 2>/dev/null; rm -rf "$TMPDIR"' EXIT
export GUARD_CONTROL_URL="http://127.0.0.1:$PORT"
for _ in $(seq 1 20); do curl -fs -o /dev/null "$GUARD_CONTROL_URL/v1/routes" && break; sleep 0.2; done

# loaded <version> <id>...: APISIX has loaded these routes from the file of that version.
loaded() {
  local version=$1
  shift
  rm -f "$CONTROL"/v1/route/*
  for id in "$@"; do
    echo "{\"modifiedIndex\":$version,\"key\":\"/routes/$id\",\"value\":{\"id\":\"$id\"}}" > "$CONTROL/v1/route/$id"
  done
}
stage() {
  cat << 'EOF' > "$STAGED"
global_rules:
  - id: metrics
routes:
  - id: client
  - id: "model-qwen3"
  - id: no-backend
#END
EOF
  touch -d @1700000100 "$STAGED"
}

echo "== guard.sh: APISIX loaded every route of the new file"
echo "previous-config" > "$TARGET"
touch -d @1700000000 "$TARGET"
stage
loaded 1700000100 client model-qwen3 no-backend
"$GUARD" "$STAGED" "$TARGET" >/dev/null
grep -q 'model-qwen3' "$TARGET" || { echo "FAIL: a file APISIX loaded was not kept" >&2; exit 1; }
[[ ! -e "$TARGET.last-good" ]] || { echo "FAIL: the copy of the previous file was left behind" >&2; exit 1; }

echo "== guard.sh: APISIX left a route out"
echo "previous-config" > "$TARGET"
touch -d @1700000000 "$TARGET"
stage
loaded 1700000100 client no-backend
if out=$("$GUARD" "$STAGED" "$TARGET" 2>&1); then
  echo "FAIL: a file with a route APISIX left out was accepted" >&2
  exit 1
fi
[[ "$(cat "$TARGET")" = "previous-config" ]] || { echo "FAIL: the previous file was not put back" >&2; exit 1; }
grep -q 'model-qwen3' <<< "$out" || { echo "FAIL: the error does not name the route: $out" >&2; exit 1; }

echo "== guard.sh: APISIX still runs the previous file"
echo "previous-config" > "$TARGET"
touch -d @1700000000 "$TARGET"
stage
loaded 1600000000 client model-qwen3 no-backend
if "$GUARD" "$STAGED" "$TARGET" 2>/dev/null; then
  echo "FAIL: routes of an older file were taken for the new one's" >&2
  exit 1
fi
[[ "$(cat "$TARGET")" = "previous-config" ]] || { echo "FAIL: the previous file was not put back" >&2; exit 1; }

echo "== guard.sh: a new file with the same modification time as the current one"
stage
cp -p "$STAGED" "$TARGET"
"$GUARD" "$STAGED" "$TARGET" >/dev/null 2>&1 || true
[[ "$(stat -c %Y "$TARGET")" -gt 1700000100 ]] || { echo "FAIL: the new file is not newer than the current one" >&2; exit 1; }

echo "== guard.sh: the Control API does not answer"
kill $SERVER; wait $SERVER 2>/dev/null || true
echo "previous-config" > "$TARGET"
touch -d @1700000000 "$TARGET"
stage
if ! out=$("$GUARD" "$STAGED" "$TARGET" 2>&1); then
  echo "FAIL: a file that passed the checks was rejected without an answer from APISIX: $out" >&2
  exit 1
fi
grep -q 'model-qwen3' "$TARGET" || { echo "FAIL: the new file was not kept" >&2; exit 1; }
grep -q 'not checked' <<< "$out" || { echo "FAIL: no warning that the routes are not checked: $out" >&2; exit 1; }

echo "ok: all guard tests passed"
