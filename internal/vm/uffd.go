package vm

// Platform-neutral pieces of the UFFD memory backend (see uffd_linux.go for the
// syscall/mmap/socket handler). Kept untagged so the fault-offset arithmetic —
// the correctness core — is unit-testable on any host.

// uffdioCopy is the UFFDIO_COPY ioctl request: _IOWR(UFFDIO=0xAA, 0x03,
// sizeof(uffdioCopyArg)=40) = 0xc028aa03. Constant across amd64/arm64 Linux.
const uffdioCopy = 0xc028aa03

// uffdEventPagefault is UFFD_EVENT_PAGEFAULT — the only event a MISSING-mode
// registration delivers that we act on.
const uffdEventPagefault = 0x12

// uffdMsgSize is sizeof(struct uffd_msg). Reads on the uffd return whole
// multiples of this; the faulting address of a pagefault event lives at
// offset 16 (u8 event @0, 7B reserved, then the union: u64 flags @8, u64
// address @16).
const uffdMsgSize = 32

// uffdioCopyArg mirrors struct uffdio_copy: copy Len bytes from Src (our
// address space) into Dst (Firecracker's), waking the faulting thread.
type uffdioCopyArg struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// guestRegion is one entry of the JSON mapping array Firecracker sends: a guest
// memory region living at BaseHostVirtAddr in Firecracker's address space,
// backed by the mem file starting at Offset.
type guestRegion struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSizeKiB      uint64 `json:"page_size_kib"`
}

// resolvePage maps a faulting host virtual address to the page-aligned
// destination address, the source offset in the mem file, and the page size,
// by locating the region that contains it. ok=false means no region does
// (which shouldn't happen for a fault the kernel delivered).
func resolvePage(regions []guestRegion, addr uint64) (aligned, srcOff, pageSize uint64, ok bool) {
	for _, r := range regions {
		if addr < r.BaseHostVirtAddr || addr >= r.BaseHostVirtAddr+r.Size {
			continue
		}
		pageSize = r.PageSizeKiB * 1024
		if pageSize == 0 {
			pageSize = 4096
		}
		aligned = addr &^ (pageSize - 1)
		srcOff = r.Offset + (aligned - r.BaseHostVirtAddr)
		return aligned, srcOff, pageSize, true
	}
	return 0, 0, 0, false
}
