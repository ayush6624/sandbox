//go:build linux

package main

import (
	"log"
	"net"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"golang.org/x/sys/unix"
)

func runThawWakeListener() {
	for {
		if err := listenThawWake(); err != nil {
			log.Printf("thaw wake: %v (retrying)", err)
			time.Sleep(time.Second)
		}
	}
}

func listenThawWake() error {
	ifi, err := net.InterfaceByName(mmdsIface)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(agentapi.ThawWakeEtherType)))
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{
		Protocol: htons(agentapi.ThawWakeEtherType),
		Ifindex:  ifi.Index,
	}); err != nil {
		return err
	}

	buf := make([]byte, 128)
	for {
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if !isThawWakeFrame(buf[:n]) {
			continue
		}
		select {
		case thawPollWake <- struct{}{}:
		default:
		}
	}
}
