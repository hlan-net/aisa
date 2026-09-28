#!/bin/sh
set -e

REPO_ROOT=$(git rev-parse --show-toplevel)
HOOKS_DIR="$REPO_ROOT/.githooks"
TARGET_DIR="$REPO_ROOT/.git/hooks"

if [ ! -d "$HOOKS_DIR" ]; then
    echo "Missing $HOOKS_DIR"
    exit 1
fi

if [ ! -d "$TARGET_DIR" ]; then
    echo "Missing $TARGET_DIR"
    exit 1
fi

cp "$HOOKS_DIR/pre-push" "$TARGET_DIR/pre-push"
chmod +x "$TARGET_DIR/pre-push"

echo "Installed pre-push hook."
