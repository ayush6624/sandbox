package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestCreateProgressCanceledInvalidRetryReturnsMemberConflict(t *testing.T) {
	s := createRequestServer(t)
	s.createSem = make(chan struct{}, 1)
	s.createSem <- struct{}{}
	member := createops.Member{ID: "canceled-invalid-retry", Spec: createops.Spec{}}
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var executionErr error
	go func() {
		_, executionErr = s.Execute(ctx, createops.Worker{RegistryID: s.reg.RegistryID()}, command)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	first := waitCreateStage(t, s, member.ID, registry.CreateStageAdmission)
	cancel()
	<-done
	if !errors.Is(executionErr, context.Canceled) {
		t.Fatalf("canceled create: %v", executionErr)
	}
	<-s.createSem
	if _, err := s.reg.CreateRequest(context.Background(), memberIntent(t, member)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("canceled create committed a ledger outcome: %v", err)
	}

	command.Members[0].Spec.Lifecycle.TTLSeconds = -1
	data, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/create-commands", bytes.NewReader(data))
	response := httptest.NewRecorder()
	s.handleCreateCommand(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("conflicting retry returned transport HTTP %d: %s", response.Code, response.Body.String())
	}
	var outcomes []createops.Outcome
	if err := json.Unmarshal(response.Body.Bytes(), &outcomes); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].ID != member.ID || outcomes[0].Failure == nil || outcomes[0].Failure.Status != http.StatusConflict || outcomes[0].Failure.Code != "create_request_conflict" {
		t.Fatalf("conflicting retry outcomes: %+v", outcomes)
	}
	retained, err := s.reg.CreateProgress(context.Background(), member.ID)
	if err != nil || retained.Sequence != first.Sequence || retained.Attempt != first.Attempt || retained.Condition != "active" || retained.Outcome != nil {
		t.Fatalf("conflicting retry changed retained progress: %+v %v", retained, err)
	}
	if _, err := s.reg.CreateRequest(context.Background(), memberIntent(t, member)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("conflicting retry committed a ledger outcome: %v", err)
	}
}
