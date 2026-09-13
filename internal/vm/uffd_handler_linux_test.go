//go:build linux

package vm

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type trackedFaultSource struct {
	read   func(uint64, uint64) ([]byte, error)
	closed chan struct{}
	closes atomic.Int32
}

func (s *trackedFaultSource) at(off, n uint64) ([]byte, error) { return s.read(off, n) }
func (s *trackedFaultSource) close() error {
	if s.closes.Add(1) == 1 {
		close(s.closed)
	}
	return nil
}

func TestUFFDHandlerCloseReleasesSourceDuringHandshake(t *testing.T) {
	for _, connected := range []bool{false, true} {
		name := "before accept"
		if connected {
			name = "awaiting mappings"
		}
		t.Run(name, func(t *testing.T) {
			src := &trackedFaultSource{closed: make(chan struct{})}
			h, err := startUffdHandler(filepath.Join(t.TempDir(), "uffd"), src)
			if err != nil {
				t.Fatal(err)
			}
			defer h.close()
			if connected {
				c, err := net.Dial("unix", h.sockPath)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				deadline := time.Now().Add(time.Second)
				for {
					h.connMu.Lock()
					accepted := h.conn != nil
					h.connMu.Unlock()
					if accepted {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("connection not accepted")
					}
					time.Sleep(time.Millisecond)
				}
			}
			h.close()
			h.close()
			select {
			case <-src.closed:
			case <-time.After(time.Second):
				t.Fatal("source leaked")
			}
			if src.closes.Load() != 1 {
				t.Fatalf("source closes=%d", src.closes.Load())
			}
		})
	}
}

func TestUFFDHandlerCloseReleasesExternalChunkSource(t *testing.T) {
	closed := make(chan struct{})
	page, err := buildUFFDSource(RunOptions{UFFDChunks: &UFFDChunkSource{
		Total: 4096, ChunkSize: 4096,
		Load:  func(uint64) ([]byte, error) { return make([]byte, 4096), nil },
		Close: func() error { close(closed); return nil },
	}}, "unused")
	if err != nil {
		t.Fatal(err)
	}
	h, err := startUffdHandler(filepath.Join(t.TempDir(), "uffd"), page)
	if err != nil {
		t.Fatal(err)
	}
	h.close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("handler did not release external source")
	}
}

func TestUFFDFaultFailuresAreFatal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		read   func(uint64, uint64) ([]byte, error)
		mapped bool
	}{
		{"unmapped", nil, false},
		{"source error", func(uint64, uint64) ([]byte, error) { return nil, errors.New("storage unavailable") }, true},
		{"empty", func(uint64, uint64) ([]byte, error) { return nil, nil }, true},
		{"partial page", func(uint64, uint64) ([]byte, error) { return make([]byte, 1), nil }, true},
		{"permanent ioctl", func(uint64, uint64) ([]byte, error) { return make([]byte, 4096), nil }, true},
		{"panic", func(uint64, uint64) ([]byte, error) { panic("loader panic") }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(stop)
			var pipe [2]int
			if err := unix.Pipe2(pipe[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
				t.Fatal(err)
			}
			defer unix.Close(pipe[0])
			defer unix.Close(pipe[1])
			h := &uffdHandler{stopFD: stop, src: &trackedFaultSource{read: tc.read}}
			fatal := make(chan error, 1)
			h.fatal.set(func(err error) { fatal <- err; h.signalStop() })
			done := make(chan struct{})
			var regions []guestRegion
			if tc.mapped {
				regions = []guestRegion{{BaseHostVirtAddr: 0x10000, Size: 4096, PageSize: 4096}}
			}
			go func() { h.faultLoop(pipe[0], regions); close(done) }()
			msg := make([]byte, uffdMsgSize)
			msg[0] = uffdEventPagefault
			binary.LittleEndian.PutUint64(msg[16:24], 0x10000)
			if _, err := unix.Write(pipe[1], msg); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-fatal:
				if err == nil {
					t.Fatal("nil fatal error")
				}
			case <-time.After(time.Second):
				h.signalStop()
				t.Fatal("unserved fault did not stop VM")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("fault loop did not stop")
			}
		})
	}
}

func TestUFFDHandlerCloseWaitsForActiveFault(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	src := &trackedFaultSource{closed: make(chan struct{}), read: func(uint64, uint64) ([]byte, error) {
		close(entered)
		<-release
		return make([]byte, 4096), nil
	}}
	h, err := startUffdHandler(filepath.Join(t.TempDir(), "uffd"), src)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: h.sockPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	defer unix.Close(pipe[1])
	mappings, _ := json.Marshal([]guestRegion{{BaseHostVirtAddr: 0x10000, Size: 4096, PageSize: 4096}})
	if _, _, err := conn.WriteMsgUnix(mappings, unix.UnixRights(pipe[0]), nil); err != nil {
		t.Fatal(err)
	}
	msg := make([]byte, uffdMsgSize)
	msg[0] = uffdEventPagefault
	binary.LittleEndian.PutUint64(msg[16:24], 0x10000)
	if _, err := unix.Write(pipe[1], msg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("fault was not received")
	}
	h.close()
	select {
	case <-src.closed:
		t.Error("source released during a live copy")
	default:
	}
	close(release)
	select {
	case <-src.closed:
	case <-time.After(time.Second):
		t.Fatal("source was not released after copy finished")
	}
	if src.closes.Load() != 1 {
		t.Fatalf("source closes=%d", src.closes.Load())
	}
}
