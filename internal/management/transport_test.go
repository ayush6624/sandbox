package management

import (
	"os"
	"path/filepath"
	"testing"
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
