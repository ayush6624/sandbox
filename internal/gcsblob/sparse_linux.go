//go:build linux

package gcsblob

import (
	"errors"
	"os"
	"syscall"
)

// Linux lseek whence values for sparse-file navigation (unistd.h).
const (
	seekData = 3 // SEEK_DATA
	seekHole = 4 // SEEK_HOLE
)

// dataRanges enumerates the allocated (non-hole) regions of f via
// SEEK_DATA/SEEK_HOLE, so encoding skips holes entirely. On filesystems
// without hole support the whole file reports as one data range.
func dataRanges(f *os.File) ([]Range, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	var out []Range
	var off int64
	for off < size {
		dataOff, err := seekWhence(f, off, seekData)
		if err != nil {
			// ENXIO: no more data past off — the rest is one hole.
			if errors.Is(err, syscall.ENXIO) {
				break
			}
			return nil, err
		}
		if dataOff >= size {
			break
		}
		holeOff, err := seekWhence(f, dataOff, seekHole)
		if err != nil {
			if errors.Is(err, syscall.ENXIO) {
				holeOff = size
			} else {
				return nil, err
			}
		}
		if holeOff > size {
			holeOff = size
		}
		out = append(out, Range{Off: dataOff, Len: holeOff - dataOff})
		off = holeOff
	}
	return out, nil
}

func seekWhence(f *os.File, off int64, whence int) (int64, error) {
	return f.Seek(off, whence)
}
