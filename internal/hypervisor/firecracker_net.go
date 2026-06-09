package hypervisor

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"

	"github.com/openthesis/openthesis/internal/prng"
)

// shmHeadroomFactor scales the per-VM memory size to account for snapshot
// metadata (vm.snap is small but non-zero), Diff snapshot tail growth as the
// guest dirties pages, and a margin so we do not write right up to the tmpfs
// limit. 1.2 = 20% headroom over the raw guest RAM size.
const shmHeadroomFactor = 1.2

// shmRequiredBytes returns the number of free bytes the snapshot directory
// must have to safely accept one more snapshot for a VM with expectedMemoryMB
// guest RAM, given concurrency parallel workers writing snapshots into the
// same tmpfs. Each worker is reserved one snapshot worth of headroom.
func shmRequiredBytes(expectedMemoryMB uint64, concurrency int) uint64 {
	if concurrency < 1 {
		concurrency = 1
	}
	perVM := uint64(float64(expectedMemoryMB) * shmHeadroomFactor)
	return perVM * uint64(concurrency) * (1 << 20) // MiB → bytes
}

// shmHasSpace reports whether path has enough free bytes to accept one more
// snapshot for a VM with expectedMemoryMB of guest RAM at the given
// concurrency level. It returns the currently-available bytes, an ok flag,
// and any statfs error encountered.
func shmHasSpace(path string, expectedMemoryMB uint64, concurrency int) (uint64, bool, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false, err
	}
	avail := stat.Bavail * uint64(stat.Bsize) //nolint:gosec
	required := shmRequiredBytes(expectedMemoryMB, concurrency)
	return avail, avail >= required, nil
}

// fcMACFromSeed derives a deterministic locally-administered unicast MAC address
// from the VM seed and interface index using SplitMix64 with domain separation.
// Domain constant "mac_addr" matches the Rust-side MAC_DOMAIN constant.
func fcMACFromSeed(seed uint64, ifIndex uint32) string {
	const domainSep = uint64(0x6d61635f61646472) // "mac_addr"
	domainSeed := seed ^ domainSep ^ uint64(ifIndex)
	rng := prng.New(domainSeed)
	v := rng.Uint64()
	b := [6]byte{
		byte(v >> 56), byte(v >> 48), byte(v >> 40),
		byte(v >> 32), byte(v >> 24), byte(v >> 16),
	}
	// Locally administered (bit 1 of byte 0 = 1), unicast (bit 0 = 0).
	b[0] = (b[0] & 0xfe) | 0x02
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		b[0], b[1], b[2], b[3], b[4], b[5])
}

// fcCreateTAP creates a TAP interface with the given name and brings it up.
// Firecracker requires the TAP to already exist before it starts; it does not
// create the TAP itself (unlike QEMU with -netdev tap,script=... which does).
func fcCreateTAP(name string) error {
	// ip tuntap add dev <name> mode tap
	if out, err := exec.Command("ip", "tuntap", "add", "dev", name, "mode", "tap").
		CombinedOutput(); err != nil {
		return fmt.Errorf("ip tuntap add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// ip link set <name> up
	if out, err := exec.Command("ip", "link", "set", name, "up").
		CombinedOutput(); err != nil {
		// Best-effort cleanup.
		_ = exec.Command("ip", "link", "del", name).Run()
		return fmt.Errorf("ip link set up: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// fcDeleteTAP deletes a TAP interface created by fcCreateTAP.
func fcDeleteTAP(name string) error {
	out, err := exec.Command("ip", "link", "del", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link del %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
