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

	// UFFDChunkBytes selects the UFFD page source on the UFFD restore path
	// (RestoreUFFD): 0 = whole-file mmap (localSource, the default), >0 = read the
	// mem file in fixed chunks of this many bytes on demand (chunkedSource — the
	// indexing/cache a remote GCS source will reuse; roadmap Phase B1). Ignored
	// off the UFFD path, and superseded by UFFDChunks when that is set.
	UFFDChunkBytes uint64

	// UFFDChunks, when non-nil, serves the UFFD restore from this externally
	// supplied chunk source (the server's GCS-backed loader; roadmap Phase B2)
	// instead of the local mem file. Its Load is called on each chunk fault.
	UFFDChunks *UFFDChunkSource
}

// UFFDChunkSource describes a chunked UFFD page source whose chunk bytes come
// from an injected loader — the seam that lets the server back UFFD faults with
// GCS-resident chunks without the vm package importing gcsblob. Total/ChunkSize
// come from the chunk manifest; Prefetch is the chunk-level fault-ahead window;
// Load returns the decompressed bytes of chunk idx (nil past the image, error =
// unservable fault → the VM is killed).
type UFFDChunkSource struct {
	Total     uint64
	ChunkSize uint64
	Prefetch  uint64
	Load      func(idx uint64) ([]byte, error)
	// Prewarm is last wake's working set (chunk indices) to bulk-fetch in the
	// background as the guest resumes, so a cold wake doesn't fault-storm GCS one
	// chunk at a time (roadmap B3). Empty on the first wake (nothing recorded yet).
	Prewarm []uint64
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
