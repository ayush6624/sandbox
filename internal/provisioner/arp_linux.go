//go:build linux

package provisioner

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// ARPListener sniffs ARP frames on one tap device. Fan-out clones resume on an
// unbridged tap carrying the snapshot's baked IP; once the in-guest thaw agent
// adopts the clone's fresh identity it broadcasts a gratuitous ARP (see
// cmd/sandboxd announceIdentity), which is the host's signal that the tap is
// safe to bridge. Open the listener BEFORE resuming the clone or the announce
// can be missed.
type ARPListener struct {
	fd int
}

// ListenARP opens an AF_PACKET socket bound to tap that receives only ARP.
func ListenARP(tap string) (*ARPListener, error) {
	ifi, err := net.InterfaceByName(tap)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return nil, err
	}
	sll := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifi.Index}
	if err := unix.Bind(fd, sll); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &ARPListener{fd: fd}, nil
}

// WaitForSenderIP blocks until an ARP frame whose sender protocol address is
// ip arrives on the tap, or timeout passes.
func (l *ARPListener) WaitForSenderIP(ip string, timeout time.Duration) error {
	want := net.ParseIP(ip).To4()
	if want == nil {
		return fmt.Errorf("bad IPv4 %q", ip)
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 128)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return fmt.Errorf("no ARP from %s within %s", ip, timeout)
		}
		pfds := []unix.PollFd{{Fd: int32(l.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfds, int(remain/time.Millisecond)+1)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if n == 0 {
			continue // poll timed out; loop re-checks the deadline
		}
		nr, _, err := unix.Recvfrom(l.fd, buf, 0)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		// Ethernet (14) + ARP for IPv4 over ethernet (28): sender IP at 28:32.
		if nr < 42 || buf[12] != 0x08 || buf[13] != 0x06 {
			continue
		}
		if buf[16] != 0x08 || buf[17] != 0x00 || buf[18] != 6 || buf[19] != 4 {
			continue
		}
		if want[0] == buf[28] && want[1] == buf[29] && want[2] == buf[30] && want[3] == buf[31] {
			return nil
		}
	}
}

// Close releases the socket.
func (l *ARPListener) Close() error { return unix.Close(l.fd) }

func htons(v uint16) uint16 { return v<<8 | v>>8 }
