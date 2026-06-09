#!/usr/bin/env bash
#
# Usage: ./deploy/deploy.sh <USER>@<IP>
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <USER>@<IP>"
    echo "Example: $0 ubuntu@206.223.236.99"
    exit 1
fi

TARGET="$1"
IP="$(echo "$TARGET" | cut -d@ -f2)"
SSH_USER="$(echo "$TARGET" | cut -d@ -f1)"

cd "$PROJECT_ROOT"

echo "==> Building openthesis for linux/amd64"
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
    -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
    -o bin/openthesis-linux-amd64 \
    ./cmd/openthesis

echo "    openthesis: $(du -h bin/openthesis-linux-amd64 | cut -f1)"

echo "==> Building openthesis-init for linux/amd64 (Rust)"
if command -v cargo &>/dev/null && \
   rustup target list --installed 2>/dev/null | grep -q x86_64-unknown-linux-musl && \
   command -v x86_64-linux-musl-gcc &>/dev/null && \
   cargo build --release --target x86_64-unknown-linux-musl \
       --manifest-path cmd/openthesis-init-rs/Cargo.toml \
       2>/dev/null; then
    cp cmd/openthesis-init-rs/target/x86_64-unknown-linux-musl/release/openthesis-init \
       bin/openthesis-init-linux-amd64
    echo "    openthesis-init (Rust local): $(du -h bin/openthesis-init-linux-amd64 | cut -f1)"
elif [[ -f bin/openthesis-init-linux-amd64 ]]; then
    echo "    openthesis-init (pre-built): $(du -h bin/openthesis-init-linux-amd64 | cut -f1)"
else
    echo "    Rust build unavailable; falling back to Go guest agent"
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build \
        -o bin/openthesis-init-linux-amd64 \
        ./cmd/openthesis-init
    echo "    openthesis-init (Go): $(du -h bin/openthesis-init-linux-amd64 | cut -f1)"
fi

echo "==> Syncing to $IP"

rsync -azP --checksum \
    bin/openthesis-linux-amd64 \
    "$TARGET:/tmp/openthesis-new"

rsync -azP --checksum \
    bin/openthesis-init-linux-amd64 \
    "$TARGET:/tmp/openthesis-init-new"

echo "==> Activating on server"

ssh "$TARGET" bash <<'REMOTE'
set -euo pipefail

mkdir -p "$HOME/.openthesis/bin"

if [[ -f "$HOME/.openthesis/bin/openthesis" ]]; then
    cp "$HOME/.openthesis/bin/openthesis" "$HOME/.openthesis/bin/openthesis.prev"
fi

mv /tmp/openthesis-new "$HOME/.openthesis/bin/openthesis"
chmod 755 "$HOME/.openthesis/bin/openthesis"

mv /tmp/openthesis-init-new "$HOME/.openthesis/bin/openthesis-init"
chmod 755 "$HOME/.openthesis/bin/openthesis-init"

# Symlink into PATH if writable; skip silently if not.
if [[ -w /usr/local/bin ]]; then
    ln -sf "$HOME/.openthesis/bin/openthesis" /usr/local/bin/openthesis
fi

"$HOME/.openthesis/bin/openthesis" doctor 2>&1 || true

REMOTE

echo "==> Deploy complete"
