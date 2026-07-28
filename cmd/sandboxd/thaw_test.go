package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplyIdentityUsesOneValidatedIPBatch(t *testing.T) {
	oldRun := runIPBatch
	t.Cleanup(func() { runIPBatch = oldRun })

	var got string
	runIPBatch = func(batch string) ([]byte, error) {
		got = batch
		return nil, nil
	}
	id := cloneIdentity{
		IP: "172.16.0.12", MAC: "02:00:00:00:00:12",
		GW: "172.16.0.1", Prefix: "24",
	}
	if err := applyIdentity(id); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"link set dev eth0 down",
		"addr add 172.16.0.12/24 dev eth0",
		"route replace default via 172.16.0.1",
		"route replace 169.254.169.254/32 dev eth0",
	} {
		if !strings.Contains(got, line+"\n") {
			t.Fatalf("batch missing %q:\n%s", line, got)
		}
	}

	if err := applyIdentity(cloneIdentity{
		IP: "172.16.0.12\nlink delete eth0", MAC: id.MAC, GW: id.GW, Prefix: id.Prefix,
	}); err == nil {
		t.Fatal("batch command injection was accepted")
	}
}

func TestSnapshotPollHandlerArmsAndDisarms(t *testing.T) {
	snapshotPollArmed.Store(false)
	t.Cleanup(func() {
		snapshotPollArmed.Store(false)
		for {
			select {
			case <-thawPollWake:
			default:
				return
			}
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/snapshot-poll", strings.NewReader(`{"armed":true}`))
	rec := httptest.NewRecorder()
	handleSnapshotPoll(rec, req)
	if rec.Code != http.StatusOK || !snapshotPollArmed.Load() {
		t.Fatalf("arm response=%d armed=%t body=%s", rec.Code, snapshotPollArmed.Load(), rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/snapshot-poll", strings.NewReader(`{"armed":false}`))
	rec = httptest.NewRecorder()
	handleSnapshotPoll(rec, req)
	if rec.Code != http.StatusOK || snapshotPollArmed.Load() {
		t.Fatalf("disarm response=%d armed=%t body=%s", rec.Code, snapshotPollArmed.Load(), rec.Body.String())
	}
}
