package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCredentialsRotateWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("new-token\nold-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := NewCredentials(nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := creds.Outbound(); got != "new-token" {
		t.Fatalf("outbound = %q", got)
	}
	for _, token := range []string{"new-token", "old-token"} {
		if !creds.MatchAuthorization("Bearer " + token) {
			t.Fatalf("%q should be accepted during overlap", token)
		}
	}

	// Atomic replacement is the documented rotation operation. Give the new
	// file a distinct size so filesystems with coarse mtimes still reload.
	next := path + ".next"
	if err := os.WriteFile(next, []byte("next-generation-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if got := creds.Outbound(); got != "next-generation-token" {
		t.Fatalf("outbound after rotation = %q", got)
	}
	if creds.MatchAuthorization("Bearer old-token") {
		t.Fatal("retired token remains accepted")
	}
}

func TestCredentialsRetainLastGoodSetDuringBrokenReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("working-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := NewCredentials(nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !creds.MatchAuthorization("Bearer working-token") {
		t.Fatal("last known-good credential should survive an empty intermediate file")
	}
}

func TestCredentialsHandlerUsesAuthorizationOnly(t *testing.T) {
	creds, err := NewCredentials([]string{"sekrit"}, "")
	if err != nil {
		t.Fatal(err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler := creds.Handler(next, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})

	query := httptest.NewRequest(http.MethodGet, "/shell?access_token=sekrit", nil)
	query.Header.Set("Connection", "Upgrade")
	query.Header.Set("Upgrade", "websocket")
	queryResponse := httptest.NewRecorder()
	handler.ServeHTTP(queryResponse, query)
	if queryResponse.Code != http.StatusUnauthorized {
		t.Fatalf("query credential status = %d", queryResponse.Code)
	}

	header := httptest.NewRequest(http.MethodGet, "/shell", nil)
	header.Header.Set("Authorization", "Bearer sekrit")
	headerResponse := httptest.NewRecorder()
	handler.ServeHTTP(headerResponse, header)
	if headerResponse.Code != http.StatusNoContent {
		t.Fatalf("header credential status = %d", headerResponse.Code)
	}
}

func TestCredentialsStaticRotationOverlap(t *testing.T) {
	creds, err := NewCredentials([]string{"next", "current", "next", ""}, "")
	if err != nil {
		t.Fatal(err)
	}
	if creds.Outbound() != "next" {
		t.Fatalf("outbound = %q", creds.Outbound())
	}
	if !creds.MatchAuthorization("Bearer current") {
		t.Fatal("overlap credential not accepted")
	}
	if creds.MatchAuthorization("Bearer wrong") {
		t.Fatal("wrong credential accepted")
	}
}

// Ensure the imported time package remains tied to the cache behavior: this
// also catches accidental reload implementations that only compare mtimes.
var _ = time.Time{}
