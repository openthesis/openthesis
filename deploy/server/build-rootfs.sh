#!/usr/bin/env bash
#
# Usage: ./deploy/build-rootfs.sh [<USER>@<IP>]   (omit for local build)
#
set -euo pipefail

ROOTFS_SIZE_MB=2048
ROOTFS_DIR="${ROOTFS_DIR:-${HOME}/.openthesis/rootfs}"
ROOTFS_IMG="${ROOTFS_IMG:-$ROOTFS_DIR/rootfs.ext4}"

build_rootfs() {
    set -euo pipefail

    if [[ "$(id -u)" -ne 0 ]]; then
        SUDO="sudo"
    else
        SUDO=""
    fi

    echo "--- Building rootfs ---"
    mkdir -p "$ROOTFS_DIR"

    echo "--- Creating ${ROOTFS_SIZE_MB}MB ext4 image ---"
    $SUDO dd if=/dev/zero of="$ROOTFS_IMG" bs=1M count="$ROOTFS_SIZE_MB" status=progress
    $SUDO mkfs.ext4 -F "$ROOTFS_IMG"

    MOUNT_DIR=$(mktemp -d)
    $SUDO mount -o loop "$ROOTFS_IMG" "$MOUNT_DIR"

    echo "--- Bootstrapping Debian (bookworm) ---"
    $SUDO debootstrap --variant=minbase bookworm "$MOUNT_DIR" http://deb.debian.org/debian

    echo "--- Installing container runtime ---"
    $SUDO chroot "$MOUNT_DIR" bash -c "
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq \
            runc \
            iproute2 \
            iptables \
            procps \
            ca-certificates \
            curl
        apt-get clean
        rm -rf /var/lib/apt/lists/*
    "

    # Optional: install k3s + helm for Kubernetes-native SUTs.
    # Set INSTALL_K3S=true before running this script to include them.
    if [[ "${INSTALL_K3S:-false}" == "true" ]]; then
        K3S_VERSION="${K3S_VERSION:-v1.32.3+k3s1}"
        HELM_VERSION="${HELM_VERSION:-v3.17.0}"
        echo "--- Installing k3s ${K3S_VERSION} ---"
        $SUDO curl -sfL "https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}/k3s" \
            -o "$MOUNT_DIR/usr/local/bin/k3s"
        $SUDO chmod +x "$MOUNT_DIR/usr/local/bin/k3s"
        # kubectl symlink
        $SUDO ln -sf /usr/local/bin/k3s "$MOUNT_DIR/usr/local/bin/kubectl"
        echo "--- Installing helm ${HELM_VERSION} ---"
        $SUDO curl -sfL "https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz" | \
            $SUDO tar xz --strip=1 -C "$MOUNT_DIR/usr/local/bin/" linux-amd64/helm
        $SUDO chmod +x "$MOUNT_DIR/usr/local/bin/helm"
        $SUDO mkdir -p "$MOUNT_DIR/opt/charts"
        echo "--- k3s + helm installed ---"
    fi

    echo "--- Installing init ---"
    $SUDO rm -f "$MOUNT_DIR/sbin/init"
    $SUDO tee "$MOUNT_DIR/sbin/init" > /dev/null <<'INIT'
#!/bin/bash
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts /dev/shm
mount -t devpts devpts /dev/pts
mount -t tmpfs tmpfs /dev/shm
mount -t tmpfs tmpfs /tmp
mount -t tmpfs tmpfs /run

hostname openthesis-vm

SEED=$(cat /proc/cmdline | grep -oP 'openthesis\.seed=\K[0-9]+' || echo "42")
export OPENTHESIS_SEED="$SEED"

export OPENTHESIS_OUTPUT_DIR="/tmp/openthesis-output"
mkdir -p "$OPENTHESIS_OUTPUT_DIR"

if ! ip route show | grep -q default; then
    GATEWAY=$(cat /proc/cmdline | grep -oP 'ip=[^:]+::[^:]+' | cut -d: -f3)
    if [[ -n "$GATEWAY" ]]; then
        ip route add default via "$GATEWAY"
    fi
fi
echo "nameserver 8.8.8.8" > /etc/resolv.conf

exec /sbin/agetty -a root ttyS0 linux
INIT
    $SUDO chmod +x "$MOUNT_DIR/sbin/init"

    $SUDO tee "$MOUNT_DIR/etc/hosts" > /dev/null <<HOSTS
127.0.0.1   localhost openthesis-vm
::1         localhost
HOSTS

    $SUDO mkdir -p "$MOUNT_DIR/opt/openthesis/bin"
    $SUDO mkdir -p "$MOUNT_DIR/opt/openthesis/lib"
    $SUDO mkdir -p "$MOUNT_DIR/var/lib/openthesis"

    # LD_PRELOAD coverage for dynamic binaries; static Go binaries use init-process KCOV.
    if [ -f /tmp/guest-kcov-preload.c ]; then
        gcc -shared -fPIC -O2 -o /tmp/libkcov_preload.so /tmp/guest-kcov-preload.c -lpthread
        $SUDO install -m 755 /tmp/libkcov_preload.so "$MOUNT_DIR/opt/openthesis/lib/libkcov_preload.so"
    fi

    if [ -f /tmp/libfault.c ]; then
        gcc -shared -fPIC -O2 -ldl -o /tmp/libfault.so /tmp/libfault.c
        $SUDO install -m 755 /tmp/libfault.so "$MOUNT_DIR/opt/openthesis/lib/libfault.so"
        $SUDO install -m 755 /tmp/libfault.so "$MOUNT_DIR/usr/local/lib/libfault.so"
    fi

    # Userspace BB coverage for C/C++ SUTs with -fsanitize-coverage=trace-pc-guard.
    if [ -f /tmp/libcover.c ]; then
        gcc -shared -fPIC -O2 -o /tmp/libcover.so /tmp/libcover.c -lpthread
        $SUDO install -m 755 /tmp/libcover.so "$MOUNT_DIR/opt/openthesis/lib/libcover.so"
        $SUDO install -m 755 /tmp/libcover.so "$MOUNT_DIR/usr/local/lib/libcover.so"
    fi

    $SUDO umount "$MOUNT_DIR"
    rmdir "$MOUNT_DIR"

    echo ""
    echo "--- Rootfs built: $ROOTFS_IMG ($(du -h "$ROOTFS_IMG" | cut -f1)) ---"
}

if [[ $# -ge 1 ]]; then
    TARGET="$1"
    echo "==> Building rootfs on $TARGET"

    SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
    scp -o StrictHostKeyChecking=accept-new \
        "$SCRIPT_DIR/../guest/guest-kcov-preload.c" "$TARGET:/tmp/guest-kcov-preload.c"
    scp -o StrictHostKeyChecking=accept-new \
        "$SCRIPT_DIR/../guest/libcover.c" "$TARGET:/tmp/libcover.c"
    scp -o StrictHostKeyChecking=accept-new \
        "$SCRIPT_DIR/../guest/libfault.c" "$TARGET:/tmp/libfault.c"

    ssh -o StrictHostKeyChecking=accept-new "$TARGET" bash -s <<REMOTE
$(declare -f build_rootfs)
ROOTFS_SIZE_MB="$ROOTFS_SIZE_MB"
ROOTFS_DIR="\${ROOTFS_DIR:-\${HOME}/.openthesis/rootfs}"
ROOTFS_IMG="\${ROOTFS_IMG:-\${ROOTFS_DIR}/rootfs.ext4}"
build_rootfs
REMOTE

    echo "==> Rootfs build complete on $TARGET"
else
    echo "==> Building rootfs locally"
    build_rootfs
    echo "==> Rootfs build complete"
fi
