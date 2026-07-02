//go:build linux

package main

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// announceIdentity broadcasts gratuitous ARP replies for ip/mac on iface. A
// fan-out clone's tap is unbridged until the guest sheds the snapshot's baked
// identity; the host sniffs the tap for this announce (provisioner.ARPListener)
// and bridges the instant it arrives — instead of sleeping a fixed margin.
// Sent a few times because ARP is fire-and-forget.
func announceIdentity(iface, ipStr, macStr string) error {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return fmt.Errorf("bad IPv4 %q", ipStr)
	}
	mac, err := net.ParseMAC(macStr)
	if err != nil || len(mac) != 6 {
		return fmt.Errorf("bad MAC %q: %v", macStr, err)
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return err
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	bcast := [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	dst := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  ifi.Index,
		Halen:    6,
		Addr:     [8]byte{bcast[0], bcast[1], bcast[2], bcast[3], bcast[4], bcast[5]},
	}

	// Ethernet header + gratuitous ARP reply: sender == target == our new IP.
	frame := make([]byte, 42)
	copy(frame[0:6], bcast[:])
	copy(frame[6:12], mac)
	frame[12], frame[13] = 0x08, 0x06 // ethertype ARP
	arp := frame[14:]
	arp[0], arp[1] = 0x00, 0x01 // htype ethernet
	arp[2], arp[3] = 0x08, 0x00 // ptype IPv4
	arp[4], arp[5] = 6, 4       // hlen, plen
	arp[6], arp[7] = 0x00, 0x02 // op: reply
	copy(arp[8:14], mac)        // sender MAC
	copy(arp[14:18], ip)        // sender IP
	copy(arp[18:24], bcast[:])  // target MAC
	copy(arp[24:28], ip)        // target IP

	for i := 0; i < 3; i++ {
		if err := unix.Sendto(fd, frame, 0, dst); err != nil {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }
