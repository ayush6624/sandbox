package gateway

import (
	"net"
	"testing"
)

// The edge feeds host_addr straight to net.Dial. Heartbeats register a worker's
// addr as a URL, so emitting it verbatim made every public ingress request fail
// with "too many colons in address" — a 502 on the whole data path.
func TestDialAddrIsDialable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://10.160.0.48:8080", "10.160.0.48:8080"},
		{"https://10.160.0.48:8443", "10.160.0.48:8443"},
		{"http://worker.internal:8080", "worker.internal:8080"},
		// No explicit port: infer from scheme rather than dialing port-less.
		{"http://10.160.0.48", "10.160.0.48:80"},
		{"https://10.160.0.48", "10.160.0.48:443"},
		// Already a dial target: pass through untouched.
		{"10.160.0.48:8080", "10.160.0.48:8080"},
		// IPv6 must stay bracketed or SplitHostPort rejects it.
		{"http://[2001:db8::1]:8080", "[2001:db8::1]:8080"},
	} {
		got := dialAddr(tc.in)
		if got != tc.want {
			t.Errorf("dialAddr(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		// Whatever we emit must survive the parse net.Dial performs.
		if _, _, err := net.SplitHostPort(got); err != nil {
			t.Errorf("dialAddr(%q) = %q is not a valid dial target: %v", tc.in, got, err)
		}
	}
}

// hostOnly and dialAddr are not interchangeable: hostOnly deliberately drops the
// port for callers that report a host port separately. Guard against someone
// "simplifying" one into the other.
func TestHostOnlyIsNotADialTarget(t *testing.T) {
	if got := hostOnly("http://10.160.0.48:8080"); got == dialAddr("http://10.160.0.48:8080") {
		t.Fatalf("hostOnly and dialAddr must differ; both returned %q", got)
	}
}
