#!/usr/bin/env bash
#
# build-gvisor.sh - clone, patch, and build gVisor with OpenThesis DST patches.
#
# Usage:
#   ./deploy/build-gvisor.sh <USER>@<IP>
#   ./deploy/build-gvisor.sh               # build locally
#
# Builds a patched version of gVisor's runsc with deterministic simulation
# support. The resulting binary is placed at ~/.openthesis/bin/runsc.
#
# Patches add: virtual time, deterministic RNG, TID-ordered scheduler,
# deterministic network, deterministic filesystem, sfork-based checkpoints,
# control socket, coverage bitmap, syscall fault injection, instruction counting.
#
# Requirements:
#   - Go 1.22+ (gVisor minimum)
#   - Bazel 7.x (gVisor build system)
#   - Linux x86_64 host
#   - git
#
set -euo pipefail

GVISOR_VERSION="c9735035b4c67b16ba1c1142470eb3f76a4f7b91"
GVISOR_REPO="https://github.com/google/gvisor.git"
SRC_DIR="${SRC_DIR:-${HOME}/.openthesis/build/src}"
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.openthesis}"
PATCH_DIR="$(cd "$(dirname "$0")/../gvisor-patches" && pwd)"
BAZEL_VERSION="8.3.1"

build_gvisor() {
    set -euo pipefail

    if [[ "$(id -u)" -ne 0 ]]; then
        SUDO="sudo"
    else
        SUDO=""
    fi

    JOBS="$(nproc 2>/dev/null || echo 4)"

    # ---------------------------------------------------------------
    # 0. Install build dependencies
    # ---------------------------------------------------------------
    echo "--- Installing build dependencies ---"
    $SUDO apt-get update -qq

    # Required: these must succeed for gVisor + eBPF genrules.
    $SUDO apt-get install -y -qq \
        clang \
        llvm \
        lld \
        gcc \
        make \
        git \
        curl \
        unzip \
        zip \
        python3 \
        libbpf-dev \
        gcc-multilib \
        libc6-dev-i386

    # Optional/tooling-only: package names vary by distro release.
    $SUDO apt-get install -y -qq \
        linux-headers-generic \
        gcc-aarch64-linux-gnu \
        g++-aarch64-linux-gnu \
        binutils-aarch64-linux-gnu || true

    # gVisor's XDP/eBPF build includes glibc stubs that require 32-bit dev headers.
    if [[ ! -f /usr/include/x86_64-linux-gnu/gnu/stubs-32.h ]]; then
        echo "ERROR: missing /usr/include/x86_64-linux-gnu/gnu/stubs-32.h" >&2
        echo "Install libc6-dev-i386 (and gcc-multilib) before building runsc." >&2
        exit 1
    fi

    # ---------------------------------------------------------------
    # 1. Install Bazel if not present
    # ---------------------------------------------------------------
    # Install bazelisk - it reads .bazelversion and downloads the right Bazel automatically.
    if ! command -v bazel &>/dev/null; then
        echo "--- Installing bazelisk (manages Bazel version from .bazelversion) ---"
        BAZELISK_URL="https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64"
        curl -fsSL "$BAZELISK_URL" -o /tmp/bazelisk
        chmod +x /tmp/bazelisk
        $SUDO mv /tmp/bazelisk /usr/local/bin/bazel
        echo "    bazelisk installed as /usr/local/bin/bazel"
    else
        # If bazel is installed but isn't bazelisk, check if it can handle .bazelversion.
        if bazel --version 2>&1 | grep -q "bazelisk"; then
            echo "--- bazelisk found ---"
        else
            echo "--- bazel found ($(bazel --version 2>&1 | head -1)), replacing with bazelisk for .bazelversion support ---"
            BAZELISK_URL="https://github.com/bazelbuild/bazelisk/releases/latest/download/bazelisk-linux-amd64"
            curl -fsSL "$BAZELISK_URL" -o /tmp/bazelisk
            chmod +x /tmp/bazelisk
            $SUDO mv /tmp/bazelisk /usr/local/bin/bazel
        fi
    fi

    # ---------------------------------------------------------------
    # 2. Install Go if not present
    # ---------------------------------------------------------------
    if ! command -v go &>/dev/null; then
        echo "--- Installing Go ---"
        GO_VERSION="1.22.6"
        GO_URL="https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
        curl -fsSL "$GO_URL" -o /tmp/go.tar.gz
        $SUDO rm -rf /usr/local/go
        $SUDO tar -C /usr/local -xzf /tmp/go.tar.gz
        rm -f /tmp/go.tar.gz
        export PATH="/usr/local/go/bin:$PATH"
        echo "    go $(go version)"
    else
        echo "--- Go found: $(go version) ---"
    fi

    # ---------------------------------------------------------------
    # 3. Clone gVisor
    # ---------------------------------------------------------------
    echo "--- Cloning gVisor (${GVISOR_VERSION}) ---"
    $SUDO mkdir -p "$SRC_DIR"
    $SUDO chown "$(id -u):$(id -g)" "$SRC_DIR"
    cd "$SRC_DIR"

    # Always start from a clean tree so patches apply cleanly.
    if [[ -d "gvisor" ]]; then
        echo "    (removing previous source tree)"
        rm -rf "gvisor"
    fi

    git clone "$GVISOR_REPO" gvisor
    cd gvisor && git checkout "$GVISOR_VERSION" --detach && cd ..
    cd gvisor
    git config user.email "build@openthesis.local"
    git config user.name "OpenThesis Build"

    # ---------------------------------------------------------------
    # 4. Apply OpenThesis patches
    # ---------------------------------------------------------------
    echo "--- Applying OpenThesis patches ---"

    # Read the series file for ordered patch application.
    if [[ ! -f "$PATCH_DIR/series" ]]; then
        echo "ERROR: no series file found in $PATCH_DIR" >&2
        exit 1
    fi

    PATCH_COUNT=0
    PATCH_FAIL=0
    while IFS= read -r patchname; do
        # Skip empty lines and comments.
        [[ -z "$patchname" || "$patchname" == \#* ]] && continue

        patchfile="$PATCH_DIR/$patchname"
        if [[ ! -f "$patchfile" ]]; then
            echo "WARNING: patch file not found, skipping: $patchname" >&2
            continue
        fi

        # Normalise line endings (SCP can introduce CRLF).
        sed -i 's/\r$//' "$patchfile"

        echo "    applying $patchname"
        if git -c commit.gpgsign=false am --3way --quiet "$patchfile" 2>/dev/null; then
            PATCH_COUNT=$((PATCH_COUNT + 1))
        else
            git am --abort 2>/dev/null || true
            echo "    FAIL: $patchname did not apply - run 'git am --show-current-patch=diff' in $(pwd) to debug"
            PATCH_FAIL=$((PATCH_FAIL + 1))
        fi
    done < "$PATCH_DIR/series"

    echo "    applied $PATCH_COUNT patches, skipped $PATCH_FAIL"
    if [[ $PATCH_FAIL -gt 0 ]]; then
        echo "ERROR: $PATCH_FAIL patch(es) were skipped - refusing to continue with a partial patch set" >&2
        exit 1
    fi

    ensure_go_src_in_build() {
        local gofile="$1"
        local dir
        local base
        local buildfile
        dir="$(dirname "$gofile")"
        base="$(basename "$gofile")"
        buildfile="$dir/BUILD"

        [[ -f "$gofile" && -f "$buildfile" ]] || return 0

        if grep -q "\"$base\"" "$buildfile"; then
            return 0
        fi

        echo "    adding $base to $buildfile srcs"
        # Insert only into multiline srcs blocks (e.g. go_library srcs),
        # not one-line forms like: srcs = ["foo.proto"],
        if grep -q 'srcs = \[[[:space:]]*$' "$buildfile"; then
            sed -i "/srcs = \[[[:space:]]*$/a\        \"$base\"," "$buildfile"
        else
            echo "ERROR: could not find srcs list in $buildfile" >&2
            return 1
        fi
    }

    # Keep BUILD src lists in sync for patch-created Go files.
    while IFS= read -r new_go_file; do
        [[ -n "$new_go_file" ]] || continue
        ensure_go_src_in_build "$new_go_file" || exit 1
    done < <(
        grep -hE '^[[:space:]]*create mode 100644 .+\.go$' "$PATCH_DIR"/*.patch 2>/dev/null \
            | awk '{print $4}' \
            | sort -u
    )

    # Some upstream revisions keep explicit Go src lists in BUILD files.
    # Ensure the virtual clock source is included when patch 0001 added it.
    if [[ -f pkg/sentry/time/virtual_clock.go && -f pkg/sentry/time/BUILD ]]; then
        # Normalize virtual_clock.go to stdlib-only imports to avoid strict-deps
        # drift across upstream BUILD definitions.
        cat > pkg/sentry/time/virtual_clock.go <<'EOF'
// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package time

import (
	"sync"
	"syscall"
	"time"
)

const (
	clockRealtime       int32 = 0
	clockMonotonic      int32 = 1
	clockMonotonicRaw   int32 = 4
	clockMonotonicCoarse int32 = 6
	clockBoottime       int32 = 7
)

// VirtualClock provides deterministic time for the Sentry in DST mode.
// It replaces CalibratedClock and advances only when explicitly ticked
// by the deterministic scheduler or instruction counter.
type VirtualClock struct {
	mu sync.Mutex

	// epoch is the base wall-clock time (configurable, default 2024-01-01T00:00:00Z).
	epoch time.Time

	// monotonicNS tracks elapsed nanoseconds since virtual boot.
	monotonicNS int64

	// quantumNS is the time added per scheduling quantum.
	quantumNS int64

	// ipsRate is the instructions-per-nanosecond rate for instruction-counted time.
	ipsRate float64

	// frozen is true when the clock is paused (during checkpoint).
	frozen bool
}

// VirtualClockOpts configures a VirtualClock.
type VirtualClockOpts struct {
	Epoch     time.Time
	QuantumNS int64
	IPSRate   float64
}

// DefaultVirtualClockOpts returns sensible defaults.
func DefaultVirtualClockOpts() VirtualClockOpts {
	return VirtualClockOpts{
		Epoch:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		QuantumNS: 10_000_000, // 10ms
		IPSRate:   1.0,        // 1 instruction per nanosecond = 1 GHz
	}
}

// NewVirtualClock creates a VirtualClock with the given options.
func NewVirtualClock(opts VirtualClockOpts) *VirtualClock {
	return &VirtualClock{
		epoch:     opts.Epoch,
		quantumNS: opts.QuantumNS,
		ipsRate:   opts.IPSRate,
	}
}

// GetTime returns the current virtual time for the given clock ID.
// Implements the Clocks interface used by the Timekeeper.
func (vc *VirtualClock) GetTime(id ClockID) (int64, error) {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	switch id {
	case Realtime:
		return vc.epoch.UnixNano() + vc.monotonicNS, nil
	case Monotonic:
		return vc.monotonicNS, nil
	default:
		return 0, syscall.EINVAL
	}
}

// GetTimeByLinuxID returns the current virtual time for the given linux clock ID.
func (vc *VirtualClock) GetTimeByLinuxID(clockID int32) (int64, error) {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	switch clockID {
	case clockRealtime:
		return vc.epoch.UnixNano() + vc.monotonicNS, nil
	case clockMonotonic, clockMonotonicRaw, clockMonotonicCoarse, clockBoottime:
		return vc.monotonicNS, nil
	default:
		return 0, syscall.EINVAL
	}
}

// Update implements Clocks.Update. For VirtualClock, this is a no-op since
// time only advances via AdvanceQuantum/AdvanceInstructions.
func (vc *VirtualClock) Update() (Parameters, bool, Parameters, bool) {
	vc.mu.Lock()
	defer vc.mu.Unlock()

	realtimeNS := vc.epoch.UnixNano() + vc.monotonicNS
	monoParams := Parameters{
		BaseCycles: 0,
		BaseRef:    ReferenceNS(vc.monotonicNS),
		Frequency:  1000000000, // 1 GHz nominal
	}
	realParams := Parameters{
		BaseCycles: 0,
		BaseRef:    ReferenceNS(realtimeNS),
		Frequency:  1000000000,
	}
	return monoParams, true, realParams, true
}

// AdvanceQuantum advances the clock by one scheduling quantum.
// Called by the deterministic scheduler after selecting a task.
func (vc *VirtualClock) AdvanceQuantum() {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if !vc.frozen {
		vc.monotonicNS += vc.quantumNS
	}
}

// AdvanceInstructions advances the clock by the given retired instruction count.
// Called by the instruction counter (patch 10) after a task slice completes.
func (vc *VirtualClock) AdvanceInstructions(count uint64) {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	if !vc.frozen {
		vc.monotonicNS += int64(float64(count) / vc.ipsRate)
	}
}

// Freeze stops the clock. Called during checkpointing to ensure the
// snapshot captures a consistent time value.
func (vc *VirtualClock) Freeze() {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.frozen = true
}

// Thaw resumes the clock after a checkpoint completes.
func (vc *VirtualClock) Thaw() {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	vc.frozen = false
}

// MonotonicNS returns the current monotonic nanoseconds (for snapshot metadata).
func (vc *VirtualClock) MonotonicNS() int64 {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.monotonicNS
}
EOF

        if ! grep -q '"virtual_clock.go"' pkg/sentry/time/BUILD; then
            echo "    adding virtual_clock.go to pkg/sentry/time/BUILD srcs"
            if grep -q '"calibrated_clock.go"' pkg/sentry/time/BUILD; then
                sed -i '/"calibrated_clock.go"/a\        "virtual_clock.go",' pkg/sentry/time/BUILD
            else
                echo "ERROR: could not locate insertion point in pkg/sentry/time/BUILD" >&2
                exit 1
            fi
        fi

        # virtual_clock.go imports these packages; keep Go strict-deps satisfied.
        if ! grep -q 'deps = \[' pkg/sentry/time/BUILD; then
            echo "    adding deps block to pkg/sentry/time/BUILD"
            awk '
                { print }
                /srcs = \[/ { in_srcs = 1; next }
                in_srcs && /^[[:space:]]*\],/ && !done {
                    print "    deps = ["
                    print "        \"//pkg/abi/linux\","
                    print "        \"//pkg/errors/linuxerr\","
                    print "    ],"
                    done = 1
                    in_srcs = 0
                }
            ' pkg/sentry/time/BUILD > pkg/sentry/time/BUILD.tmp
            mv pkg/sentry/time/BUILD.tmp pkg/sentry/time/BUILD
        else
            if ! grep -q '"//pkg/abi/linux"' pkg/sentry/time/BUILD; then
                echo "    adding //pkg/abi/linux to pkg/sentry/time/BUILD deps"
                sed -i '/deps = \[/a\        "//pkg/abi/linux",' pkg/sentry/time/BUILD
            fi
            if ! grep -q '"//pkg/errors/linuxerr"' pkg/sentry/time/BUILD; then
                echo "    adding //pkg/errors/linuxerr to pkg/sentry/time/BUILD deps"
                sed -i '/deps = \[/a\        "//pkg/errors/linuxerr",' pkg/sentry/time/BUILD
            fi
        fi
    fi

    # Some patch transports can truncate trailing lines; enforce a valid
    # coverage implementation when patch 0008 is present.
    if [[ -f pkg/sentry/kernel/dst_coverage.go ]]; then
        cat > pkg/sentry/kernel/dst_coverage.go <<'EOF'
// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

package kernel

import (
	"fmt"
	"math/bits"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	// DefaultCoverageBitmapSize is 64KB (512K edge slots).
	DefaultCoverageBitmapSize = 65536

	// coverageMemfdName is the name used for the memfd.
	coverageMemfdName = "openthesis-coverage"
)

// CoverageBitmap provides shared-memory code coverage collection using
// an AFL-style edge coverage bitmap. The bitmap is backed by a memfd
// so the host process can read it directly via mmap without any IPC.
type CoverageBitmap struct {
	mu sync.Mutex
	fd int
	bitmap []byte
	size int
	prevPC uint64
	totalEdges int
}

// NewCoverageBitmap creates a new coverage bitmap backed by a memfd.
func NewCoverageBitmap(size int) (*CoverageBitmap, error) {
	if size <= 0 {
		size = DefaultCoverageBitmapSize
	}

	fd, err := unix.MemfdCreate(coverageMemfdName, 0)
	if err != nil {
		return nil, fmt.Errorf("memfd_create(%s): %w", coverageMemfdName, err)
	}

	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("ftruncate coverage memfd to %d: %w", size, err)
	}

	bitmap, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("mmap coverage memfd: %w", err)
	}

	return &CoverageBitmap{
		fd:     fd,
		bitmap: bitmap,
		size:   size,
	}, nil
}

// RecordEdge records a control flow edge from the previous PC to pc.
func (cb *CoverageBitmap) RecordEdge(pc uint64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	hash := (pc ^ (cb.prevPC >> 1)) % uint64(cb.size*8)
	byteIdx := hash / 8
	bitIdx := uint(hash % 8)
	mask := byte(1 << bitIdx)

	if cb.bitmap[byteIdx]&mask == 0 {
		cb.bitmap[byteIdx] |= mask
		cb.totalEdges++
	}

	cb.prevPC = pc
}

// FD returns the memfd file descriptor for sharing with the host.
func (cb *CoverageBitmap) FD() int {
	return cb.fd
}

// Size returns the bitmap size in bytes.
func (cb *CoverageBitmap) Size() int {
	return cb.size
}

// Count returns the number of set bits in the bitmap.
func (cb *CoverageBitmap) Count() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	count := 0
	for _, b := range cb.bitmap {
		count += bits.OnesCount8(b)
	}
	return count
}

// Reset clears bitmap state for a new exploration episode.
func (cb *CoverageBitmap) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	for i := range cb.bitmap {
		cb.bitmap[i] = 0
	}
	cb.prevPC = 0
	cb.totalEdges = 0
}

// Close unmaps and closes the backing memfd.
func (cb *CoverageBitmap) Close() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.bitmap != nil {
		unix.Munmap(cb.bitmap)
		cb.bitmap = nil
	}
	if cb.fd >= 0 {
		unix.Close(cb.fd)
		cb.fd = -1
	}
	return nil
}
EOF
    fi

    # Patch 0007 wires control-plane code in runsc/boot that imports runsc/dst.
    # Keep Bazel strict-deps satisfied across upstream BUILD variants.
    if [[ -f runsc/dst/ctrl.go && ! -f runsc/dst/BUILD ]]; then
        echo "    creating runsc/dst/BUILD"
        mkdir -p runsc/dst
        cat > runsc/dst/BUILD <<'EOF'
load("//tools:defs.bzl", "go_library")

go_library(
    name = "dst",
    srcs = ["ctrl.go"],
    visibility = ["//runsc:__subpackages__"],
    deps = [
        "//pkg/log",
        "//pkg/sentry/kernel",
        "//pkg/sentry/time",
    ],
)
EOF
    fi

    if [[ -f runsc/boot/loader.go && -f runsc/boot/BUILD ]]; then
        if grep -q '"gvisor.dev/gvisor/runsc/dst"' runsc/boot/loader.go; then
            if ! grep -q '"//runsc/dst"' runsc/boot/BUILD; then
                echo "    adding //runsc/dst to runsc/boot/BUILD deps"
                if grep -q 'deps = \[' runsc/boot/BUILD; then
                    sed -i '/deps = \[/a\        "//runsc/dst",' runsc/boot/BUILD
                else
                    echo "ERROR: could not find deps list in runsc/boot/BUILD" >&2
                    exit 1
                fi
            fi
        fi
    fi

    # Ensure the DST control socket server is actually started in Loader.New.
    # Patch series adds initDSTControlSocket(), but some revisions miss the call.
    if [[ -f runsc/boot/loader.go ]]; then
        if grep -q 'func (l \*Loader) initDSTControlSocket' runsc/boot/loader.go; then
            if grep -q 'initDSTControlSocket(args\\.Conf\\.DSTControlPath)' runsc/boot/loader.go && ! grep -q 'dstSocketPath := args\\.Conf\\.DSTControlPath' runsc/boot/loader.go; then
                echo "    upgrading legacy initDSTControlSocket() call in runsc/boot/loader.go"
                sed -i '/initDSTControlSocket(args\.Conf\.DSTControlPath)/i\
\tdstSocketPath := args.Conf.DSTControlPath\
\tif dstSocketPath == "" {\
\t\tdstSocketPath = args.Conf.RootDir + "/openthesis-" + args.ID + ".sock"\
\t}' runsc/boot/loader.go
                sed -i 's/initDSTControlSocket(args\.Conf\.DSTControlPath)/initDSTControlSocket(dstSocketPath)/' runsc/boot/loader.go
            fi
            if ! grep -q 'l\.initDSTControlSocket(' runsc/boot/loader.go; then
                echo "    wiring initDSTControlSocket() into runsc/boot/loader.go (enableStrace anchor)"
                awk '
                    {
                        print
                        if ($0 ~ /if err := enableStrace\(args\.Conf\); err != nil \{/) {
                            in_block = 1
                            next
                        }
                        if (in_block && $0 ~ /^[[:space:]]*\}/) {
                            print ""
                            print "\tdstSocketPath := args.Conf.DSTControlPath"
                            print "\tif dstSocketPath == \"\" {"
                            print "\t\tdstSocketPath = args.Conf.RootDir + \"/openthesis-\" + args.ID + \".sock\""
                            print "\t}"
                            print "\tif err := l.initDSTControlSocket(dstSocketPath); err != nil {"
                            print "\t\treturn nil, fmt.Errorf(\"init DST control socket: %w\", err)"
                            print "\t}"
                            in_block = 0
                        }
                    }
                ' runsc/boot/loader.go > runsc/boot/loader.go.tmp
                if grep -q 'l\.initDSTControlSocket(' runsc/boot/loader.go.tmp; then
                    mv runsc/boot/loader.go.tmp runsc/boot/loader.go
                else
                    rm -f runsc/boot/loader.go.tmp
                fi
            fi

            if ! grep -q 'l\.initDSTControlSocket(' runsc/boot/loader.go; then
                echo "    wiring initDSTControlSocket() into runsc/boot/loader.go (tk.SetClocks fallback)"
                awk '
                    {
                        print
                        if (!inserted && $0 ~ /tk\.SetClocks\(/) {
                            print ""
                            print "\tdstSocketPath := args.Conf.DSTControlPath"
                            print "\tif dstSocketPath == \"\" {"
                            print "\t\tdstSocketPath = args.Conf.RootDir + \"/openthesis-\" + args.ID + \".sock\""
                            print "\t}"
                            print "\tif err := l.initDSTControlSocket(dstSocketPath); err != nil {"
                            print "\t\treturn nil, fmt.Errorf(\"init DST control socket: %w\", err)"
                            print "\t}"
                            inserted = 1
                        }
                    }
                ' runsc/boot/loader.go > runsc/boot/loader.go.tmp
                if grep -q 'l\.initDSTControlSocket(' runsc/boot/loader.go.tmp; then
                    mv runsc/boot/loader.go.tmp runsc/boot/loader.go
                else
                    rm -f runsc/boot/loader.go.tmp
                fi
            fi

            if ! grep -q 'l\.initDSTControlSocket(' runsc/boot/loader.go; then
                echo "ERROR: failed to wire initDSTControlSocket() into runsc/boot/loader.go" >&2
                exit 1
            fi
        fi
    fi

    # Ensure DST control server creates parent directory for the Unix socket.
    # Without this, sandbox startup fails with:
    # "listen unix ...: bind: no such file or directory".
    if [[ -f runsc/dst/ctrl.go ]]; then
        if grep -q 'func NewControlServer(path string, k \*kernel\.Kernel)' runsc/dst/ctrl.go; then
            if ! grep -q 'os\.MkdirAll(filepath\.Dir(path)' runsc/dst/ctrl.go; then
                echo "    ensuring DST control socket parent directory exists in runsc/dst/ctrl.go"
                if ! grep -q '"path/filepath"' runsc/dst/ctrl.go; then
                    sed -i '/"os"/a\
\t"path/filepath"' runsc/dst/ctrl.go
                fi
                sed -i '/os.Remove(path)/a\
\
\tif err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {\
\t\treturn nil, fmt.Errorf("dst ctrl: mkdir %s: %w", filepath.Dir(path), err)\
\t}' runsc/dst/ctrl.go
            fi
        fi
    fi

    # Ensure flags tagged in runsc/config/config.go are actually registered.
    # Without these, runsc panics in NewFromFlags: "Flag \"openthesis-dst\" not found".
    if [[ -f runsc/config/config.go && -f runsc/config/flags.go ]]; then
        if grep -q 'flag:"openthesis-dst"' runsc/config/config.go; then
            if ! grep -q '"openthesis-dst"' runsc/config/flags.go; then
                echo "    registering OpenThesis runsc flags in runsc/config/flags.go"
                sed -i '/allow-rootfs-tar-annotation/a\
\tflagSet.Bool("openthesis-dst", false, "enable OpenThesis deterministic simulation mode.")\
\tflagSet.Int64("openthesis-dst-epoch", 0, "OpenThesis virtual clock epoch in nanoseconds since Unix epoch.")\
\tflagSet.Int64("openthesis-dst-quantum-ns", 0, "OpenThesis virtual scheduler quantum in nanoseconds.")\
\tflagSet.String("openthesis-control", "", "Unix socket path for OpenThesis control API.")' runsc/config/flags.go
            fi
        fi
    fi

    # Ensure `runsc run` accepts --openthesis-control and plumbs it into conf.
    # Some patch variants update config/loader but miss cmd/run.go wiring.
    if [[ -f runsc/cmd/run.go ]]; then
        echo "    ensuring --openthesis-control wiring in runsc/cmd/run.go"

        if ! grep -q 'openthesisControl string' runsc/cmd/run.go; then
            awk '
                {
                    print
                    if ($0 ~ /spec \*specs\.Spec/) {
                        print ""
                        print "\t// openthesisControl is an OpenThesis control socket path override."
                        print "\topenthesisControl string"
                    }
                }
            ' runsc/cmd/run.go > runsc/cmd/run.go.tmp
            mv runsc/cmd/run.go.tmp runsc/cmd/run.go
        fi

        if ! grep -q 'StringVar(&r.openthesisControl, "openthesis-control"' runsc/cmd/run.go; then
            awk '
                {
                    print
                    if ($0 ~ /f\.IntVar\(&r\.execFD, "exec-fd"/) {
                        print "\tf.StringVar(&r.openthesisControl, \"openthesis-control\", \"\", \"OpenThesis control socket path override\")"
                    }
                }
            ' runsc/cmd/run.go > runsc/cmd/run.go.tmp
            mv runsc/cmd/run.go.tmp runsc/cmd/run.go
        fi

        if ! grep -q 'conf.DSTControlPath = r.openthesisControl' runsc/cmd/run.go; then
            awk '
                {
                    print
                    if ($0 ~ /conf := args\[0\]\.\(\*config\.Config\)/) {
                        print "\tif r.openthesisControl != \"\" {"
                        print "\t\tconf.DSTControlPath = r.openthesisControl"
                        print "\t}"
                    }
                }
            ' runsc/cmd/run.go > runsc/cmd/run.go.tmp
            mv runsc/cmd/run.go.tmp runsc/cmd/run.go
        fi
    fi

    # ---------------------------------------------------------------
    # 5. Build runsc
    # ---------------------------------------------------------------
    echo "--- Building gVisor runsc (${JOBS} jobs) ---"

    # Set Bazel output base outside the source tree to avoid polluting it.
    BAZEL_OUTPUT_BASE="/tmp/openthesis-bazel-output"
    mkdir -p "$BAZEL_OUTPUT_BASE"

    bazel \
        --output_base="$BAZEL_OUTPUT_BASE" \
        build \
        --jobs="$JOBS" \
        --define=openthesis=true \
        //runsc:runsc

    # ---------------------------------------------------------------
    # 6. Install
    # ---------------------------------------------------------------
    echo "--- Installing runsc ---"
    $SUDO mkdir -p "$INSTALL_DIR/bin"

    RUNSC_BIN=$(bazel --output_base="$BAZEL_OUTPUT_BASE" info bazel-bin 2>/dev/null)/runsc/runsc_/runsc
    if [[ ! -f "$RUNSC_BIN" ]]; then
        # Fallback: try the linux_amd64 path.
        RUNSC_BIN=$(find "$BAZEL_OUTPUT_BASE" -name "runsc" -type f -executable 2>/dev/null | head -1)
    fi

    if [[ -z "$RUNSC_BIN" || ! -f "$RUNSC_BIN" ]]; then
        echo "ERROR: could not find built runsc binary" >&2
        exit 1
    fi

    $SUDO cp "$RUNSC_BIN" "$INSTALL_DIR/bin/runsc"
    $SUDO chmod 755 "$INSTALL_DIR/bin/runsc"
    $SUDO chown openthesis:openthesis "$INSTALL_DIR/bin/runsc" 2>/dev/null || true

    # ---------------------------------------------------------------
    # 7. Install OCI runtime configuration
    # ---------------------------------------------------------------
    echo "--- Configuring OCI runtime ---"
    OCI_CONFIG_DIR="/etc/openthesis"
    $SUDO mkdir -p "$OCI_CONFIG_DIR"

    $SUDO tee "$OCI_CONFIG_DIR/runsc.toml" > /dev/null <<'TOML'
# OpenThesis patched runsc configuration.
# This file is referenced by container runtimes (e.g., containerd) to configure
# the deterministic gVisor sandbox.

[runsc]
# Use the KVM platform for best performance. Falls back to ptrace if KVM
# is unavailable (e.g., inside a VM without nested virtualization).
platform = "kvm"

# Enable deterministic mode (activates all OpenThesis patches).
deterministic = true

# Default simulation seed. Overridden per-run by the control plane.
seed = 42

# Virtual clock epoch (RFC 3339). All virtual time starts from this point.
epoch = "2024-01-01T00:00:00Z"

# Scheduling quantum in nanoseconds (default 10ms).
quantum_ns = 10000000

# Instructions-per-second rate for instruction-counted time (default 1 GHz).
ips_rate = 1000000000

# Coverage bitmap size in bytes (default 64KB).
coverage_size = 65536

# Network configuration.
[runsc.network]
# Use netstack (gVisor's userspace network stack) for deterministic networking.
network = "sandbox"

# Filesystem configuration.
[runsc.filesystem]
# Use deterministic inode allocation.
deterministic_inodes = true
# Sort directory entries lexicographically.
sorted_readdir = true
TOML

    echo ""
    echo "--- gVisor (runsc) built ---"
    "$INSTALL_DIR/bin/runsc" --version 2>&1 | head -1 || echo "    (installed at $INSTALL_DIR/bin/runsc)"

    # ---------------------------------------------------------------
    # 8. Verify installation
    # ---------------------------------------------------------------
    echo "--- Verifying runsc ---"
    if "$INSTALL_DIR/bin/runsc" --version &>/dev/null; then
        echo "    runsc binary: OK"
    else
        echo "    runsc binary: present (version check may require root)"
    fi

    echo "    binary size: $(du -h "$INSTALL_DIR/bin/runsc" | cut -f1)"
    echo "    config: $OCI_CONFIG_DIR/runsc.toml"
}

# ---------------------------------------------------------------
# Entry point: local or remote build
# ---------------------------------------------------------------
if [[ $# -ge 1 ]]; then
    TARGET="$1"
    echo "==> Building gVisor on $TARGET"

    # Transfer patches to the remote host.
    echo "==> Uploading patches to $TARGET"
    ssh -o StrictHostKeyChecking=accept-new "$TARGET" "mkdir -p /tmp/openthesis-gvisor-patches"
    scp -o StrictHostKeyChecking=accept-new "$PATCH_DIR"/series \
        "$TARGET:/tmp/openthesis-gvisor-patches/"
    # Only copy patch files if they exist.
    if ls "$PATCH_DIR"/*.patch &>/dev/null; then
        scp -o StrictHostKeyChecking=accept-new "$PATCH_DIR"/*.patch \
            "$TARGET:/tmp/openthesis-gvisor-patches/"
    fi

    ssh -o StrictHostKeyChecking=accept-new "$TARGET" bash -s <<REMOTE
$(declare -f build_gvisor)
GVISOR_VERSION="$GVISOR_VERSION"
GVISOR_REPO="$GVISOR_REPO"
SRC_DIR="$SRC_DIR"
INSTALL_DIR="$INSTALL_DIR"
PATCH_DIR="/tmp/openthesis-gvisor-patches"
BAZEL_VERSION="$BAZEL_VERSION"
build_gvisor
REMOTE

    echo "==> gVisor build complete on $TARGET"
else
    echo "==> Building gVisor locally"
    build_gvisor
    echo "==> gVisor build complete"
fi
