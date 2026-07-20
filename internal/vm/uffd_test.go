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
		{BaseHostVirtAddr: 0x1000_0000, Size: 0x10000, Offset: 0, PageSize: 4},
		{BaseHostVirtAddr: 0x2000_0000, Size: 0x10000, Offset: 0x10000, PageSize: 4},
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
	regions := []guestRegion{{BaseHostVirtAddr: 0x1000_0800, Size: 0x10000, Offset: 0, PageSize: 4}}
	if _, srcOff, _, ok := resolvePage(regions, 0x1000_0800); ok {
		t.Fatalf("expected no match (aligned below base), got srcOff=%#x", srcOff)
	}
}

// TestResolvePageHugePage covers 2 MiB pages (page_size_kib=2048), the layout
// whose -2 MiB underflow surfaced the bug on the fleet.
func TestResolvePageHugePage(t *testing.T) {
	const twoMiB = 2 * 1024 * 1024
	regions := []guestRegion{{BaseHostVirtAddr: 0x4000_0000, Size: 0x4000_0000, Offset: 0, PageSize: 2048}}
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
	regions := []guestRegion{{BaseHostVirtAddr: 0x2000_0000, Size: 0x1001, Offset: 0, PageSize: 4}}
	if _, _, _, ok := resolvePage(regions, 0x2000_1000); ok {
		t.Fatal("expected no match: page straddles region end")
	}
}

// TestPageSizeBytes pins the unit normalization, including Firecracker v1.15's
// real behavior: page_size_kib carries BYTES (4096), not KiB.
func TestPageSizeBytes(t *testing.T) {
	cases := []struct {
		field uint64
		want  uint64
	}{
		{0, 4096},       // absent → 4 KiB default
		{4, 4096},       // documented KiB unit: 4 KiB
		{2048, 2 << 20}, // KiB unit: 2 MiB huge page
		{4096, 4096},    // FC v1.15 actual: bytes, 4 KiB page (the crash's origin)
	}
	for _, c := range cases {
		if got := (guestRegion{PageSize: c.field}).pageSizeBytes(); got != c.want {
			t.Errorf("pageSizeBytes(%d) = %d, want %d", c.field, got, c.want)
		}
	}
}

// TestResolvePageRealFirecracker replays the exact region the fleet logged
// (base 4 KiB-page-aligned host VA, 1 GiB, page_size_kib=4096-as-bytes): a
// fault must page in at true 4 KiB granularity, not 4 MiB.
func TestResolvePageRealFirecracker(t *testing.T) {
	base := uint64(124664269504512) // 0x716c00000000, from the fleet log
	regions := []guestRegion{{BaseHostVirtAddr: base, Size: 1 << 30, Offset: 0, PageSize: 4096}}
	aligned, srcOff, page, ok := resolvePage(regions, base+0x1234)
	if !ok {
		t.Fatal("expected hit")
	}
	if page != 4096 {
		t.Fatalf("pageSize = %d, want 4096 (4 KiB, not 4 MiB)", page)
	}
	if aligned != base+0x1000 || srcOff != 0x1000 {
		t.Fatalf("aligned=%#x srcOff=%#x, want base+0x1000 / 0x1000", aligned, srcOff)
	}
}

// TestFaultWindow covers fault-ahead: the run spans the faulting page plus
// prefetch-1 following pages, is clamped to the region end, and never crosses
// a boundary.
func TestFaultWindow(t *testing.T) {
	const p = 4096
	// One region: base 0x1000_0000, 64 KiB (16 pages), backed from offset 0x8000.
	regions := []guestRegion{{BaseHostVirtAddr: 0x1000_0000, Size: 0x10000, Offset: 0x8000, PageSize: 4096}}

	t.Run("full window mid-region", func(t *testing.T) {
		dst, srcOff, length, ok := faultWindow(regions, 0x1000_0abc, 4)
		if !ok || dst != 0x1000_0000 || srcOff != 0x8000 || length != 4*p {
			t.Fatalf("got dst=%#x srcOff=%#x len=%d ok=%v", dst, srcOff, length, ok)
		}
	})
	t.Run("clamped to region end", func(t *testing.T) {
		// Fault in the 15th page (0-indexed 14); only 2 pages remain, so a
		// prefetch of 8 clamps to 2 pages.
		addr := regions[0].BaseHostVirtAddr + 14*p + 10
		dst, srcOff, length, ok := faultWindow(regions, addr, 8)
		if !ok || dst != regions[0].BaseHostVirtAddr+14*p || length != 2*p {
			t.Fatalf("got dst=%#x len=%d ok=%v, want dst=%#x len=%d", dst, length, ok, regions[0].BaseHostVirtAddr+14*p, 2*p)
		}
		if srcOff != 0x8000+14*p {
			t.Fatalf("srcOff=%#x, want %#x", srcOff, 0x8000+14*p)
		}
	})
	t.Run("prefetch 1 is a single page", func(t *testing.T) {
		_, _, length, ok := faultWindow(regions, 0x1000_0000, 1)
		if !ok || length != p {
			t.Fatalf("len=%d ok=%v, want %d", length, ok, p)
		}
	})
	t.Run("prefetch 0 defaults to 1 page", func(t *testing.T) {
		_, _, length, ok := faultWindow(regions, 0x1000_0000, 0)
		if !ok || length != p {
			t.Fatalf("len=%d ok=%v, want %d", length, ok, p)
		}
	})
	t.Run("last page: window is exactly one page", func(t *testing.T) {
		addr := regions[0].BaseHostVirtAddr + 15*p // final page
		_, _, length, ok := faultWindow(regions, addr, 8)
		if !ok || length != p {
			t.Fatalf("len=%d ok=%v, want %d", length, ok, p)
		}
	})
	t.Run("no region", func(t *testing.T) {
		if _, _, _, ok := faultWindow(regions, 0x2000_0000, 4); ok {
			t.Fatal("expected no match")
		}
	})
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
	if r.BaseHostVirtAddr != 123456 || r.Size != 65536 || r.Offset != 4096 || r.PageSize != 4 {
		t.Fatalf("bad decode: %+v", r)
	}
}
