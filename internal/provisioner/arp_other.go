//go:build !linux

package provisioner

import (
	"errors"
	"time"
)

// ARPListener needs AF_PACKET; on non-Linux ListenARP fails and callers fall
// back to the fixed reidentify sleep. Signatures must match arp_linux.go.
type ARPListener struct{}

func ListenARP(tap string) (*ARPListener, error) {
	return nil, errors.New("arp listener: linux only")
}

func (l *ARPListener) WaitForSenderIP(ip string, timeout time.Duration) error {
	return errors.New("arp listener: linux only")
}

func (l *ARPListener) Close() error { return nil }
