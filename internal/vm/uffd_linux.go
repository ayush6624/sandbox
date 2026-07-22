//go:build linux

package vm

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
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
// The page bytes come from a pageSource (uffd_source.go). The default
// localSource mmaps the mem file read-only and serves faults straight out of the
// page cache, so it doubles as lazy disk I/O: a guest page fault reads the
// backing file page only if it isn't already cached. The source is deliberately
// just "the local mem file" for now; a remote/GCS-backed source slots in behind
// the same interface for cross-host restore (Model B in the scale-to-zero doc,
// roadmap Phase B) without touching the fault loop below.

// uffdHandler services page faults for one restored VM out of its page source,
// for the lifetime of that VM.
type uffdHandler struct {
	sockPath string
	ln       *net.UnixListener
	src      pageSource
	fatal    fatalOnce   // kills the VM if a fault can't be served (see below)
	hist     latencyHist // per-fault source-fetch latency (logged at teardown)

	// stopFD is an eventfd in faultLoop's poll set; close() signals it to make the
	// loop exit deterministically when Firecracker goes away. We do NOT rely on the
	// uffd delivering POLLHUP on FC exit — measured on the fleet, it does not fire
	// reliably, so the loop hung and leaked the fault goroutine + the page-source
	// mapping (a 1 GiB mmap for localSource) on every wake. FC's process exit IS
	// reliably observed (cmd.Wait → close()), so that drives teardown instead.
	stopFD int
	stopMu sync.Mutex // serializes signalStop vs closeStop (no write-after-close)

	closeOnce sync.Once // guards listener close + stop signal + socket removal
	srcOnce   sync.Once // guards src.close() + stopFD close (owned by fault goroutine)
}

// startUffdHandler binds the handler socket and serves Firecracker's connection
// in the background over the given (already-built) page source. It must be
// listening before the snapshot-load call (Firecracker dials it during load).
// On any error the caller owns closing src.
func startUffdHandler(sockPath string, src pageSource) (*uffdHandler, error) {
	_ = os.Remove(sockPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen uffd socket: %w", err)
	}
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("uffd stop eventfd: %w", err)
	}
	h := &uffdHandler{sockPath: sockPath, ln: ln, src: src, stopFD: efd}
	go h.accept()
	return h, nil
}

// accept waits for Firecracker's single connection and serves it. The fault
// goroutine owns the page source's lifetime: it closes only after serve()
// returns (Firecracker gone), so a page copy can never race the unmap — unlike
// close(), which runs from other goroutines and must not touch the source.
func (h *uffdHandler) accept() {
	defer h.releaseSource()
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
	// Non-blocking + poll(): a blocking read does NOT reliably wake when
	// Firecracker exits. poll() over the uffd plus the stop eventfd (signaled by
	// close() on FC process-exit) gives faultLoop a deterministic exit — the uffd's
	// own POLLHUP is unreliable on FC teardown (see the stopFD comment).
	if err := unix.SetNonblock(uffd, true); err != nil {
		fmt.Fprintf(os.Stderr, "uffd: set nonblock: %v\n", err)
	}

	var regions []guestRegion
	if err := json.Unmarshal(body[:n], &regions); err != nil {
		fmt.Fprintf(os.Stderr, "uffd: parse mappings %q: %v\n", string(body[:n]), err)
		return
	}

	h.faultLoop(uffd, regions)
	fmt.Fprintf(os.Stderr, "uffd: handler exiting: %s\n", h.hist.summary())
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
	// Poll the uffd for faults AND the stop eventfd for teardown. The stop fd is
	// the reliable exit (close() signals it on FC process-exit); the uffd's own
	// POLLHUP is kept as a belt-and-suspenders secondary but is not depended on.
	pfd := []unix.PollFd{
		{Fd: int32(uffd), Events: unix.POLLIN},
		{Fd: int32(h.stopFD), Events: unix.POLLIN},
	}
	for {
		pfd[0].Revents, pfd[1].Revents = 0, 0
		if _, err := unix.Poll(pfd, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return
		}
		// Stop eventfd signaled by close() = Firecracker exited. Return so serve()
		// logs the summary and releaseSource() unmaps — the fix for the leak.
		if pfd[1].Revents&unix.POLLIN != 0 {
			return
		}
		// POLLHUP/POLLERR = Firecracker's mm is gone (secondary signal; may not
		// fire on every kernel/FC — hence the stop eventfd above).
		if pfd[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return
		}
		if pfd[0].Revents&unix.POLLIN == 0 {
			continue
		}
		n, err := unix.Read(uffd, buf)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			return
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
			h.copyWindow(uffd, regions, addr)
		}
	}
}

// copyWindow installs the faulting page plus a fault-ahead run into the guest
// in one UFFDIO_COPY, read from the mem file at the region-mapped offset. One
// syscall for many pages is the whole point — it amortizes the userspace
// round-trip that made single-page UFFD lose to the eager File backend.
func (h *uffdHandler) copyWindow(uffd int, regions []guestRegion, addr uint64) {
	dst, srcOff, length, ok := faultWindow(regions, addr, prefetchPages)
	if !ok {
		fmt.Fprintf(os.Stderr, "uffd: fault @%#x maps to no region\n", addr)
		return
	}
	h.copyRange(uffd, dst, srcOff, length)
}

// copyRange asks the page source for [srcOff, srcOff+length) and installs those
// bytes at guest host addr dst in one UFFDIO_COPY. Shared by fault-ahead and
// prewarm. The source clamps the run (short read → the tail refaults later) and
// owns the bytes' lifetime for the duration of the copy.
func (h *uffdHandler) copyRange(uffd int, dst, srcOff, length uint64) {
	t0 := time.Now()
	buf, err := h.src.at(srcOff, length)
	h.hist.record(time.Since(t0)) // source-fetch latency the faulting vCPU waited on
	// Running summary every 512 faults: the teardown log depends on faultLoop
	// exiting (POLLHUP), which isn't guaranteed on every FC teardown, so this
	// surfaces p50/p99 during the wake itself.
	if c := h.hist.count.Load(); c%512 == 0 {
		fmt.Fprintf(os.Stderr, "uffd: fault progress: %s\n", h.hist.summary())
	}
	if err != nil {
		// The source could not supply the page (after its own retries), so this
		// fault is left UNSERVED and Firecracker would wait forever on it. Kill
		// the VM instead — the wake fails cleanly, exactly like a failed
		// File-backend wake, rather than hanging the guest. localSource never
		// errors; the GCS source (B2) does. fatal fires once, then poll() sees
		// POLLHUP as FC dies and serve() tears down normally.
		fmt.Fprintf(os.Stderr, "uffd: source at %#x len %d: %v\n", srcOff, length, err)
		h.fatal.fire(err)
		return
	}
	if len(buf) == 0 {
		return // nothing at this offset (past image end); guest refaults if needed
	}
	arg := uffdioCopyArg{
		Dst: dst,
		Src: uint64(uintptr(unsafe.Pointer(&buf[0]))),
		Len: uint64(len(buf)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(uffd), uintptr(uffdioCopy), uintptr(unsafe.Pointer(&arg)))
	// EEXIST: part of the run was already populated (prewarm, a prior fault-ahead,
	// or a racing fault) — benign; a faulting page is at the run's head and gets
	// copied up to the first present page, so the guest still progresses.
	// EAGAIN: the mapping changed under us (removed) — the guest will refault.
	if errno != 0 && errno != unix.EEXIST && errno != unix.EAGAIN {
		fmt.Fprintf(os.Stderr, "uffd: copy %d bytes @%#x: %v\n", len(buf), dst, errno)
	}
}

// close unblocks a pending accept() and removes the socket. It deliberately
// does NOT close the page source: Firecracker may still fault pages during its
// own shutdown, so the source (and its mmap) lives until the fault goroutine
// sees the VM exit and calls releaseSource(). Idempotent; safe to call from any
// goroutine (restore failure, machine stop, the wait goroutine on FC exit).
func (h *uffdHandler) close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		h.signalStop() // wake faultLoop's poll() so it exits and releaseSource runs
		if h.ln != nil {
			_ = h.ln.Close()
		}
		_ = os.Remove(h.sockPath)
	})
}

// signalStop wakes faultLoop's poll() by writing the stop eventfd. Guarded by
// stopMu against closeStop so it never writes a since-closed (possibly reused)
// fd. Safe to call before faultLoop starts (the write just stays pending).
func (h *uffdHandler) signalStop() {
	h.stopMu.Lock()
	defer h.stopMu.Unlock()
	if h.stopFD >= 0 {
		_, _ = unix.Write(h.stopFD, []byte{1, 0, 0, 0, 0, 0, 0, 0})
	}
}

// closeStop closes the stop eventfd. Called only from releaseSource (the fault
// goroutine), after faultLoop has returned — so no poll or signalStop can still
// be using it.
func (h *uffdHandler) closeStop() {
	h.stopMu.Lock()
	defer h.stopMu.Unlock()
	if h.stopFD >= 0 {
		_ = unix.Close(h.stopFD)
		h.stopFD = -1
	}
}

// releaseSource closes the page source (unmap + file close for localSource) and
// the stop eventfd. Called only from the fault goroutine's defer, after serve()
// has returned — so no page copy is in flight and faultLoop is done polling.
func (h *uffdHandler) releaseSource() {
	h.srcOnce.Do(func() {
		if h.src != nil {
			if err := h.src.close(); err != nil {
				fmt.Fprintf(os.Stderr, "uffd: close page source: %v\n", err)
			}
		}
		h.closeStop()
	})
}
