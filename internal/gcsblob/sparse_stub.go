//go:build !linux

package gcsblob

import "os"

// dataRanges on non-Linux treats the whole file as one data range. GCS
// snapshot durability only runs on Linux hosts (metadata-server auth); this
// keeps the package compiling in the macOS dev build.
func dataRanges(f *os.File) ([]Range, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return nil, nil
	}
	return []Range{{Off: 0, Len: fi.Size()}}, nil
}
