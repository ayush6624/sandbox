package main

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

var defaultGateway = gatewayFromProcRoute

// hostOnly prevents an unprivileged workload inside the guest from invoking
// sandboxd's narrow privileged operations (clock stepping, network/SSH
// re-identification, and login-key installation). The host reaches sandboxd
// from the guest's default gateway address; local user processes do not.
func hostOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			httpError(w, http.StatusForbidden, errors.New("privileged endpoint requires host control source"))
			return
		}
		source, err := netip.ParseAddr(host)
		if err != nil {
			httpError(w, http.StatusForbidden, errors.New("privileged endpoint requires host control source"))
			return
		}
		gateway, err := defaultGateway()
		if err != nil || source.Unmap() != gateway.Unmap() {
			httpError(w, http.StatusForbidden, errors.New("privileged endpoint requires host control source"))
			return
		}
		next(w, r)
	}
}

// gatewayFromProcRoute returns the IPv4 gateway for the default route. Linux
// exposes it as a little-endian hexadecimal integer in /proc/net/route.
func gatewayFromProcRoute() (netip.Addr, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil || flags&0x2 == 0 { // RTF_GATEWAY
			continue
		}
		raw, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("parse default gateway %q: %w", fields[2], err)
		}
		return netip.AddrFrom4([4]byte{
			byte(raw),
			byte(raw >> 8),
			byte(raw >> 16),
			byte(raw >> 24),
		}), nil
	}
	if err := scanner.Err(); err != nil {
		return netip.Addr{}, err
	}
	return netip.Addr{}, errors.New("default gateway not found")
}
