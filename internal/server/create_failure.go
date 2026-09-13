package server

import (
	"context"
	"log"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

type createFailure uint8

const (
	createFailureRootfs createFailure = iota
	createFailureNetwork
	createFailureVM
	createFailureAgent
	createFailureIdentity
	createFailurePublish
)

var createFailureCauses = [...]registry.CreateFailure{
	{Status: 500, Code: "rootfs_prepare_failed", Detail: "Sandbox filesystem preparation failed."},
	{Status: 500, Code: "network_setup_failed", Detail: "Sandbox network setup failed."},
	{Status: 500, Code: "vm_start_failed", Detail: "The sandbox virtual machine could not start."},
	{Status: 500, Code: "guest_readiness_failed", Detail: "The sandbox agent did not become ready."},
	{Status: 500, Code: "guest_identity_failed", Detail: "Sandbox guest identity initialization failed."},
	{Status: 500, Code: "create_publish_failed", Detail: "The sandbox could not be published as ready."},
}

// Capture the cause before cleanup can replace it with generic interruption.
// The registry keeps the request allocated until teardown commits its outcome.
func (s *Server) recordCreateFailure(ctx context.Context, id string, cause createFailure) {
	if ctx.Err() != nil {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.reg.RecordCreateFailure(recordCtx, id, createFailureCauses[cause]); err != nil {
		log.Printf("[%s] retain create failure %s: %v", id, createFailureCauses[cause].Code, err)
	}
}
