package management

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTransportValidation(t *testing.T) {
	tests := []struct {
		name    string
		mode    TransportMode
		addr    string
		cert    string
		key     string
		wantErr bool
	}{
		{"missing mode", "", "127.0.0.1:8080", "", "", true},
		{"development public", TransportDevelopment, "0.0.0.0:8080", "", "", false},
		{"private rfc1918", TransportPrivateProxy, "10.0.0.2:8080", "", "", false},
		{"private tailnet", TransportPrivateProxy, "100.69.9.101:8080", "", "", false},
		{"private loopback", TransportPrivateProxy, "127.0.0.1:8080", "", "", false},
		{"private wildcard", TransportPrivateProxy, "0.0.0.0:8080", "", "", true},
		{"private public", TransportPrivateProxy, "8.8.8.8:8080", "", "", true},
		{"tls missing files", TransportTLS, "0.0.0.0:8080", "", "", true},
		{"tls configured", TransportTLS, "0.0.0.0:8080", "cert", "key", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (Transport{Mode: tt.mode, CertFile: tt.cert, KeyFile: tt.key}).ValidateListener(tt.addr)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestEncryptedOrPrivateEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://api.example.com:9090",
		"http://10.2.0.3:9090",
		"http://100.69.9.101:9090",
	} {
		if !IsEncryptedOrPrivateEndpoint(endpoint) {
			t.Fatalf("%s should be accepted", endpoint)
		}
	}
	for _, endpoint := range []string{
		"http://8.8.8.8:9090",
		"http://0.0.0.0:9090",
		"http://gateway.example.com:9090",
	} {
		if IsEncryptedOrPrivateEndpoint(endpoint) {
			t.Fatalf("%s should be rejected", endpoint)
		}
	}
}

func TestTLSConfigRequiresReadablePair(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Transport{Mode: TransportTLS, CertFile: cert, KeyFile: key}).TLSConfig(); err == nil {
		t.Fatal("malformed TLS pair accepted")
	}
}

func TestCertificateReloadsAfterAtomicRotation(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeTestKeyPair(t, certPath, keyPath, 1)
	reloader, err := newCertificateReloader(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	before, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	beforeDER := append([]byte(nil), before.Certificate[0]...)

	nextCert := certPath + ".next"
	nextKey := keyPath + ".next"
	writeTestKeyPair(t, nextCert, nextKey, 2)
	if err := os.Rename(nextCert, certPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(nextKey, keyPath); err != nil {
		t.Fatal(err)
	}
	after, err := reloader.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeDER) == string(after.Certificate[0]) {
		t.Fatal("certificate did not reload after atomic replacement")
	}
}

func writeTestKeyPair(t *testing.T, certPath, keyPath string, serial int64) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "management.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"management.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}
