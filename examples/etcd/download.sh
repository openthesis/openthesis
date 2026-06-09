#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ETCD_VERSION="v3.5.17"
BIN_DIR="$SCRIPT_DIR/bin"

mkdir -p "$BIN_DIR"

if [[ -f "$BIN_DIR/etcd" ]]; then
    echo "etcd already downloaded"
    exit 0
fi

TMP=$(mktemp -d)
trap 'rm -rf $TMP' EXIT

echo "Downloading etcd $ETCD_VERSION..."
curl -fsSL "https://github.com/etcd-io/etcd/releases/download/$ETCD_VERSION/etcd-$ETCD_VERSION-linux-amd64.tar.gz" -o "$TMP/etcd.tar.gz"
tar -C "$TMP" -xzf "$TMP/etcd.tar.gz" "etcd-$ETCD_VERSION-linux-amd64/etcd"
mv "$TMP/etcd-$ETCD_VERSION-linux-amd64/etcd" "$BIN_DIR/etcd"
chmod +x "$BIN_DIR/etcd"

echo "etcd binary: $BIN_DIR/etcd ($(du -h "$BIN_DIR/etcd" | cut -f1))"
