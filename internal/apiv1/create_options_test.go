package apiv1

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestCreateResourceAndIdleOptions(t *testing.T) {
	cases := []struct {
		name, body string
		resources  *createops.Resources
		idle       int
		invalid    bool
	}{
		{name: "omitted", body: `{}`},
		{name: "empty", body: `{"resources":{}}`},
		{name: "zero", body: `{"resources":{"vcpu":0,"memory_mib":0}}`},
		{name: "cpu only", body: `{"resources":{"vcpu":2}}`, resources: &createops.Resources{VCPU: 2}},
		{name: "memory only", body: `{"resources":{"memory_mib":512}}`, resources: &createops.Resources{MemoryMIB: 512}},
		{name: "both", body: `{"resources":{"vcpu":2,"memory_mib":512}}`, resources: &createops.Resources{VCPU: 2, MemoryMIB: 512}},
		{name: "template default", body: `{"source":{"type":"template","id":"default"},"resources":{"vcpu":2}}`, resources: &createops.Resources{VCPU: 2}},
		{name: "snapshot defaults", body: `{"source":{"type":"snapshot","id":"s"},"resources":{"vcpu":0}}`},
		{name: "disabled idle", body: `{"lifecycle":{"idle_timeout_seconds":-1}}`, idle: -1},
		{name: "snapshot override", body: `{"source":{"type":"snapshot","id":"s"},"resources":{"vcpu":2}}`, invalid: true},
		{name: "template override", body: `{"source":{"type":"template","id":"s"},"resources":{"memory_mib":512}}`, invalid: true},
		{name: "negative cpu", body: `{"resources":{"vcpu":-1}}`, invalid: true},
		{name: "small memory", body: `{"resources":{"memory_mib":127}}`, invalid: true},
		{name: "negative memory", body: `{"resources":{"memory_mib":-1}}`, invalid: true},
		{name: "fractional cpu", body: `{"resources":{"vcpu":1.5}}`, invalid: true},
		{name: "invalid idle", body: `{"lifecycle":{"idle_timeout_seconds":-2}}`, invalid: true},
		{name: "invalid ttl", body: `{"lifecycle":{"ttl_seconds":-1}}`, invalid: true},
	}
	for _, route := range []string{"/v1/sandbox-creations", "/v1/sandbox-batches"} {
		for _, tc := range cases {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				store, err := createops.Open(filepath.Join(t.TempDir(), "operations.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				h := NewWithCreateOperations(http.NotFoundHandler(), store, recoveryExecutor{})
				mux := http.NewServeMux()
				h.Register(mux)
				body := tc.body
				if route == "/v1/sandbox-batches" {
					body = fmt.Sprintf(`{"count":1,"sandbox":%s}`, body)
				}
				req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
				req.Header.Set("Idempotency-Key", "option")
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				page, err := store.List(context.Background(), createops.ListQuery{Limit: 100})
				ops := page.Operations
				if err != nil {
					t.Fatal(err)
				}
				if tc.invalid {
					if w.Code != 400 || len(ops) != 0 {
						t.Fatalf("invalid request accepted: %d %s %+v", w.Code, w.Body.String(), ops)
					}
					return
				}
				if w.Code != 202 || len(ops) != 1 || len(ops[0].Members) != 1 {
					t.Fatalf("acceptance: %d %s %+v", w.Code, w.Body.String(), ops)
				}
				spec := ops[0].Members[0].Spec
				if !reflect.DeepEqual(spec.Resources, tc.resources) || spec.Lifecycle.IdleTimeoutSeconds != tc.idle {
					t.Fatalf("stored request: %+v", spec)
				}
			})
		}
	}
}

func TestPublicLifecycleUpdatePreservesDisabledIdle(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		initial, want int
		invalid       bool
	}{
		{name: "disable", body: `{"lifecycle":{"idle_timeout_seconds":-1}}`, initial: 10, want: -1},
		{name: "ttl while disabled", body: `{"lifecycle":{"ttl_seconds":60}}`, initial: -1, want: -1},
		{name: "host default", body: `{"lifecycle":{"idle_timeout_seconds":0}}`, initial: -1, want: 0},
		{name: "invalid", body: `{"lifecycle":{"idle_timeout_seconds":-2}}`, initial: -1, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			patches := 0
			sandbox := registry.Sandbox{ID: "s", Status: registry.StatusRunning, CreatedAt: time.Now().UTC(), Vcpus: 1, MemMIB: 128, HibernateAfterSec: tc.initial}
			legacy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					patches++
					var body struct {
						Idle int `json:"idle_timeout_seconds"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.Idle != tc.want {
						t.Fatalf("forwarded idle %d want %d", body.Idle, tc.want)
					}
					sandbox.HibernateAfterSec = body.Idle
				}
				_ = json.NewEncoder(w).Encode(sandbox)
			})
			h := New(legacy)
			mux := http.NewServeMux()
			h.Register(mux)
			req := httptest.NewRequest(http.MethodPatch, "/v1/sandboxes/s", strings.NewReader(tc.body))
			req.Header.Set("Idempotency-Key", "idle")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if tc.invalid {
				if w.Code != 400 || patches != 0 {
					t.Fatalf("invalid update: %d %s patches=%d", w.Code, w.Body.String(), patches)
				}
				return
			}
			var result Sandbox
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || patches != 1 || result.Lifecycle.IdleTimeoutSeconds != tc.want {
				t.Fatalf("update: %d %s patches=%d", w.Code, w.Body.String(), patches)
			}
		})
	}
}
