//go:build linux

package provisioner

import (
	"fmt"
	"net"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"golang.org/x/sys/unix"
)

// WakeThawAgent sends a private Ethernet notification directly into an
// unbridged clone tap. The guest uses it to interrupt its MMDS polling timer;
// failure is harmless because polling remains active.
func WakeThawAgent(tap string) error {
	ifi, err := net.InterfaceByName(tap)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(agentapi.ThawWakeEtherType)))
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	dst := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	src := ifi.HardwareAddr
	if len(src) != 6 {
		src = net.HardwareAddr{0x02, 0, 0, 0, 0, 1}
	}
	frame := make([]byte, 14+len(agentapi.ThawWakeMagic))
	copy(frame[0:6], dst)
	copy(frame[6:12], src)
	frame[12] = byte(agentapi.ThawWakeEtherType >> 8)
	frame[13] = byte(agentapi.ThawWakeEtherType & 0xff)
	copy(frame[14:], agentapi.ThawWakeMagic)

	addr := &unix.SockaddrLinklayer{
		Protocol: htons(agentapi.ThawWakeEtherType),
		Ifindex:  ifi.Index,
		Halen:    6,
	}
	copy(addr.Addr[:], dst)
	for range 3 {
		if err := unix.Sendto(fd, frame, 0, addr); err != nil {
			return fmt.Errorf("send thaw wake: %w", err)
		}
	}
	return nil
}
