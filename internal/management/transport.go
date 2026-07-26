package management

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type TransportMode string

const (
	TransportDevelopment  TransportMode = "development"
	TransportTLS          TransportMode = "tls"
	TransportPrivateProxy TransportMode = "private_proxy"
)

type Transport struct {
	Mode     TransportMode
	CertFile string
	KeyFile  string
}

// ValidateListener makes plaintext an explicit choice and only accepts it on
// an address that is verifiably private. Development mode is the sole
// compatibility escape hatch.
func (t Transport) ValidateListener(addr string) error {
	switch t.Mode {
	case TransportTLS:
		if strings.TrimSpace(t.CertFile) == "" || strings.TrimSpace(t.KeyFile) == "" {
			return errors.New("management TLS requires both certificate and key files")
		}
	case TransportPrivateProxy:
		if !IsPrivateAddress(addr) {
			return fmt.Errorf("private_proxy management listener %q is not a verifiably private IP (wildcard and public binds are refused)", addr)
		}
	case TransportDevelopment:
		return nil
	case "":
		return errors.New("management_transport is required for TCP: choose tls, private_proxy, or explicit development")
	default:
		return fmt.Errorf("unknown management_transport %q", t.Mode)
	}
	return nil
}

func IsPrivateAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || cgnat.Contains(ip)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func IsEncryptedOrPrivateEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme == "https" {
		return u.Host != ""
	}
	if u.Scheme != "http" {
		return false
	}
	return IsPrivateAddress(u.Host)
}

func (t Transport) TLSConfig() (*tls.Config, error) {
	if t.Mode != TransportTLS {
		return nil, nil
	}
	reloader, err := newCertificateReloader(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: reloader.GetCertificate,
	}, nil
}

// certificateReloader picks up atomically replaced certificate/key files on
// the next handshake. A malformed intermediate update retains the last
// known-good certificate, so rotation does not interrupt established traffic.
type certificateReloader struct {
	certFile string
	keyFile  string

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

func newCertificateReloader(certFile, keyFile string) (*certificateReloader, error) {
	r := &certificateReloader{certFile: certFile, keyFile: keyFile}
	if err := r.reload(true); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *certificateReloader) reload(required bool) error {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		if required {
			return fmt.Errorf("stat TLS certificate: %w", err)
		}
		return nil
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		if required {
			return fmt.Errorf("stat TLS key: %w", err)
		}
		return nil
	}
	r.mu.RLock()
	unchanged := certInfo.ModTime().Equal(r.certMod) && keyInfo.ModTime().Equal(r.keyMod)
	r.mu.RUnlock()
	if unchanged {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		if required {
			return fmt.Errorf("load management TLS key pair: %w", err)
		}
		return nil
	}
	r.mu.Lock()
	r.cert = &cert
	r.certMod = certInfo.ModTime()
	r.keyMod = keyInfo.ModTime()
	r.mu.Unlock()
	return nil
}

func (r *certificateReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	_ = r.reload(false)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cert == nil {
		return nil, errors.New("management TLS certificate unavailable")
	}
	return r.cert, nil
}
