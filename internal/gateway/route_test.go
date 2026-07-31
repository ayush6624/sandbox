package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandleRouteReturnsDirectWorkerCredential(t *testing.T) {
	g := liveGateway(&host{
		id: "worker-a", addr: "10.0.0.2:8080", token: "worker-token",
		slotsTotal: 2, lastSeen: time.Now(),
	})
	g.route["sandbox-a"] = "worker-a"
	req := httptest.NewRequest("GET", "/route/sandbox-a", nil)
	req.SetPathValue("id", "sandbox-a")
	w := httptest.NewRecorder()
	g.handleRoute(w, req)
	if w.Code != 200 {
		t.Fatalf("route: %d %s", w.Code, w.Body)
	}
	var got edgeRoute
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.HostAddr != "10.0.0.2:8080" || got.Token != "worker-token" || got.TTL != 5 {
		t.Fatalf("route = %+v", got)
	}
}
