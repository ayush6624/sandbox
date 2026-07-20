package vm

import (
	"encoding/json"
	"testing"
)

func TestResolvePage(t *testing.T) {
	const kib4 = 4096
	// Two regions: the first backed from mem-file offset 0, the second from a
	// nonzero offset (as a multi-region snapshot would lay them out).
	regions := []guestRegion{
		{BaseHostVirtAddr: 0x1000_0000, Size: 0x10000, Offset: 0, PageSizeKiB: 4},
		{BaseHostVirtAddr: 0x2000_0000, Size: 0x10000, Offset: 0x10000, PageSizeKiB: 4},
	}

	tests := []struct {
		name        string
		addr        uint64
		wantAligned uint64
		wantSrcOff  uint64
		wantPage    uint64
		wantOK      bool
	}{
		{"region0 page-aligned start", 0x1000_0000, 0x1000_0000, 0, kib4, true},
		{"region0 unaligned mid-page", 0x1000_0abc, 0x1000_0000, 0, kib4, true},
		{"region0 second page", 0x1000_1000, 0x1000_1000, 0x1000, kib4, true},
		{"region0 unaligned second page", 0x1000_1fff, 0x1000_1000, 0x1000, kib4, true},
		{"region1 start maps to its offset", 0x2000_0000, 0x2000_0000, 0x10000, kib4, true},
		{"region1 third page", 0x2000_2345, 0x2000_2000, 0x12000, kib4, true},
		{"below all regions", 0x0fff_ffff, 0, 0, 0, false},
		{"gap between regions", 0x1800_0000, 0, 0, 0, false},
		{"at region0 end (exclusive)", 0x1001_0000, 0, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			aligned, srcOff, page, ok := resolvePage(regions, tc.addr)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if aligned != tc.wantAligned {
				t.Errorf("aligned = %#x, want %#x", aligned, tc.wantAligned)
			}
			if srcOff != tc.wantSrcOff {
				t.Errorf("srcOff = %#x, want %#x", srcOff, tc.wantSrcOff)
			}
			if page != tc.wantPage {
				t.Errorf("pageSize = %d, want %d", page, tc.wantPage)
			}
			// dst and src must move in lockstep: the page's offset from its
			// region base is identical on both sides of the copy.
			i := regionOf(t, regions, tc.addr)
			if aligned-regions[i].BaseHostVirtAddr != srcOff-regions[i].Offset {
				t.Errorf("dst/src delta mismatch: aligned=%#x srcOff=%#x", aligned, srcOff)
			}
		})
	}
}

func regionOf(t *testing.T, regions []guestRegion, addr uint64) int {
	t.Helper()
	for i, r := range regions {
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			return i
		}
	}
	t.Fatalf("addr %#x in no region", addr)
	return -1
}

// TestResolvePageDefaultPageSize covers a mapping that omits page_size_kib
// (defaults to 4096) — a safety net against a Firecracker JSON field rename.
func TestResolvePageDefaultPageSize(t *testing.T) {
	regions := []guestRegion{{BaseHostVirtAddr: 0x4000, Size: 0x8000, Offset: 0x100000}}
	_, srcOff, page, ok := resolvePage(regions, 0x5abc)
	if !ok {
		t.Fatal("expected hit")
	}
	if page != 4096 {
		t.Fatalf("pageSize = %d, want 4096 default", page)
	}
	if srcOff != 0x100000+(0x5000-0x4000) {
		t.Fatalf("srcOff = %#x, want %#x", srcOff, 0x100000+0x1000)
	}
}

// TestResolvePageNoUnderflow is the regression for the crash: a fault whose
// page-aligned address falls below the region base (e.g. a base not aligned to
// the page size) must NOT match — the old code matched on the raw address and
// then underflowed aligned-base into a ~2^64 offset that panicked the indexer.
func TestResolvePageNoUnderflow(t *testing.T) {
	// Region base is 0x800 into a page; a fault at base aligns down to 0x...000,
	// which is below base.
	regions := []guestRegion{{BaseHostVirtAddr: 0x1000_0800, Size: 0x10000, Offset: 0, PageSizeKiB: 4}}
	if _, srcOff, _, ok := resolvePage(regions, 0x1000_0800); ok {
		t.Fatalf("expected no match (aligned below base), got srcOff=%#x", srcOff)
	}
}

// TestResolvePageHugePage covers 2 MiB pages (page_size_kib=2048), the layout
// whose -2 MiB underflow surfaced the bug on the fleet.
func TestResolvePageHugePage(t *testing.T) {
	const twoMiB = 2 * 1024 * 1024
	regions := []guestRegion{{BaseHostVirtAddr: 0x4000_0000, Size: 0x4000_0000, Offset: 0, PageSizeKiB: 2048}}
	aligned, srcOff, page, ok := resolvePage(regions, 0x4020_1234)
	if !ok {
		t.Fatal("expected hit")
	}
	if page != twoMiB {
		t.Fatalf("pageSize = %d, want %d", page, twoMiB)
	}
	if aligned != 0x4020_0000 || srcOff != 0x20_0000 {
		t.Fatalf("aligned=%#x srcOff=%#x, want 0x40200000 / 0x200000", aligned, srcOff)
	}
}

// TestResolvePagePageStraddlesEnd rejects a page that would run past the region
// end (keeps a copy from reading beyond the mapped region).
func TestResolvePagePageStraddlesEnd(t *testing.T) {
	// size is one byte into the last page, so the aligned last page + pageSize
	// exceeds base+size.
	regions := []guestRegion{{BaseHostVirtAddr: 0x2000_0000, Size: 0x1001, Offset: 0, PageSizeKiB: 4}}
	if _, _, _, ok := resolvePage(regions, 0x2000_1000); ok {
		t.Fatal("expected no match: page straddles region end")
	}
}

// TestGuestRegionJSON pins the wire field names Firecracker sends.
func TestGuestRegionJSON(t *testing.T) {
	const body = `[{"base_host_virt_addr":123456,"size":65536,"offset":4096,"page_size_kib":4}]`
	var regions []guestRegion
	if err := json.Unmarshal([]byte(body), &regions); err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 {
		t.Fatalf("got %d regions", len(regions))
	}
	r := regions[0]
	if r.BaseHostVirtAddr != 123456 || r.Size != 65536 || r.Offset != 4096 || r.PageSizeKiB != 4 {
		t.Fatalf("bad decode: %+v", r)
	}
}
