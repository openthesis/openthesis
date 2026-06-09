#!/usr/bin/env bash
#
# Usage: ./deploy/build-kernel.sh [<USER>@<IP>]   (omit for local build)
#
set -euo pipefail

KERNEL_VERSION="6.6.75"
KERNEL_URL="https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${KERNEL_VERSION}.tar.xz"
SRC_DIR="${SRC_DIR:-${HOME}/.openthesis/build/src}"
OUTPUT_DIR="${OUTPUT_DIR:-${HOME}/.openthesis/kernel}"
LINUX_PATCHES_DIR="$(cd "$(dirname "$0")/../linux-patches" && pwd)"

build_kernel() {
    set -euo pipefail

    JOBS="$(nproc 2>/dev/null || echo 4)"

    echo "--- Downloading Linux ${KERNEL_VERSION} ---"
    mkdir -p "$SRC_DIR" "$OUTPUT_DIR"
    cd "$SRC_DIR"

    if [[ ! -d "linux-${KERNEL_VERSION}" ]]; then
        curl -fsSL "$KERNEL_URL" -o linux.tar.xz
        tar xf linux.tar.xz
        rm linux.tar.xz
    fi

    cd "linux-${KERNEL_VERSION}"

    # Apply DST kernel patches (RDTSC exiting, HLT user-exit, epoll order).
    echo "--- Applying DST kernel patches ---"
    local _series_file="${LINUX_PATCHES_DIR}/series"
    [[ -f "$_series_file" ]] || { echo "error: missing series file: ${_series_file}"; exit 1; }
    while IFS= read -r p || [[ -n "$p" ]]; do
        [[ -z "$p" || "$p" == \#* ]] && continue
        local pf="${LINUX_PATCHES_DIR}/${p}"
        [[ -f "$pf" ]] || { echo "error: patch not found: ${pf}"; exit 1; }
        echo "  Applying ${p}..."
        patch -p1 --forward --reject-file=/dev/null < "$pf" || {
            echo "  (already applied or skipped: ${p})"
        }
    done < "$_series_file"

    echo "--- Generating deterministic kernel config ---"
    make defconfig

    ./scripts/config --disable CONFIG_SMP
    ./scripts/config --disable CONFIG_MODULES
    ./scripts/config --disable CONFIG_RANDOMIZE_BASE
    ./scripts/config --disable CONFIG_RANDOMIZE_MEMORY
    ./scripts/config --disable CONFIG_HW_RANDOM
    ./scripts/config --disable CONFIG_HW_RANDOM_VIRTIO
    ./scripts/config --disable CONFIG_RANDOM_TRUST_CPU
    ./scripts/config --disable CONFIG_PREEMPT
    ./scripts/config --disable CONFIG_PREEMPT_VOLUNTARY
    ./scripts/config --disable CONFIG_PREEMPT_DYNAMIC
    ./scripts/config --enable  CONFIG_PREEMPT_NONE
    ./scripts/config --disable CONFIG_NO_HZ
    ./scripts/config --disable CONFIG_HIGH_RES_TIMERS
    ./scripts/config --disable CONFIG_INTEL_IDLE
    ./scripts/config --disable CONFIG_CPU_FREQ
    ./scripts/config --disable CONFIG_CPU_IDLE
    # Disabling haltpoll prevents KVM from busy-waiting instead of HLT,
    # which would cause non-deterministic instruction counts on HLT exits.
    ./scripts/config --disable CONFIG_HALTPOLL_CPUIDLE
    ./scripts/config --disable CONFIG_CPU_IDLE_GOV_HALTPOLL
    ./scripts/config --disable CONFIG_ACPI_PROCESSOR_IDLE

    ./scripts/config --enable CONFIG_VIRTIO
    ./scripts/config --enable CONFIG_VIRTIO_PCI
    ./scripts/config --enable CONFIG_VIRTIO_MMIO
    ./scripts/config --enable CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES
    ./scripts/config --enable CONFIG_VIRTIO_BLK
    ./scripts/config --enable CONFIG_VIRTIO_NET
    ./scripts/config --enable CONFIG_VIRTIO_CONSOLE
    ./scripts/config --enable CONFIG_VIRTIO_BALLOON
    ./scripts/config --enable CONFIG_VSOCKETS
    ./scripts/config --enable CONFIG_VSOCKETS_DIAG
    ./scripts/config --enable CONFIG_VIRTIO_VSOCKETS

    ./scripts/config --enable CONFIG_EXT4_FS
    ./scripts/config --enable CONFIG_TMPFS
    ./scripts/config --enable CONFIG_PROC_FS
    ./scripts/config --enable CONFIG_SYSFS
    ./scripts/config --enable CONFIG_DEVTMPFS
    ./scripts/config --enable CONFIG_DEVTMPFS_MOUNT

    ./scripts/config --enable CONFIG_SERIAL_8250
    ./scripts/config --enable CONFIG_SERIAL_8250_CONSOLE

    ./scripts/config --enable CONFIG_NET
    ./scripts/config --enable CONFIG_INET
    ./scripts/config --enable CONFIG_NETDEVICES
    # delay_port fault injection uses tc prio+netem+u32 - all required.
    ./scripts/config --enable CONFIG_NET_SCH_HTB
    ./scripts/config --enable CONFIG_NET_SCH_PRIO
    ./scripts/config --enable CONFIG_NET_SCH_NETEM
    ./scripts/config --enable CONFIG_NET_SCH_TBF
    ./scripts/config --enable CONFIG_NET_CLS_U32

    # KVM_GUEST enables paravirt clock inside the VM; not for KVM_INTEL/AMD (host side).
    ./scripts/config --enable CONFIG_KVM_GUEST
    ./scripts/config --enable CONFIG_PARAVIRT
    ./scripts/config --enable CONFIG_PARAVIRT_CLOCK

    # KCOV_INSTRUMENT_ALL instruments the full kernel, not just debug code.
    # Required to trace networking + VFS paths taken by SUT processes.
    ./scripts/config --enable CONFIG_DEBUG_FS
    ./scripts/config --enable CONFIG_KCOV
    ./scripts/config --enable CONFIG_KCOV_INSTRUMENT_ALL

    ./scripts/config --disable CONFIG_SOUND
    ./scripts/config --disable CONFIG_USB
    ./scripts/config --disable CONFIG_WLAN
    ./scripts/config --disable CONFIG_WIRELESS
    ./scripts/config --disable CONFIG_BT
    ./scripts/config --disable CONFIG_DRM
    ./scripts/config --disable CONFIG_FB
    ./scripts/config --disable CONFIG_INPUT_MOUSE
    ./scripts/config --disable CONFIG_INPUT_KEYBOARD

    make olddefconfig

    echo "--- Building kernel (${JOBS} jobs) ---"
    make -j"$JOBS" bzImage

    cp arch/x86/boot/bzImage "$OUTPUT_DIR/vmlinuz"
    cp vmlinux "$OUTPUT_DIR/vmlinux"
    cp .config "$OUTPUT_DIR/kernel.config"

    echo ""
    echo "--- Kernel built ---"
    echo "    vmlinuz: $(du -h "$OUTPUT_DIR/vmlinuz" | cut -f1)"
    echo "    vmlinux: $(du -h "$OUTPUT_DIR/vmlinux" | cut -f1)"
}

if [[ $# -ge 1 ]]; then
    TARGET="$1"
    echo "==> Building kernel on $TARGET"

    # Upload Linux patches to the target before building.
    echo "--- Uploading Linux patches ---"
    ssh -o StrictHostKeyChecking=accept-new "$TARGET" \
        "mkdir -p ~/.openthesis/build/src/linux-patches"
    rsync -av --progress \
        "$LINUX_PATCHES_DIR/" \
        "$TARGET:~/.openthesis/build/src/linux-patches/"

    ssh -o StrictHostKeyChecking=accept-new "$TARGET" bash -s <<REMOTE
$(declare -f build_kernel)
KERNEL_VERSION="$KERNEL_VERSION"
KERNEL_URL="$KERNEL_URL"
SRC_DIR="\${SRC_DIR:-\${HOME}/.openthesis/build/src}"
OUTPUT_DIR="\${OUTPUT_DIR:-\${HOME}/.openthesis/kernel}"
LINUX_PATCHES_DIR="\${HOME}/.openthesis/build/src/linux-patches"
build_kernel
REMOTE

    echo "==> Kernel build complete on $TARGET"
else
    echo "==> Building kernel locally"
    build_kernel
    echo "==> Kernel build complete"
fi
