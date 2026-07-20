//go:build linux

package vm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// UFFD memory backend for snapshot restore.
//
// The default File backend faults the ENTIRE guest RAM out of the snapshot mem
// file before ResumeVM returns — the bulk of the ~1 s wake latency, paid in
// full even though a freshly-woken guest touches only a small working set.
//
// With the UFFD backend, Firecracker registers the guest memory with
// userfaultfd, connects to a handler over a unix socket, and hands it the uffd
// (via SCM_RIGHTS) plus a JSON description of the guest memory regions. The
// guest then resumes IMMEDIATELY with no memory paged in; every page it touches
// faults to us and we copy it in from the mem file on demand. Wake latency
// tracks the working set, not the guest size, and wake I/O collapses to the
// pages actually read. See docs/scale-to-zero.md.
//
// The handler mmaps the mem file read-only and serves faults straight out of
// the page cache, so it doubles as lazy disk I/O: a guest page fault reads the
// backing file page only if it isn't already cached. The page source is
// deliberately just "the local mem file" for now; the same fault path is where
// a remote/GCS-backed source would slot in for cross-host restore (Model B in
// the scale-to-zero doc).

// uffdHandler services page faults for one restored VM out of its snapshot mem
// file, for the lifetime of that VM.
type uffdHandler struct {
	sockPath string
	ln       *net.UnixListener
	memFile  *os.File
	mem      []byte // read-only mmap of the whole mem file

	closeOnce sync.Once // guards listener close + socket removal
	memOnce   sync.Once // guards mem unmap + file close (owned by the fault goroutine)
}

// startUffdHandler binds the handler socket and mmaps the mem file, then serves
// Firecracker's connection in the background. It must be listening before the
// snapshot-load call (Firecracker dials it during load).
func startUffdHandler(sockPath, memPath string) (*uffdHandler, error) {
	_ = os.Remove(sockPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen uffd socket: %w", err)
	}
	f, err := os.Open(memPath)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("open mem file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("stat mem file: %w", err)
	}
	mem, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		_ = f.Close()
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("mmap mem file: %w", err)
	}
	h := &uffdHandler{sockPath: sockPath, ln: ln, memFile: f, mem: mem}
	go h.accept()
	return h, nil
}

// accept waits for Firecracker's single connection and serves it. The fault
// goroutine owns the mem mapping's lifetime: it unmaps only after serve()
// returns (Firecracker gone), so a page copy can never race an unmap — unlike
// close(), which runs from other goroutines and must not touch the mapping.
func (h *uffdHandler) accept() {
	defer h.releaseMem()
	conn, err := h.ln.AcceptUnix()
	if err != nil {
		return // listener closed before Firecracker connected (restore failed)
	}
	h.serve(conn)
}

// serve receives the guest memory mappings and the uffd fd, then services
// faults until Firecracker exits (the uffd read fails) or the handler closes.
func (h *uffdHandler) serve(conn *net.UnixConn) {
	defer conn.Close()

	body := make([]byte, 4096)
	oob := make([]byte, unix.CmsgSpace(4)) // room for a single fd
	n, oobn, _, _, err := conn.ReadMsgUnix(body, oob)
	if err != nil {
		fmt.Fprintf(os.Stderr, "uffd: recv mappings: %v\n", err)
		return
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(scms) == 0 {
		fmt.Fprintf(os.Stderr, "uffd: parse control message: %v\n", err)
		return
	}
	fds, err := unix.ParseUnixRights(&scms[0])
	if err != nil || len(fds) == 0 {
		fmt.Fprintf(os.Stderr, "uffd: no fd in control message: %v\n", err)
		return
	}
	uffd := fds[0]
	defer unix.Close(uffd)
	// Firecracker may hand us a non-blocking uffd; force blocking so our
	// dedicated read loop parks on it instead of spinning on EAGAIN. One OS
	// thread per awake UFFD-restored VM — fine at the "tens–hundreds awake"
	// scale this targets.
	if err := unix.SetNonblock(uffd, false); err != nil {
		fmt.Fprintf(os.Stderr, "uffd: set blocking: %v\n", err)
	}

	var regions []guestRegion
	if err := json.Unmarshal(body[:n], &regions); err != nil {
		fmt.Fprintf(os.Stderr, "uffd: parse mappings %q: %v\n", string(body[:n]), err)
		return
	}
	h.faultLoop(uffd, regions)
}

// faultLoop reads pagefault events off the uffd and copies each faulting page
// in from the mem file. It returns when the uffd is no longer readable —
// normally because Firecracker exited (freeze/destroy tore the VM down).
//
// A recover() guards the whole loop: a page-fault handler is on the critical
// path of a live guest, but it runs inside the shared serve process, so a bug
// here must degrade to "this one wake fails" — never crash serve and take every
// sandbox on the host down with it.
func (h *uffdHandler) faultLoop(uffd int, regions []guestRegion) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "uffd: fault loop panic (wake will fail, serve survives): %v\n", r)
		}
	}()
	// One-time dump of the real region layout — base/size/offset/page size —
	// so a wrong assumption is diagnosable from the logs, not just a crash.
	fmt.Fprintf(os.Stderr, "uffd: serving %d region(s): %+v\n", len(regions), regions)
	buf := make([]byte, uffdMsgSize*16) // batch several events per read
	for {
		n, err := unix.Read(uffd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return // Firecracker gone (or fd closed) — stop serving
		}
		if n <= 0 {
			return
		}
		for off := 0; off+uffdMsgSize <= n; off += uffdMsgSize {
			msg := buf[off : off+uffdMsgSize]
			if msg[0] != uffdEventPagefault {
				continue // ignore non-fault events (e.g. REMOVE)
			}
			addr := binary.LittleEndian.Uint64(msg[16:24])
			h.copyPage(uffd, regions, addr)
		}
	}
}

// copyPage installs the one page containing addr into the guest, read from the
// mem file at the region-mapped offset.
func (h *uffdHandler) copyPage(uffd int, regions []guestRegion, addr uint64) {
	aligned, srcOff, pageSize, ok := resolvePage(regions, addr)
	if !ok {
		fmt.Fprintf(os.Stderr, "uffd: fault @%#x maps to no region\n", addr)
		return
	}
	// Overflow-safe bounds check: srcOff+pageSize can wrap past 2^64 and
	// silently pass a naive `> len` comparison (this is what let the original
	// underflow panic reach the slice index). Compare without adding.
	memLen := uint64(len(h.mem))
	if srcOff > memLen || pageSize > memLen-srcOff {
		fmt.Fprintf(os.Stderr, "uffd: page @%#x src %#x+%d out of mem (len %d) — skipping\n", aligned, srcOff, pageSize, memLen)
		return
	}
	arg := uffdioCopyArg{
		Dst: aligned,
		Src: uint64(uintptr(unsafe.Pointer(&h.mem[srcOff]))),
		Len: pageSize,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uintptr(uffdioCopy), uintptr(unsafe.Pointer(&arg)))
	// EEXIST: another fault already populated this page — benign. EAGAIN: the
	// mapping changed under us (removed) — the guest will refault.
	if errno != 0 && errno != unix.EEXIST && errno != unix.EAGAIN {
		fmt.Fprintf(os.Stderr, "uffd: copy page @%#x: %v\n", aligned, errno)
	}
}

// close unblocks a pending accept() and removes the socket. It deliberately
// does NOT unmap the mem file: Firecracker may still fault pages during its own
// shutdown, so the mapping lives until the fault goroutine sees the VM exit and
// calls releaseMem(). Idempotent; safe to call from any goroutine (restore
// failure, machine stop, the wait goroutine on Firecracker exit).
func (h *uffdHandler) close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		if h.ln != nil {
			_ = h.ln.Close()
		}
		_ = os.Remove(h.sockPath)
	})
}

// releaseMem unmaps the mem file and closes it. Called only from the fault
// goroutine's defer, after serve() has returned — so no page copy is in flight.
func (h *uffdHandler) releaseMem() {
	h.memOnce.Do(func() {
		if h.mem != nil {
			_ = unix.Munmap(h.mem)
		}
		if h.memFile != nil {
			_ = h.memFile.Close()
		}
	})
}
