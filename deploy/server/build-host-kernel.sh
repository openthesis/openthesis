#!/usr/bin/env bash
#
# Usage: ./deploy/build-host-kernel.sh [--local]
#        ./deploy/build-host-kernel.sh <USER>@<IP>
#
# Patches are generated with diff -u; apply via patch -p99.
# Versioned patch sets live under deploy/host-kernel-patches/<kernel-major.minor>/.
# The script auto-detects the running kernel version and selects the right set.
#
# Supported kernel versions:
#   6.8.x  - Ubuntu 24.04 (linux-source-6.8.0 package)
#   6.18.x - Pop!_OS / System76 / vanilla kernel.org 6.18
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PATCHES_BASE="$SCRIPT_DIR/../host-kernel-patches"

LOCAL=false
TARGET=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --local) LOCAL=true; shift ;;
        -*) echo "Unknown flag: $1"; exit 1 ;;
        *) TARGET="$1"; shift ;;
    esac
done

[[ -z "$TARGET" ]] && LOCAL=true

_build_body() {
cat <<'BODY'
set -euo pipefail

if [[ "$(id -u)" -ne 0 ]]; then SUDO="sudo"; else SUDO=""; fi

RUNNING_KERNEL="$(uname -r)"
KERNEL_MAJOR_MINOR="$(echo "$RUNNING_KERNEL" | grep -oP '^\d+\.\d+')"
PATCHES_DIR="${_OT_PATCHES_BASE}/${KERNEL_MAJOR_MINOR}"

# Fall back to closest available version if exact match not found.
if [[ ! -d "$PATCHES_DIR" ]]; then
    # Try major.minor without the patch version (e.g. 6.18 for 6.18.7)
    AVAIL=$(ls -d "${_OT_PATCHES_BASE}"/[0-9]* 2>/dev/null | sort -V | tail -1)
    if [[ -n "$AVAIL" ]]; then
        echo "warn: no patch set for ${KERNEL_MAJOR_MINOR}, using $(basename $AVAIL)"
        PATCHES_DIR="$AVAIL"
    else
        echo "error: no patch sets found under ${_OT_PATCHES_BASE}"
        exit 1
    fi
fi

echo "==> Host kernel: $RUNNING_KERNEL  patch set: $(basename $PATCHES_DIR)"

# Determine CPU vendor and which KVM module to rebuild.
if grep -q "AuthenticAMD" /proc/cpuinfo; then
    VENDOR="amd"
    KVM_MOD="kvm_amd"
else
    VENDOR="intel"
    KVM_MOD="kvm_intel"
fi
echo "    CPU vendor: $VENDOR  KVM module: $KVM_MOD"

# --- Source acquisition ---
case "$KERNEL_MAJOR_MINOR" in
    6.8)
        echo "--- Installing build deps + linux-source-6.8.0 ---"
        $SUDO apt-get update -q
        $SUDO apt-get install -y -qq \
            build-essential bison flex libssl-dev libelf-dev bc dwarves pahole \
            linux-source-6.8.0 "linux-headers-${RUNNING_KERNEL}" 2>&1 | tail -5

        KERNEL_SRC="/usr/src/linux-source-6.8.0"
        if [[ ! -f "$KERNEL_SRC/Makefile" ]]; then
            echo "--- Extracting linux-source-6.8.0 ---"
            cd "$KERNEL_SRC"
            for ext in bz2 gz xz; do
                [[ -f "linux-source-6.8.0.tar.$ext" ]] || continue
                $SUDO tar xf "linux-source-6.8.0.tar.$ext" --strip-components=1 && break
            done
        fi
        $SUDO chown -R "$(id -u):$(id -g)" "$KERNEL_SRC"
        SRC_PREFIX="/usr/src/linux-source-6.8.0/"
        ;;
    6.18|6.17|6.19)
        echo "--- Installing build deps + linux-source-${KERNEL_MAJOR_MINOR}.7 ---"
        $SUDO apt-get update -q 2>/dev/null || true
        # Install the distro kernel source package so our module is built against
        # the exact same source tree the running kernel was compiled from.
        # Downloading vanilla kernel.org source produces relocation mismatches
        # because the distro (e.g. Pop!_OS) applies its own patches.
        PKG_KV="${KERNEL_MAJOR_MINOR}.7"
        $SUDO apt-get install -y -qq \
            build-essential bison flex libssl-dev libelf-dev bc dwarves pahole libdw-dev \
            "linux-source-${PKG_KV}" 2>&1 | tail -5

        KERNEL_SRC="/usr/src/linux-source-${PKG_KV}"
        if [[ ! -f "$KERNEL_SRC/Makefile" ]]; then
            echo "--- Extracting linux-source-${PKG_KV} ---"
            cd "$KERNEL_SRC"
            for ext in xz bz2 gz; do
                [[ -f "linux-source-${PKG_KV}.tar.$ext" ]] || continue
                $SUDO tar xf "linux-source-${PKG_KV}.tar.$ext" --strip-components=1 && break
            done
        fi
        $SUDO chown -R "$(id -u):$(id -g)" "$KERNEL_SRC"
        SRC_PREFIX="/usr/src/linux-source-${PKG_KV}/"
        ;;
    *)
        echo "error: unsupported kernel version ${KERNEL_MAJOR_MINOR}"
        echo "       add a patch set to deploy/host-kernel-patches/${KERNEL_MAJOR_MINOR}/"
        exit 1
        ;;
esac

# Sync kernel headers from the running kernel's build tree to fix struct layout
# mismatches. On Ubuntu 24.04, CONFIG_RUST=y changes task_struct/kvm_arch field
# offsets relative to the linux-source package. Using the running kernel's own
# build tree headers guarantees our module is compiled against the exact struct
# definitions the loaded kernel was built with, preventing vermagic ABI mismatches.
KBUILD="/lib/modules/${RUNNING_KERNEL}/build"
if [[ -d "$KBUILD/include" ]]; then
    echo "--- Syncing headers from ${KBUILD} (fixes CONFIG_RUST=y struct layout) ---"
    rsync -a --quiet "$KBUILD/include/" "$KERNEL_SRC/include/" 2>/dev/null || true
    [[ -d "$KBUILD/arch/x86/include" ]] && \
        rsync -a --quiet "$KBUILD/arch/x86/include/" "$KERNEL_SRC/arch/x86/include/" 2>/dev/null || true
    echo "    done"
fi

cd "$KERNEL_SRC"

# Check if all non-blank + lines from a patch file are present in the target.
# Used as a fallback "already applied" test when patch -N can't detect it due
# to overlapping context from later patches.
_additions_present() {
    local patch_file="$1" target="$2"
    python3 - "$patch_file" "$target" <<'PYEOF'
import sys
pf, tf = sys.argv[1], sys.argv[2]
with open(pf) as f:
    patch_lines = f.readlines()
with open(tf) as f:
    target_set = set(l.rstrip('\n') for l in f)
in_hunk = False
for line in patch_lines:
    line = line.rstrip('\n')
    if line.startswith('@@ '):
        in_hunk = True
        continue
    if line.startswith('---') or line.startswith('+++'):
        in_hunk = False
        continue
    if in_hunk and line.startswith('+'):
        content = line[1:]
        if content.strip() and content not in target_set:
            sys.exit(1)  # at least one added line is missing
sys.exit(0)
PYEOF
}

# Restore any files left in a partial-patch state from a previous failed run.
# patch(1) creates .rej files when a hunk fails; the target file is then partially
# modified. We restore from the source tarball so patching starts clean.
_TARBALL=$(find "$KERNEL_SRC" -maxdepth 1 -name 'linux-source-*.tar.*' 2>/dev/null | sort | head -1)
if [[ -n "$_TARBALL" ]]; then
    _TARBALL_PREFIX=$(tar tf "$_TARBALL" 2>/dev/null | head -1 | sed 's|/.*||' || true)
    while IFS= read -r rej; do
        src="${rej%.rej}"
        rel="${src#$KERNEL_SRC/}"
        echo "    Restoring $rel from tarball (previous partial patch)"
        tar xf "$_TARBALL" --strip-components=1 "${_TARBALL_PREFIX}/${rel}" 2>/dev/null || true
        rm -f "$rej"
    done < <(find "$KERNEL_SRC" -name "*.rej" 2>/dev/null)
fi

# --- Apply patches ---
echo "--- Applying KVM patches ---"
series_file="$PATCHES_DIR/series"

while IFS= read -r patch; do
    [[ -z "$patch" || "$patch" == \#* ]] && continue
    patch_file="$PATCHES_DIR/$patch"
    [[ -f "$patch_file" ]] || { echo "!!! missing patch: $patch_file"; exit 1; }

    # Skip VMX patches on AMD, skip SVM patches on Intel.
    case "$patch" in
        *vmx*) [[ "$VENDOR" == "intel" ]] || { echo "    (skip VMX patch on AMD: $patch)"; continue; } ;;
        *svm*) [[ "$VENDOR" == "amd"   ]] || { echo "    (skip SVM patch on Intel: $patch)"; continue; } ;;
    esac

    TARGET_FILE=$(grep "^+++" "$patch_file" | head -1 | sed 's/^+++ //' | awk '{print $1}')
    # Strip any /usr/src/linux*/ prefix - handles both linux-X.Y.Z/ and
    # linux-source-X.Y.Z/ regardless of the path embedded in the patch.
    REL_FILE=$(echo "$TARGET_FILE" | sed 's|^/usr/src/linux[^/]*/||')

    [[ -n "$REL_FILE" && "$REL_FILE" != "$TARGET_FILE" ]] || {
        echo "!!! could not extract relative path from: $patch (TARGET_FILE=$TARGET_FILE)"
        exit 1
    }
    [[ -f "$REL_FILE" ]] || { echo "!!! target file not found: $REL_FILE"; exit 1; }

    # Fast path: if every non-blank added line is already in the target file,
    # the patch is fully applied - no need to invoke patch(1) at all.
    # This correctly handles cases where later patches modified the same region,
    # preventing context-fuzz false positives.
    if _additions_present "$patch_file" "$REL_FILE"; then
        echo "    (already applied: $patch)"
        continue
    fi

    echo "    Applying $patch -> $REL_FILE"
    PATCH_OUT=$(patch --force -p99 --input="$patch_file" "$REL_FILE" 2>&1) || true
    if echo "$PATCH_OUT" | grep -q "FAILED"; then
        echo "!!! patch failed: $patch"
        echo "$PATCH_OUT"
        exit 1
    fi
    if echo "$PATCH_OUT" | grep -qv "^$"; then
        echo "$PATCH_OUT"
    else
        echo "    OK: $patch"
    fi
done < "$series_file"

echo "--- Patches applied ---"

# --- Configure ---
if [[ ! -f .config ]]; then
    cp "/boot/config-${RUNNING_KERNEL}" .config
fi
./scripts/config --set-str CONFIG_SYSTEM_TRUSTED_KEYS "" 2>/dev/null || true
./scripts/config --set-str CONFIG_SYSTEM_REVOCATION_KEYS "" 2>/dev/null || true

# Match the production kernel's debug-info settings so that pahole generates a
# .BTF section identical in structure to the distro module.  The running kernel
# was compiled with DWARF5 + CONFIG_DEBUG_INFO_BTF_MODULES=y; without these the
# module loader rejects our module with "existing value nonzero" at insmod time.
./scripts/config --enable CONFIG_DEBUG_INFO 2>/dev/null || true
./scripts/config --disable CONFIG_DEBUG_INFO_NONE 2>/dev/null || true
./scripts/config --enable CONFIG_DEBUG_INFO_DWARF5 2>/dev/null || true
./scripts/config --enable CONFIG_DEBUG_INFO_BTF 2>/dev/null || true
./scripts/config --enable CONFIG_DEBUG_INFO_BTF_MODULES 2>/dev/null || true
./scripts/config --enable CONFIG_DEBUG_INFO_COMPRESSED_NONE 2>/dev/null || true
./scripts/config --disable CONFIG_DEBUG_INFO_COMPRESSED_ZLIB 2>/dev/null || true
./scripts/config --disable CONFIG_DEBUG_INFO_COMPRESSED_ZSTD 2>/dev/null || true

# For 6.8 Ubuntu: fix SUBLEVEL that differs from package version.
if [[ "$KERNEL_MAJOR_MINOR" == "6.8" ]]; then
    SUBLEVEL=$(grep "^SUBLEVEL" Makefile | awk '{print $3}')
    [[ "$SUBLEVEL" != "0" ]] && sed -i "s/^SUBLEVEL = ${SUBLEVEL}/SUBLEVEL = 0/" Makefile
    ./scripts/config --set-str CONFIG_LOCALVERSION "-110-generic" 2>/dev/null || true
    ./scripts/config --disable CONFIG_LOCALVERSION_AUTO 2>/dev/null || true
fi

HEADERS_DIR="/usr/src/linux-headers-${RUNNING_KERNEL}"

make olddefconfig 2>&1 | tail -5

make modules_prepare 2>&1

# Copy Module.symvers AFTER modules_prepare - that target regenerates it from
# the vanilla source tree, which produces CRCs for the wrong kernel build.
# Overwriting with the running kernel's headers copy ensures all symbol CRCs
# match what the loaded kernel expects.
[[ -f "$HEADERS_DIR/Module.symvers" ]] && cp "$HEADERS_DIR/Module.symvers" Module.symvers

# Force UTS_RELEASE to match the running kernel so vermagic is identical.
# modules_prepare may regenerate utsrelease.h, so overwrite it after.
# Needed when the distro adds a suffix (e.g. Pop!_OS -76061807-generic) that
# the vanilla source tree doesn't know about.
echo "${RUNNING_KERNEL}" > include/config/kernel.release
mkdir -p include/generated
echo "#define UTS_RELEASE \"${RUNNING_KERNEL}\"" > include/generated/utsrelease.h

# Place the running kernel's BTF blob as 'vmlinux' so that scripts/Makefile.modfinal
# cmd_btf_ko fires: it checks [ -f vmlinux ] then runs pahole -J --btf_base vmlinux.
# pahole (via libbpf btf__parse) reads raw BTF files natively, so the sysfs blob
# works without needing to decompress /boot/vmlinuz.
if [[ ! -f vmlinux ]] && [[ -r /sys/kernel/btf/vmlinux ]]; then
    echo "--- Placing kernel BTF for module BTF generation ---"
    cp /sys/kernel/btf/vmlinux vmlinux
    echo "    $(wc -c < vmlinux) bytes"
fi

# Clean stale .o files only when the existing build lacks BTF - this avoids a
# full 20-minute recompile on subsequent invocations once BTF is in place.
if ! readelf -S arch/x86/kvm/kvm.ko 2>/dev/null | grep -q "\.BTF"; then
    echo "--- Cleaning stale KVM build artifacts (BTF not yet present) ---"
    make M=arch/x86/kvm clean 2>/dev/null || true
fi

# --- Build ---
echo "--- Building KVM modules ($(nproc) jobs) ---"
make -j"$(nproc)" M=arch/x86/kvm modules 2>&1 | tail -30

# Remove the temporary vmlinux we placed - it is only needed during the build.
rm -f vmlinux

echo ""
ls -lh arch/x86/kvm/kvm.ko arch/x86/kvm/vmx/kvm-intel.ko arch/x86/kvm/svm/kvm-amd.ko 2>/dev/null || \
ls -lh arch/x86/kvm/*.ko arch/x86/kvm/*/*.ko 2>/dev/null || true
echo ""
modinfo arch/x86/kvm/kvm.ko | grep vermagic

# Verify BTF was generated - the running kernel requires it.
if readelf -S arch/x86/kvm/kvm.ko 2>/dev/null | grep -q "\.BTF"; then
    echo "    .BTF section present in kvm.ko"
else
    echo "!!! .BTF section missing from kvm.ko - insmod will likely fail"
    echo "    Check that pahole is installed and /sys/kernel/btf/vmlinux is readable"
fi

# --- Replace pahole-generated BTF with clean distro BTF ---
#
# pahole -J --btf_base <raw-sysfs-btf-blob> produces a type entry with an
# empty/invalid name string. When our module loads, this corrupts the kernel's
# BTF verifier state and causes ALL subsequent module BTF validations to fail
# (including overlay.ko), which breaks Docker.
#
# Fix: copy the .BTF section from the corresponding distro module (which was
# compiled with a proper ELF vmlinux and has clean BTF), then strip the now-
# inconsistent .BTF.ext from our module. Our code changes (new KVM capabilities)
# are in the text section and are unaffected by the BTF replacement.
_replace_btf() {
    local our_ko="$1" mod_name="$2"
    local distro_ko ko_tmp btf_tmp
    distro_ko=$(find "/lib/modules/${RUNNING_KERNEL}" -name "${mod_name}.ko*" 2>/dev/null | head -1)
    if [[ -z "$distro_ko" ]]; then
        echo "    warn: distro ${mod_name}.ko not found, keeping pahole BTF"
        return
    fi
    ko_tmp=$(mktemp --suffix=.ko)
    btf_tmp=$(mktemp --suffix=.btf)
    case "$distro_ko" in
        *.zst) zstd -d "$distro_ko" -o "$ko_tmp" -f -q 2>/dev/null ;;
        *.xz)  xz -d -c "$distro_ko" > "$ko_tmp" 2>/dev/null ;;
        *)     cp "$distro_ko" "$ko_tmp" ;;
    esac
    if objcopy --dump-section .BTF="$btf_tmp" "$ko_tmp" 2>/dev/null && \
       objcopy --update-section .BTF="$btf_tmp" "$our_ko" 2>/dev/null; then
        objcopy --remove-section .BTF.ext "$our_ko" 2>/dev/null || true
        echo "    $(basename "$our_ko"): BTF replaced from distro module"
    else
        echo "    warn: BTF replacement failed for $(basename "$our_ko"), keeping pahole BTF"
    fi
    rm -f "$ko_tmp" "$btf_tmp"
}

echo "--- Replacing module BTF with clean distro BTF ---"
_replace_btf "arch/x86/kvm/kvm.ko" "kvm"
if [[ "$VENDOR" == "amd" ]]; then
    _replace_btf "arch/x86/kvm/kvm-amd.ko" "kvm_amd"
else
    _replace_btf "arch/x86/kvm/kvm-intel.ko" "kvm_intel"
fi

# --- Load ---
echo "--- Loading new KVM modules ---"
# Check for live (non-zombie) Firecracker processes. Zombies are already dead
# and hold no KVM resources; they must not block the module swap.
if pgrep -x firecracker >/dev/null 2>&1; then
    _live_fc=$(pgrep -x firecracker | xargs -r ps -o pid=,stat= 2>/dev/null | awk '$2 !~ /^Z/ {print $1}')
    if [[ -n "$_live_fc" ]]; then
        echo "!!! Stop Firecracker VMs before loading modules (pids: $_live_fc)"
        exit 1
    fi
fi

# Pre-load overlay before our KVM modules go in. If any residual BTF edge
# case survives the replacement above, overlay will already be resident and
# won't need re-validation, keeping Docker working.
$SUDO modprobe overlay 2>/dev/null || true

# Strip DWARF debug sections from modules before loading.
# strip --strip-debug removes .debug_* sections but preserves .BTF and .symtab.
# Use find to locate the .ko files - module-only builds (M=arch/x86/kvm) put all
# .ko files directly under arch/x86/kvm/, not in vendor subdirs.
while IFS= read -r ko; do
    strip --strip-debug "$ko" 2>/dev/null || true
done < <(find arch/x86/kvm -maxdepth 2 -name "*.ko" 2>/dev/null)

# Detect modules already stuck in GOING state from a previous failed run.
# /proc/modules shows state in field 5: Live, Loading, or Unloading (= GOING).
_stuck=$( awk '$1 ~ /^kvm/ && $5 == "Unloading" {print $1}' /proc/modules )
if [[ -n "$_stuck" ]]; then
    echo "!!! kvm modules stuck in GOING state from a previous failed unload: $_stuck"
    echo "    the kernel will not finish cleanup while this persists"
    echo "    reboot to recover, then retry"
    exit 1
fi

# Check for open /dev/kvm file descriptors before unloading.
# An open fd keeps the module alive in GOING state indefinitely.
if [[ -e /dev/kvm ]]; then
    _kvm_holders=$($SUDO fuser /dev/kvm 2>/dev/null || true)
    if [[ -n "$_kvm_holders" ]]; then
        echo "!!! /dev/kvm is held open - stop these processes first:"
        echo "$_kvm_holders" | xargs -r ps -o pid=,comm=,args= 2>/dev/null || echo "    pids: $_kvm_holders"
        exit 1
    fi
fi

# Even with no open fds, the kernel runs kvm_destroy_vm() asynchronously after
# a KVM process dies: vCPU teardown, memory slot cleanup, SVM/VMX hardware
# reset via on_each_cpu(). If rmmod races with that teardown, kvm_amd's exit
# function blocks mid-cleanup and the module gets stuck in GOING state forever.
# Two seconds is enough for any in-flight VM destruction to complete.
sleep 2

# Unload vendor module first; kvm base cannot unload while vendor holds a ref.
if lsmod | grep -q "^${KVM_MOD} "; then
    if ! $SUDO rmmod "${KVM_MOD}" 2>/dev/null; then
        echo "!!! could not unload ${KVM_MOD} (holders: $(ls /sys/module/${KVM_MOD}/holders/ 2>/dev/null | tr '\n' ' '))"
        exit 1
    fi
    _wait=0
    until [[ ! -d /sys/module/${KVM_MOD} ]]; do
        sleep 0.2; (( _wait++ )) || true
        if (( _wait >= 25 )); then
            echo "!!! ${KVM_MOD} stuck in GOING state - reboot to recover"; exit 1
        fi
    done
fi

# Before touching kvm base: verify holders directory is empty.
# A GOING-state module drops its refcnt but stays in holders/ until fully gone.
_kvm_holders_dir=$(ls /sys/module/kvm/holders/ 2>/dev/null | tr '\n' ' ')
if [[ -n "$_kvm_holders_dir" ]]; then
    echo "!!! kvm holders not empty after unloading ${KVM_MOD}: ${_kvm_holders_dir}"
    echo "    waiting for holders to clear..."
    _wait=0
    until [[ -z "$(ls /sys/module/kvm/holders/ 2>/dev/null)" ]]; do
        sleep 0.5; (( _wait++ )) || true
        if (( _wait >= 20 )); then
            echo "!!! holders still present after ${_wait}s - reboot to recover"; exit 1
        fi
    done
fi

if lsmod | grep -q '^kvm '; then
    $SUDO rmmod kvm 2>/dev/null || true
fi

# Poll until sysfs confirms kvm is fully gone (kernel finishes cleanup async).
_kvm_wait=0
until [[ ! -d /sys/module/kvm ]]; do
    if (( _kvm_wait >= 10 )); then
        echo "!!! kvm stuck in GOING state after ${_kvm_wait}s"
        echo "    refcnt: $(cat /sys/module/kvm/refcnt 2>/dev/null || echo unknown)"
        echo "    holders: $(ls /sys/module/kvm/holders/ 2>/dev/null | tr '\n' ' ')"
        echo "    reboot to recover"
        exit 1
    fi
    sleep 1
    (( _kvm_wait++ )) || true
done

$SUDO insmod arch/x86/kvm/kvm.ko || {
    echo "!!! kvm.ko load failed"
    echo "==> dmesg (last 15 lines):"
    $SUDO dmesg | tail -15
    exit 1
}

if [[ "$VENDOR" == "intel" ]]; then
    KO=$(find arch/x86/kvm -maxdepth 2 -name "kvm-intel.ko" 2>/dev/null | head -1)
else
    KO=$(find arch/x86/kvm -maxdepth 2 -name "kvm-amd.ko" 2>/dev/null | head -1)
fi
if [[ -z "$KO" ]]; then
    echo "!!! vendor KVM module not found"
    exit 1
fi
$SUDO insmod "$KO" || {
    echo "!!! $KO load failed"
    echo "==> dmesg (last 10 lines):"
    $SUDO dmesg | tail -10
    exit 1
}
echo "$KO loaded"

echo ""
lsmod | grep kvm

# --- Verify ---
CAP_RDTSC=$(grep "KVM_CAP_RDTSC_EXITING" "$PATCHES_DIR/0001-"*.patch | grep -oP '\d+' | tail -1)
CAP_HLT=$(grep "KVM_CAP_HLT_USER_EXIT" "$PATCHES_DIR/0006-"*.patch 2>/dev/null | grep -oP '\d+' | tail -1 || true)
echo ""
echo "--- Verifying KVM capabilities ---"
python3 - "$CAP_RDTSC" "${CAP_HLT:-}" <<'PYEOF'
import struct, os, fcntl, sys
def check(fd, cap, name):
    r = fcntl.ioctl(fd, 0xAE03, struct.pack('I', cap))
    val = struct.unpack('I', r)[0]
    print(f"{name} ({cap}): {'ENABLED ✓' if val else 'NOT SUPPORTED ✗'}")
fd = os.open('/dev/kvm', os.O_RDONLY)
check(fd, int(sys.argv[1]), "KVM_CAP_RDTSC_EXITING")
if len(sys.argv) > 2 and sys.argv[2]:
    check(fd, int(sys.argv[2]), "KVM_CAP_HLT_USER_EXIT")
os.close(fd)
PYEOF
BODY
}

if $LOCAL; then
    echo "==> Building patched KVM modules locally"
    export _OT_PATCHES_BASE="$PATCHES_BASE"
    bash -c "$(_build_body)"
else
    echo "==> Building patched KVM modules on $TARGET"
    ssh -o StrictHostKeyChecking=accept-new "$TARGET" \
        "mkdir -p ~/.openthesis/build/src/host-kernel-patches"
    rsync -av --progress "$PATCHES_BASE/" "$TARGET:~/.openthesis/build/src/host-kernel-patches/"
    ssh -o StrictHostKeyChecking=accept-new "$TARGET" \
        _OT_PATCHES_BASE='~/.openthesis/build/src/host-kernel-patches' bash -s <<< "$(_build_body)"
fi

echo ""
echo "==> KVM module build complete."
