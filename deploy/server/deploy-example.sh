#!/usr/bin/env bash
#
# Usage: ./deploy/deploy-example.sh <USER>@<IP> <example>
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ $# -lt 2 ]]; then
    echo "Usage: $0 <USER>@<IP> <example>"
    echo "Example: $0 ubuntu@206.223.236.99 kv"
    exit 1
fi

TARGET="$1"
EXAMPLE="$2"
EXAMPLE_DIR="$PROJECT_ROOT/examples/$EXAMPLE"
REMOTE_DIR="~/.openthesis/examples/$EXAMPLE"

if [[ ! -d "$EXAMPLE_DIR" ]]; then
    echo "!!! Example directory not found: $EXAMPLE_DIR"
    exit 1
fi

cd "$PROJECT_ROOT"

if [[ -f "$EXAMPLE_DIR/download.sh" ]]; then
    echo "==> Running download.sh for $EXAMPLE (local)"
    bash "$EXAMPLE_DIR/download.sh" || true
fi

echo "==> Building $EXAMPLE example binaries for linux/amd64"

BINS=()
for cmd_dir in "$EXAMPLE_DIR"/cmd/*/; do
    cmd_name="$(basename "$cmd_dir")"
    if ! ls "$cmd_dir"*.go &>/dev/null; then
        echo "    Skipping $cmd_name (no Go files)"
        continue
    fi
    out="bin/examples/$cmd_name"
    echo "    Building $cmd_name"
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$out" "./examples/$EXAMPLE/cmd/$cmd_name"
    BINS+=("$out")
    echo "    $(du -h "$out" | cut -f1) $cmd_name"
done

echo "==> Uploading to $TARGET:$REMOTE_DIR"

ssh "$TARGET" bash -c "'
    if [[ \"\$(id -u)\" -ne 0 ]]; then SUDO=sudo; else SUDO=; fi
    \$SUDO mkdir -p $REMOTE_DIR
    \$SUDO chown -R \$(id -un):\$(id -gn) $REMOTE_DIR
'"

for bin in "${BINS[@]}"; do
    rsync -azP "$bin" "$TARGET:$REMOTE_DIR/$(basename "$bin")"
done

if [[ -d "$EXAMPLE_DIR/bin" ]]; then
    echo "==> Uploading pre-built binaries from $EXAMPLE_DIR/bin/"
    ssh "$TARGET" bash -c "'mkdir -p $REMOTE_DIR/bin'"
    rsync -azP "$EXAMPLE_DIR/bin/" "$TARGET:$REMOTE_DIR/bin/"
fi

if [[ -d "$EXAMPLE_DIR/tests" ]]; then
    rsync -azP "$EXAMPLE_DIR/tests/" "$TARGET:$REMOTE_DIR/tests/"
fi

if [[ -d "$EXAMPLE_DIR/scripts" ]]; then
    rsync -azP "$EXAMPLE_DIR/scripts/" "$TARGET:$REMOTE_DIR/scripts/"
fi

if [[ -f "$EXAMPLE_DIR/sut.ext4" ]]; then
    echo "==> Uploading sut.ext4 ($(du -h "$EXAMPLE_DIR/sut.ext4" | cut -f1))"
    rsync -azP --checksum "$EXAMPLE_DIR/sut.ext4" "$TARGET:$REMOTE_DIR/sut.ext4"
fi

if [[ -f "$EXAMPLE_DIR/openthesis.json" ]]; then
    rsync -azP "$EXAMPLE_DIR/openthesis.json" "$TARGET:$REMOTE_DIR/openthesis.json"
fi

if [[ -f "$EXAMPLE_DIR/download.sh" ]]; then
    rsync -azP "$EXAMPLE_DIR/download.sh" "$TARGET:$REMOTE_DIR/download.sh"
fi

ssh "$TARGET" bash -c "'
    find $REMOTE_DIR -maxdepth 1 -type f ! -name \"*.json\" ! -name \"*.ext4\" -exec chmod +x {} \\;
    chmod +x $REMOTE_DIR/tests/* 2>/dev/null || true
    chmod +x $REMOTE_DIR/scripts/* 2>/dev/null || true
'"

if [[ -f "$EXAMPLE_DIR/download.sh" && ! -f "$EXAMPLE_DIR/sut.ext4" ]]; then
    echo "==> Running download.sh on server (requires root, server-side build)..."
    ssh "$TARGET" bash -c "'sudo bash $REMOTE_DIR/download.sh'" || {
        echo "!!! Server-side download.sh failed - run manually:"
        echo "    ssh $TARGET"
        echo "    sudo bash $REMOTE_DIR/download.sh"
    }
fi

echo "==> Example $EXAMPLE deployed to $REMOTE_DIR"
