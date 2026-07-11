package server

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry.db"), registry.Pools{
		TapPrefix:  "fc",
		TapMax:     3,
		GuestIPMin: "172.16.0.10",
		GuestIPMax: "172.16.0.12",
		PortMin:    5200,
		PortMax:    5202,
	})
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { reg.Close() })
	return New(Config{}, reg)
}

func TestValidateResources(t *testing.T) {
	for _, tc := range []struct {
		name         string
		vcpus, mem   int64
		wantErr      bool
		errSubstring string
	}{
		{name: "zero means template default", vcpus: 0, mem: 0},
		{name: "sane override", vcpus: 1, mem: 512},
		{name: "negative vcpus", vcpus: -1, wantErr: true, errSubstring: "vcpus"},
		{name: "negative mem", mem: -1, wantErr: true, errSubstring: "mem_mib"},
		{name: "absurd vcpus", vcpus: 10_000, wantErr: true, errSubstring: "exceeds host limit"},
		{name: "absurd mem", mem: 1 << 40, wantErr: true, errSubstring: "exceeds host limit"},
		{name: "unbootably small mem", mem: 64, wantErr: true, errSubstring: "minimum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResources(tc.vcpus, tc.mem)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateResources(%d, %d) should fail", tc.vcpus, tc.mem)
				}
				if !strings.Contains(err.Error(), tc.errSubstring) {
					t.Fatalf("error %q should mention %q", err, tc.errSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateResources(%d, %d): %v", tc.vcpus, tc.mem, err)
			}
		})
	}
}

func TestCreateRejectsBadResourceOverrides(t *testing.T) {
	s := testServer(t)
	for _, body := range []string{
		`{"vcpus": -1}`,
		`{"mem_mib": -1}`,
		`{"vcpus": 10000}`,
		`{"mem_mib": 1099511627776}`,
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/sandboxes", strings.NewReader(body))
		s.handleCreate(w, r)
		if w.Code != 400 {
			t.Fatalf("create with body %s: got %d, want 400 (body: %s)", body, w.Code, w.Body)
		}
	}
}

// Restore and fanout must reject resource overrides outright: vcpus/mem are
// baked into a snapshot when it is taken, so accepting them would silently
// lie. The check runs before any snapshot lookup, so a bogus snapshot id
// still yields the 400.
func TestRestoreAndFanoutRejectResourceOverrides(t *testing.T) {
	s := testServer(t)
	for _, tc := range []struct {
		handler string
		body    string
	}{
		{"restore", `{"vcpus": 2}`},
		{"restore", `{"mem_mib": 2048}`},
		{"fanout", `{"count": 2, "vcpus": 2}`},
		{"fanout", `{"count": 2, "mem_mib": 2048}`},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/snapshots/nonexistent/"+tc.handler, strings.NewReader(tc.body))
		r.SetPathValue("id", "nonexistent")
		if tc.handler == "restore" {
			s.handleRestore(w, r)
		} else {
			s.handleFanout(w, r)
		}
		if w.Code != 400 {
			t.Fatalf("%s with body %s: got %d, want 400 (body: %s)", tc.handler, tc.body, w.Code, w.Body)
		}
		if !strings.Contains(w.Body.String(), "baked into the snapshot") {
			t.Fatalf("%s rejection should explain resources are baked in, got: %s", tc.handler, w.Body)
		}
	}

	// Overrides of 0 are the wire default and must NOT trip the rejection —
	// an absent field decodes to 0. (The bogus snapshot id then 404s.)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/snapshots/nonexistent/restore", strings.NewReader(`{"vcpus": 0, "mem_mib": 0}`))
	r.SetPathValue("id", "nonexistent")
	s.handleRestore(w, r)
	if w.Code != 404 {
		t.Fatalf("restore with zero overrides should pass validation and 404 on the snapshot, got %d", w.Code)
	}
}
