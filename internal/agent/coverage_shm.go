package agent

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

const coverageMapSize = 1 << 16 // 64K edges, matches QEMU patch

// ShmCoverageReader reads coverage data directly from QEMU's shared memory
// region, bypassing virtio-serial. The patched QEMU places the coverage
// bitmap at offset ram_size within /dev/shm/openthesis-{name}.
//
// Shared memory file layout (/dev/shm/openthesis-{name}):
//
//   byte offset        size     field
//   -----------------  -------  ----------------------------------
//   0                  ramSize  guest RAM (not mapped here)
//   ramSize            64K      edge bitmap (AFL-style hit counts)
//   ramSize + 64K      8        generation counter (uint64 LE)
//
// Collect gates copies on the generation counter:
//
//   QEMU (patched)              ShmCoverageReader.Collect()
//   ------------------          ------------------------------------
//   writes edge bitmap          gen = LE64(mapped[64K : 64K+8])
//   increments gen      ------> if gen == r.lastGen: return nil
//                               r.lastGen = gen
//                               copy(buf, mapped[0 : 64K])
//                               return buf
//
// Only the coverage region is mmap'd. The mmap offset is page-aligned;
// mapped is a sub-slice that starts at the bitmap.
//
//   [0,           ramSize)        guest RAM
//   [ramSize,     ramSize+64K)    coverage bitmap
//   [ramSize+64K, ramSize+64K+8)  generation counter (uint64)
type ShmCoverageReader struct {
	name      string
	shmPath   string
	ramSize   uint64
	rawMapped []byte // original mmap'd region (for Munmap)
	mapped    []byte // sub-slice starting at coverage bitmap
	lastGen   uint64
}

// NewShmCoverageReader creates a coverage reader that mmaps the shared memory
// file at the coverage bitmap offset. Returns an error if the file is too
// small to contain the coverage region; stock QEMU (TCG) creates shm files
// of exactly ramSize and accessing beyond that via mmap causes SIGBUS on
// tmpfs. Patched QEMU is required.
func NewShmCoverageReader(name, shmPath string, ramSizeMB uint64) (*ShmCoverageReader, error) {
	ramSize := ramSizeMB * 1024 * 1024
	totalSize := ramSize + coverageMapSize + 8

	f, err := os.OpenFile(shmPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("coverage shm open: %w", err)
	}
	defer f.Close()

	// Verify the file is large enough to contain the coverage bitmap.
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("coverage shm stat: %w", err)
	}
	if uint64(fi.Size()) < totalSize {
		return nil, fmt.Errorf("coverage shm too small: %d < %d (no coverage bitmap; patched QEMU required)", fi.Size(), totalSize)
	}

	// Map only the coverage region: 64K bitmap + 8-byte generation counter.
	mapOffset := int64(ramSize)
	mapSize := coverageMapSize + 8

	// mmap requires a page-aligned offset.
	pageSize := int64(syscall.Getpagesize())
	alignedOffset := (mapOffset / pageSize) * pageSize
	extraBytes := mapOffset - alignedOffset
	alignedSize := int(int64(mapSize) + extraBytes)

	mapped, err := syscall.Mmap(int(f.Fd()), alignedOffset, alignedSize,
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("coverage shm mmap: %w", err)
	}

	return &ShmCoverageReader{
		name:      name,
		shmPath:   shmPath,
		ramSize:   ramSize,
		rawMapped: mapped,
		mapped:    mapped[extraBytes:],
	}, nil
}

// Name returns the identifier for this coverage source.
func (r *ShmCoverageReader) Name() string {
	return r.name
}

// Collect reads the coverage bitmap from shared memory. Returns nil if the
// generation counter has not changed since the last read.
func (r *ShmCoverageReader) Collect() ([]byte, error) {
	if len(r.mapped) < coverageMapSize+8 {
		return nil, fmt.Errorf("coverage shm: mapped region too small")
	}

	gen := binary.LittleEndian.Uint64(r.mapped[coverageMapSize:])
	if gen == r.lastGen {
		return nil, nil // no new coverage since last read
	}
	r.lastGen = gen

	// Return a copy; do not expose the mmap'd slice directly.
	buf := make([]byte, coverageMapSize)
	copy(buf, r.mapped[:coverageMapSize])
	return buf, nil
}

// Close unmaps the shared memory region.
func (r *ShmCoverageReader) Close() error {
	if r.rawMapped != nil {
		err := syscall.Munmap(r.rawMapped)
		r.rawMapped = nil
		r.mapped = nil
		return err
	}
	return nil
}
