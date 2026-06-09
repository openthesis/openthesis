#!/usr/bin/env bash
#
# Usage: ./deploy/build-firecracker.sh [--local] [--arch arm64]
#        ./deploy/build-firecracker.sh <USER>@<IP> [--arch arm64]
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PATCHES_DIR="$SCRIPT_DIR/../firecracker-patches"
LINUX_PATCHES_DIR="$SCRIPT_DIR/../linux-patches"

LOCAL=false
ARCH="x86_64"
TARGET=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --local)
            LOCAL=true
            shift
            ;;
        --arch)
            if [[ $# -lt 2 ]]; then
                echo "Error: --arch requires an argument (e.g. arm64)"
                exit 1
            fi
            ARCH="$2"
            shift 2
            ;;
        --arch=*)
            ARCH="${1#--arch=}"
            shift
            ;;
        -*)
            echo "Unknown flag: $1"
            echo "Usage: $0 [--local] [--arch arm64]"
            echo "       $0 <USER>@<IP> [--arch arm64]"
            exit 1
            ;;
        *)
            if [[ -n "$TARGET" ]]; then
                echo "Error: unexpected argument '$1' (target already set to '$TARGET')"
                exit 1
            fi
            TARGET="$1"
            shift
            ;;
    esac
done

# If no target given and not --local, default to local mode.
if [[ -z "$TARGET" ]]; then
    LOCAL=true
fi

# Validate arch.
if [[ "$ARCH" != "x86_64" && "$ARCH" != "arm64" ]]; then
    echo "Error: --arch must be x86_64 or arm64 (got '$ARCH')"
    exit 1
fi

# Cargo target triple for the requested arch.
if [[ "$ARCH" == "arm64" ]]; then
    CARGO_TARGET="aarch64-unknown-linux-musl"
else
    CARGO_TARGET=""
fi

if $LOCAL; then
    echo "==> Building patched Firecracker v1.15.0 locally (arch=$ARCH)"

    # Build the script body that will run in the current shell.
    # We pass PATCHES_DIR and CARGO_TARGET as env vars so the heredoc can use them.
    export _OT_PATCHES_DIR="$PATCHES_DIR"
    export _OT_CARGO_TARGET="$CARGO_TARGET"
    export _OT_BINARY="${_OT_BINARY:-${HOME}/.openthesis/bin/firecracker}"
    export _OT_SRC_DIR="${_OT_SRC_DIR:-${HOME}/.openthesis/build/src/firecracker}"

    bash -s <<'LOCAL_BUILD'
set -euo pipefail

FC_VERSION="v1.15.0"
FC_REPO="https://github.com/firecracker-microvm/firecracker"
SRC_DIR="${_OT_SRC_DIR:-${HOME}/.openthesis/build/src/firecracker}"
PATCHES_DIR="${_OT_PATCHES_DIR}"
CARGO_TARGET="${_OT_CARGO_TARGET}"
BINARY="${_OT_BINARY:-${HOME}/.openthesis/bin/firecracker}"

if ! command -v cargo &>/dev/null; then
    echo "--- Installing Rust ---"
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | \
        sh -s -- -y --default-toolchain stable --profile minimal
    source "$HOME/.cargo/env"
fi

source "$HOME/.cargo/env" 2>/dev/null || true
rustup update stable

if [[ -n "$CARGO_TARGET" ]]; then
    echo "--- Adding Rust target $CARGO_TARGET ---"
    rustup target add "$CARGO_TARGET"
fi

mkdir -p "$(dirname "$SRC_DIR")"

if [[ -d "$SRC_DIR/.git" ]]; then
    echo "--- Resetting existing Firecracker source ---"
    cd "$SRC_DIR"
    git reset --hard HEAD
    git clean -fdx
    git fetch origin --tags
    git checkout "$FC_VERSION"
    git reset --hard "$FC_VERSION"
    git clean -fdx
else
    echo "--- Cloning Firecracker $FC_VERSION ---"
    git clone --depth 1 --branch "$FC_VERSION" "$FC_REPO" "$SRC_DIR"
fi

cd "$SRC_DIR"
echo "--- Firecracker version: $(git describe --tags) ---"

echo "--- Applying DST patches ---"

git config user.email "openthesis@localhost" 2>/dev/null || true
git config user.name "OpenThesis Build" 2>/dev/null || true

# Robust apply: git mailinfo (strip headers) + git apply --recount (tolerates
# wrong hunk line counts). Same logic as deploy/local/lib.sh ot_apply_patch.
_ot_apply() {
    local pf="$1"
    local tmp
    tmp="$(mktemp -d)"
    git mailinfo "${tmp}/msg" "${tmp}/diff" < "$pf" > "${tmp}/info"
    if ! git apply --recount "${tmp}/diff" 2>/dev/null; then
        echo "!!! Patch failed: $(basename "$pf")"
        rm -rf "$tmp"
        exit 1
    fi
    git add -A
    local subject
    subject="$(grep '^Subject:' "${tmp}/info" | sed 's/^Subject: //')"
    { [[ -n "$subject" ]] && echo "$subject"; cat "${tmp}/msg"; } > "${tmp}/full_msg"
    git commit -F "${tmp}/full_msg" --allow-empty-message
    rm -rf "$tmp"
}

while IFS= read -r patch; do
    [[ -z "$patch" || "$patch" == \#* ]] && continue
    patch_file="$PATCHES_DIR/$patch"
    if [[ ! -f "$patch_file" ]]; then
        echo "!!! Patch file not found: $patch_file"
        exit 1
    fi
    echo "    Applying $patch"
    _ot_apply "$patch_file"
done < "$PATCHES_DIR/series"

if [[ -n "$CARGO_TARGET" && -f "$PATCHES_DIR/arm64-series" ]]; then
    echo "--- Applying ARM64-specific patches ---"
    while IFS= read -r patch; do
        [[ -z "$patch" || "$patch" == \#* ]] && continue
        patch_file="$PATCHES_DIR/$patch"
        if [[ ! -f "$patch_file" ]]; then
            echo "!!! ARM64 patch file not found: $patch_file"
            exit 1
        fi
        echo "    Applying $patch"
        _ot_apply "$patch_file"
    done < "$PATCHES_DIR/arm64-series"
fi

echo "--- All patches applied ---"

echo "--- Building Firecracker (release) ---"

if [[ -n "$CARGO_TARGET" ]]; then
    cargo build \
        --release \
        --package firecracker \
        --target "$CARGO_TARGET" \
        2>&1 | tail -20
    BUILT_BINARY="$SRC_DIR/build/cargo_target/${CARGO_TARGET}/release/firecracker"
    if [[ ! -f "$BUILT_BINARY" ]]; then
        BUILT_BINARY="$SRC_DIR/target/${CARGO_TARGET}/release/firecracker"
    fi
else
    cargo build \
        --release \
        --package firecracker \
        2>&1 | tail -20
    BUILT_BINARY="$SRC_DIR/build/cargo_target/release/firecracker"
    if [[ ! -f "$BUILT_BINARY" ]]; then
        BUILT_BINARY="$SRC_DIR/target/release/firecracker"
    fi
fi

if [[ ! -f "$BUILT_BINARY" ]]; then
    echo "!!! Build failed: $BUILT_BINARY not found"
    exit 1
fi

echo "--- Installing to $BINARY ---"
mkdir -p "$(dirname "$BINARY")"
cp "$BUILT_BINARY" "${BINARY}.new"
strip "${BINARY}.new"
if [[ -w "$(dirname "$BINARY")" ]]; then
    mv "${BINARY}.new" "$BINARY"
else
    sudo mv "${BINARY}.new" "$BINARY"
    sudo chown openthesis:openthesis "$BINARY" 2>/dev/null || true
fi

echo ""
echo "=== Firecracker build complete ==="
echo "Binary: $BINARY  ($(du -sh "$BINARY" | cut -f1))"
LOCAL_BUILD

    echo "==> Firecracker build finished."
else
    echo "==> Building patched Firecracker v1.15.0 on $TARGET (arch=$ARCH)"

    # Upload patches to target.
    echo "--- Uploading DST patches ---"
    ssh "$TARGET" "mkdir -p ~/.openthesis/build/src/firecracker-patches ~/.openthesis/build/src/linux-patches"
    rsync -av --progress \
        "$PATCHES_DIR/" \
        "$TARGET:~/.openthesis/build/src/firecracker-patches/"
    rsync -av --progress \
        "$LINUX_PATCHES_DIR/" \
        "$TARGET:~/.openthesis/build/src/linux-patches/"

    ssh "$TARGET" REMOTE_CARGO_TARGET="$CARGO_TARGET" bash -s <<'REMOTE'
set -euo pipefail

FC_VERSION="v1.15.0"
FC_REPO="https://github.com/firecracker-microvm/firecracker"
SRC_DIR="${HOME}/.openthesis/build/src/firecracker"
PATCHES_DIR="${HOME}/.openthesis/build/src/firecracker-patches"
CARGO_TARGET="${REMOTE_CARGO_TARGET:-}"
BINARY="${HOME}/.openthesis/bin/firecracker"

if ! command -v cargo &>/dev/null; then
    echo "--- Installing Rust ---"
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | \
        sh -s -- -y --default-toolchain stable --profile minimal
    source "$HOME/.cargo/env"
fi

source "$HOME/.cargo/env" 2>/dev/null || true
rustup update stable

if [[ -n "$CARGO_TARGET" ]]; then
    echo "--- Adding Rust target $CARGO_TARGET ---"
    rustup target add "$CARGO_TARGET"
fi

if [[ -d "$SRC_DIR/.git" ]]; then
    echo "--- Resetting existing Firecracker source ---"
    cd "$SRC_DIR"
    git reset --hard HEAD
    git clean -fdx
    git fetch origin --tags
    git checkout "$FC_VERSION"
    git reset --hard "$FC_VERSION"
    git clean -fdx
else
    echo "--- Cloning Firecracker $FC_VERSION ---"
    git clone --depth 1 --branch "$FC_VERSION" "$FC_REPO" "$SRC_DIR"
fi

cd "$SRC_DIR"
echo "--- Firecracker version: $(git describe --tags) ---"

echo "--- Applying DST patches ---"

git config user.email "openthesis@localhost" 2>/dev/null || true
git config user.name "OpenThesis Build" 2>/dev/null || true

# Robust apply: git mailinfo (strip headers) + git apply --recount (tolerates
# wrong hunk line counts). Same logic as deploy/local/lib.sh ot_apply_patch.
_ot_apply() {
    local pf="$1"
    local tmp
    tmp="$(mktemp -d)"
    git mailinfo "${tmp}/msg" "${tmp}/diff" < "$pf" > "${tmp}/info"
    if ! git apply --recount "${tmp}/diff" 2>/dev/null; then
        echo "!!! Patch failed: $(basename "$pf")"
        rm -rf "$tmp"
        exit 1
    fi
    git add -A
    local subject
    subject="$(grep '^Subject:' "${tmp}/info" | sed 's/^Subject: //')"
    { [[ -n "$subject" ]] && echo "$subject"; cat "${tmp}/msg"; } > "${tmp}/full_msg"
    git commit -F "${tmp}/full_msg" --allow-empty-message
    rm -rf "$tmp"
}

while IFS= read -r patch; do
    [[ -z "$patch" || "$patch" == \#* ]] && continue
    patch_file="$PATCHES_DIR/$patch"
    if [[ ! -f "$patch_file" ]]; then
        echo "!!! Patch file not found: $patch_file"
        exit 1
    fi
    echo "    Applying $patch"
    _ot_apply "$patch_file"
done < "$PATCHES_DIR/series"

if [[ -n "$CARGO_TARGET" && -f "$PATCHES_DIR/arm64-series" ]]; then
    echo "--- Applying ARM64-specific patches ---"
    while IFS= read -r patch; do
        [[ -z "$patch" || "$patch" == \#* ]] && continue
        patch_file="$PATCHES_DIR/$patch"
        if [[ ! -f "$patch_file" ]]; then
            echo "!!! ARM64 patch file not found: $patch_file"
            exit 1
        fi
        echo "    Applying $patch"
        _ot_apply "$patch_file"
    done < "$PATCHES_DIR/arm64-series"
fi

echo "--- All patches applied ---"

echo "--- Building Firecracker (release) ---"

if [[ -n "$CARGO_TARGET" ]]; then
    cargo build \
        --release \
        --package firecracker \
        --target "$CARGO_TARGET" \
        2>&1 | tail -20
    BUILT_BINARY="$SRC_DIR/build/cargo_target/${CARGO_TARGET}/release/firecracker"
    if [[ ! -f "$BUILT_BINARY" ]]; then
        BUILT_BINARY="$SRC_DIR/target/${CARGO_TARGET}/release/firecracker"
    fi
else
    cargo build \
        --release \
        --package firecracker \
        2>&1 | tail -20
    BUILT_BINARY="$SRC_DIR/build/cargo_target/release/firecracker"
    if [[ ! -f "$BUILT_BINARY" ]]; then
        BUILT_BINARY="$SRC_DIR/target/release/firecracker"
    fi
fi

if [[ ! -f "$BUILT_BINARY" ]]; then
    echo "!!! Build failed: $BUILT_BINARY not found"
    exit 1
fi

echo "--- Installing to $BINARY ---"
mkdir -p "$(dirname "$BINARY")"
cp "$BUILT_BINARY" "${BINARY}.new"
strip "${BINARY}.new"
mv "${BINARY}.new" "$BINARY"

echo ""
echo "=== Firecracker build complete ==="
echo "Binary: $BINARY  ($(du -sh "$BINARY" | cut -f1))"
REMOTE

    echo "==> Firecracker build finished."
fi
