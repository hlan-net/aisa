#!/bin/sh
# Lists available updates for the Go toolchain and every module dependency.
set -e

REPO_ROOT=$(git rev-parse --show-toplevel)
cd "$REPO_ROOT"

if [ ! -f go.mod ]; then
    echo "No go.mod yet; nothing to check."
    exit 0
fi

echo "== Go toolchain"
echo "go.mod:  $(awk '/^go /{print $2}' go.mod)"
echo "local:   $(go env GOVERSION)"
echo "latest:  $(curl -fsSL --proto '=https' --tlsv1.2 'https://go.dev/VERSION?m=text' | head -n1)"

echo
echo "== Modules with updates (current [available])"
go list -m -u -f '{{if and (not .Main) .Update}}{{.Path}} {{.Version}} [{{.Update.Version}}]{{end}}' all
