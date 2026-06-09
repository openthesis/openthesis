#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REDIS_IMAGE="redis:7.2-bookworm"
BIN_DIR="$SCRIPT_DIR/bin"

mkdir -p "$BIN_DIR"

if [[ -f "$BIN_DIR/redis-server" && -f "$BIN_DIR/redis-cli" ]]; then
    echo "redis binaries already present in $BIN_DIR"
    exit 0
fi

echo "Pulling $REDIS_IMAGE and extracting binaries..."

CONTAINER=$(docker create "$REDIS_IMAGE")
trap 'docker rm "$CONTAINER" >/dev/null 2>&1 || true' EXIT

docker cp "$CONTAINER:/usr/local/bin/redis-server" "$BIN_DIR/redis-server"
docker cp "$CONTAINER:/usr/local/bin/redis-cli"    "$BIN_DIR/redis-cli"

chmod +x "$BIN_DIR/redis-server" "$BIN_DIR/redis-cli"

echo "redis-server: $BIN_DIR/redis-server ($(du -h "$BIN_DIR/redis-server" | cut -f1))"
echo "redis-cli:    $BIN_DIR/redis-cli    ($(du -h "$BIN_DIR/redis-cli"    | cut -f1))"
echo ""
echo "Build the driver:"
echo "  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o redis-driver ./cmd/redis-driver"
