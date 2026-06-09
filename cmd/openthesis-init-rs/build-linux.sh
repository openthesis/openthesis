#!/bin/bash
# Build the Rust guest agent for Linux x86_64 (static musl binary).
# Run this on a Linux machine or use cross-compilation with musl toolchain.
#
# On the server:
#   cd openthesis/cmd/openthesis-init-rs
#   ./build-linux.sh
#   cp ../../../bin/openthesis-init-linux-amd64 /opt/openthesis/bin/openthesis-init

set -e
cd "$(dirname "$0")"

# Install musl target if not present.
rustup target add x86_64-unknown-linux-musl 2>/dev/null || true

# Build static binary.
RUSTFLAGS="-C target-feature=+crt-static" \
cargo build --release --target x86_64-unknown-linux-musl

BINARY=target/x86_64-unknown-linux-musl/release/openthesis-init
echo "Built: $BINARY ($(du -sh $BINARY | cut -f1))"

# Copy to the bin directory that deploy.sh picks up.
cp "$BINARY" ../../../bin/openthesis-init-linux-amd64
echo "Copied to bin/openthesis-init-linux-amd64"
