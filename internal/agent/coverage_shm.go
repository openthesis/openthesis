package agent

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

const coverageMapSize = 1 << 16 // 64K edges, matches QEMU patch

// ShmCoverageReader reads coverage data directly from QEMU's shared memory
// region, bypassing virtio-serial. The coverage bitmap is placed at offset
// ram_size within /dev/shm/openthesis-{name} by the patched QEMU.
//
// Layout:
//
//	[0,           ramSize)       guest RAM
//	[ramSize,     ramSize+64K)   coverage bitmap
//	[ramSize+64K, ramSize+64K+8) generation counter (uint64)
type ShmCoverageReader struct {
	name      string
	shmPath   string
	ramSize   uint64
	rawMapped []byte // original mmap'd region (for Munmap)
	mapped    []byte // sub-slice starting at coverage bitmap
	lastGen   uint64
}

// NewShmCoverageReader creates a coverage reader that mmaps the shared memory
// file at the coverage bitmap offset.
//
// Returns an error if the shm file doesn't contain the coverage bitmap
// (stock QEMU without patches). This prevents SIGBUS from accessing
// pages beyond the file's actual size on tmpfs.
func NewShmCoverageReader(name, shmPath string, ramSizeMB uint64) (*ShmCoverageReader, error) {
	ramSize := ramSizeMB * 1024 * 1024
	totalSize := ramSize + coverageMapSize + 8

	f, err := os.OpenFile(shmPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("coverage shm open: %w", err)
	}
	defer f.Close()

	// Verify the file is large enough to contain the coverage bitmap.
	// Stock QEMU (TCG) creates shm files of exactly ramSize; accessing
	// beyond that via mmap causes SIGBUS on tmpfs.
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("coverage shm stat: %w", err)
	}
	if uint64(fi.Size()) < totalSize {
		return nil, fmt.Errorf("coverage shm too small: %d < %d (no coverage bitmap; patched QEMU required)", fi.Size(), totalSize)
	}

	// mmap just the coverage region (64K + 8 bytes for generation counter).
	mapOffset := int64(ramSize)
	mapSize := coverageMapSize + 8

	// Align offset to page boundary.
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

// Collect reads the coverage bitmap from shared memory.
// Returns nil if the generation counter hasn't changed since last read.
func (r *ShmCoverageReader) Collect() ([]byte, error) {
	if len(r.mapped) < coverageMapSize+8 {
		return nil, fmt.Errorf("coverage shm: mapped region too small")
	}

	// Read generation counter.
	gen := binary.LittleEndian.Uint64(r.mapped[coverageMapSize:])
	if gen == r.lastGen {
		return nil, nil // No new coverage since last read.
	}
	r.lastGen = gen

	// Copy bitmap (don't return the mmap'd slice directly).
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
