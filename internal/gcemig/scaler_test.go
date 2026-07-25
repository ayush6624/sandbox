package gcemig

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestScaleOutCapsAndOnlyGrows(t *testing.T) {
	var target atomic.Int64
	target.Store(2)
	var resizeCalls atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if got := r.Header.Get("Metadata-Flavor"); got != "Google" {
				t.Errorf("metadata flavor = %q, want Google", got)
			}
			_, _ = w.Write([]byte(`{"access_token":"test-token","expires_in":3600}`))
		case r.Method == http.MethodGet:
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"targetSize":` + strconv.FormatInt(target.Load(), 10) + `}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/p/zones/z/instanceGroupManagers/m/resize":
			resizeCalls.Add(1)
			if got := r.URL.Query().Get("size"); got != "5" {
				t.Errorf("resize size = %q, want capped size 5", got)
			}
			target.Store(5)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s, err := New("p", "z", "m", 5)
	if err != nil {
		t.Fatal(err)
	}
	s.apiBase = srv.URL
	s.tokenURL = srv.URL + "/token"
	s.client = srv.Client()

	if err := s.ScaleOut(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if err := s.ScaleOut(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	if got := resizeCalls.Load(); got != 1 {
		t.Fatalf("resize calls = %d, want 1", got)
	}
}
