package createops

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestSingleAcceptanceRemainsSeparateFromFinalResponse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operations.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	req := request("single", "single-scope", "single-hash")
	req.Type = "sandbox_create"
	accepted, err := s.Accept(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Record(ctx, req.ID, []Outcome{{ID: req.Members[0].ID, Failure: &Failure{Status: 409, Code: "denied"}}}); err != nil {
		t.Fatal(err)
	}
	final := []byte(`{"code":"denied", "retained":true}`)
	if err := s.SetFinalResponse(ctx, req.ID, 409, final); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Accept(ctx, req)
	if err != nil || replay.Created || replay.ResponseStatus != 202 || !bytes.Equal(replay.ResponseBody, accepted.ResponseBody) {
		t.Fatalf("acceptance: %+v %v", replay, err)
	}
	status, body, ready, err := s.FinalResponse(ctx, req.ID)
	if err != nil || !ready || status != 409 || !bytes.Equal(body, final) {
		t.Fatalf("final: %d %s %v %v", status, body, ready, err)
	}
}
