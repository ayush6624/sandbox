package client

import "testing"

func TestNewHTTPPreservesEncryptedEndpoint(t *testing.T) {
	c := NewHTTP("https://worker.example:8443/", "token")
	if c.baseURL != "https://worker.example:8443" {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
	if c.wsURL != "wss://worker.example:8443" {
		t.Fatalf("wsURL = %q", c.wsURL)
	}
}

func TestNewHTTPDefaultsLegacyAddressToHTTP(t *testing.T) {
	c := NewHTTP("127.0.0.1:8080", "token")
	if c.baseURL != "http://127.0.0.1:8080" {
		t.Fatalf("baseURL = %q", c.baseURL)
	}
}
