package vm

// RunOptions configures a microVM run.
type RunOptions struct {
	FirecrackerBin string
	SocketPath     string
	KernelImage    string
	RootfsPath     string
	KernelArgs     string
	Vcpus          int64
	MemMIB         int64
	LogDir         string

	// Networking (optional — if TapDevice is empty, no networking)
	TapDevice   string
	MacAddress  string
	GuestCIDR   string // e.g. "172.16.0.2/24"
	GatewayIP   string // e.g. "172.16.0.1"
	Nameservers string // e.g. "8.8.8.8"

	// Snapshot restore (optional). When both are set, NewMachineFromSnapshot
	// builds a machine that loads this snapshot and resumes instead of cold
	// booting. The network device (incl. tap name, MAC, guest IP) is restored
	// from the snapshot, so the host must recreate the tap under its original
	// name; TapDevice/MacAddress/GuestCIDR are ignored on the restore path.
	SnapshotMemPath   string
	SnapshotStatePath string
}

// RuntimeConfig captures identifiers after the SDK config is built.
type RuntimeConfig struct {
	SocketPath string
	VMID       string
}

// Snapshot types accepted by Snapshot(). Diff writes only the pages dirtied
// since the machine was loaded with dirty-page tracking enabled (clones —
// see StartClone) and is only valid on such machines; Full always works.
const (
	SnapshotFull = "Full"
	SnapshotDiff = "Diff"
)

// CloneParams drives a single identity-neutral clone restored from a snapshot
// (see StartClone). The rootfs is relocated to CloneRootfsPath, the host tap is
// remapped to TapDevice via the snapshot-load network_overrides, and the new
// network identity is pushed into MMDS so the in-guest thaw agent reconfigures
// eth0 to it. Gen must differ from the source's MMDS generation so the agent
// notices the change (we use the clone's sandbox id).
type CloneParams struct {
	MemPath         string
	StatePath       string
	CloneRootfsPath string
	TapDevice       string

	GuestIP    string // fresh IP from the pool
	MacAddress string // fresh MAC
	GatewayIP  string
	Prefix     int // guest CIDR prefix length, e.g. 24
	Gen        string
}
