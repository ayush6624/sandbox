package server

import (
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

// A restored VM's guest size comes from the snapshot, but the jailer sizes the
// VM's cgroup from RunOptions — so a restore path that passes the bare template
// caps a larger guest's VMM at the template's memory and the host OOM-kills
// firecracker as soon as the guest touches more. That failure is invisible from
// inside the guest (it sees its full size and reports memory free) and reaches
// clients only as `502 agent unreachable`, so pin the mapping here.
func TestRestoreOptionsCarrySandboxResources(t *testing.T) {
	s := testServer(t)
	s.cfg.VMTemplate = vm.RunOptions{Vcpus: 2, MemMIB: 1024}

	for _, tc := range []struct {
		name               string
		sb                 registry.Sandbox
		wantVcpus, wantMem int64
	}{
		{
			name:      "zero means template default",
			sb:        registry.Sandbox{},
			wantVcpus: 2, wantMem: 1024,
		},
		{
			name:      "a bigger snapshot keeps its own size",
			sb:        registry.Sandbox{Vcpus: 4, MemMIB: 4096},
			wantVcpus: 4, wantMem: 4096,
		},
		{
			name:      "a smaller snapshot is not rounded up to the template",
			sb:        registry.Sandbox{Vcpus: 1, MemMIB: 512},
			wantVcpus: 1, wantMem: 512,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := s.restoreOptions(tc.sb)
			if opts.MemMIB != tc.wantMem {
				t.Errorf("MemMIB = %d, want %d: the jailer cgroup would be sized for the wrong guest",
					opts.MemMIB, tc.wantMem)
			}
			if opts.Vcpus != tc.wantVcpus {
				t.Errorf("Vcpus = %d, want %d", opts.Vcpus, tc.wantVcpus)
			}
			if opts.SocketPath != "" {
				t.Errorf("SocketPath = %q, want empty so each VM gets its own", opts.SocketPath)
			}
		})
	}
}
