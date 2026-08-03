package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusRunning = "running"
	// StatusStarting holds capacity while a user-visible sandbox is still
	// passing launch, agent-readiness, clock, and identity gates. It is not
	// routed or listed until MarkRunning commits.
	StatusStarting = "starting"
	// StatusStopping removes a sandbox from routing before bounded teardown,
	// while continuing to reserve its tap, IP, and memory.
	StatusStopping = "stopping"
	// StatusPreparing is a hidden warm-pool VM that has reserved capacity but
	// has not completed launch, guest re-identification, and identity rotation.
	// It must never be claimable.
	StatusPreparing = "preparing"
	// StatusWarming is a fully started, independently identified VM reserved
	// for a future create. It consumes capacity but is not routed or listed.
	StatusWarming = "warming"
	// StatusHibernated marks an idle sandbox frozen to disk: its VM is gone and
	// its tap/IP are released back to the pools (their partial unique indexes
	// bind capacity-holding running/warming rows). The identity columns are
	// CLEARED and the pair is remembered in last_tap/last_ip — see Hibernate.
	// Explicit port mappings stay reserved and
	// their userspace listeners remain bound so a connection can wake it. The
	// row, rootfs file, and hibernation snapshot survive server restarts.
	StatusHibernated = "hibernated"
)

// Sandbox represents a row in the sandboxes table.
type Sandbox struct {
	ID string `json:"id"`
	// Name is a free-form display label, settable at create time and via
	// POST /sandboxes/{id}/rename. Not unique, not a lookup key; "" = unnamed.
	Name       string            `json:"name,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	SourceType string            `json:"source_type,omitempty"`
	SourceID   string            `json:"source_id,omitempty"`
	PID        int               `json:"pid"`
	VMID       string            `json:"vm_id"`
	SocketPath string            `json:"socket_path"`
	TapDevice  string            `json:"tap_device"`
	GuestIP    string            `json:"guest_ip"`
	// LastTap/LastIP are the tap and guest IP a hibernated sandbox's frozen
	// memory image has baked in. A hibernated row's LIVE identity columns are
	// empty (the frozen VM holds neither), so these are what the pool pickers
	// soft-avoid and what Wake tries to reclaim for a cheap same-identity
	// restore. Internal placement state, never serialized: an API client sees
	// the live identity, which for a frozen sandbox is legitimately absent.
	LastTap    string     `json:"-"`
	LastIP     string     `json:"-"`
	RootfsPath string     `json:"rootfs_path"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
	StoppedAt  *time.Time `json:"stopped_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"` // nil = no auto-destroy
	// HibernateAfterSec overrides the host's idle-hibernation window for this
	// sandbox: >0 = seconds of idleness before freezing, -1 = never hibernate,
	// 0 = inherit the host config.
	HibernateAfterSec int `json:"hibernate_after_sec,omitempty"`
	// Vcpus/MemMIB are per-sandbox resource overrides recorded at create time;
	// 0 = the host's template default. An override forces a cold boot (the
	// golden snapshot bakes the template's resources). API responses never
	// carry 0: the server fills in the template default before writing
	// (effectiveResources in internal/server), so clients always see the
	// resources the sandbox actually runs with.
	Vcpus  int64 `json:"vcpus"`
	MemMIB int64 `json:"mem_mib"`
	// BaseSnapshotID is the golden snapshot this sandbox was cloned from
	// (hot create). It makes the sandbox diff-snapshottable: a snapshot of it
	// can be stored as a delta against that base. Empty for cold boots,
	// restores, and user fan-out clones.
	BaseSnapshotID string `json:"base_snapshot_id,omitempty"`
	// HostAddr is set by the GATEWAY only (never stored): the owning host's
	// address, so clients reach forwarded ports on the host that holds the
	// port-forward listeners rather than on the gateway.
	HostAddr string `json:"host_addr,omitempty"`
	// Ports is populated by API handlers (never stored) so GET can advertise
	// URL-only ingress without forcing a second round trip.
	Ports []PortMapping `json:"ports,omitempty"`
}

// PortMapping is one explicitly exposed guest port. HostPort is zero for a
// URL-only exposure, which deliberately consumes no worker host-port slot.
type PortMapping struct {
	GuestPort  int    `json:"guest_port"`
	HostPort   int    `json:"host_port,omitempty"`
	PublicHost string `json:"public_host,omitempty"`
	PublicPort int    `json:"public_port,omitempty"`
	Mode       string `json:"mode"`
	URL        string `json:"url,omitempty"`
}

type RawPortMapping struct {
	GuestPort  int    `json:"guest_port"`
	Mode       string `json:"mode"`
	PublicHost string `json:"public_host"`
	PublicPort int    `json:"public_port"`
}

// Snapshot is a saved point-in-time image of a sandbox (Firecracker memory +
// device state plus a frozen rootfs copy) that a new sandbox can be restored
// from. TapDevice and GuestIP are recorded because the snapshot bakes them in:
// a restore must recreate the same tap and reuse the same guest IP.
type Snapshot struct {
	ID string `json:"id"`
	// Name is a free-form display label, settable at snapshot time and via
	// POST /snapshots/{id}/rename. Not unique, not a lookup key; "" = unnamed.
	Name     string `json:"name,omitempty"`
	SourceID string `json:"source_id"`
	// TapDevice and GuestIP are reused on restore (baked into the snapshot).
	TapDevice string `json:"tap_device"`
	GuestIP   string `json:"guest_ip"`
	// GuestMAC is the NIC identity baked into the snapshot. New snapshots
	// record it so a same-identity restore can prime the recreated bridge path
	// without waiting for ARP discovery. Empty on legacy snapshots.
	GuestMAC  string `json:"guest_mac,omitempty"`
	MemPath   string `json:"mem_path"`
	StatePath string `json:"state_path"`
	// RootfsPath is the frozen rootfs copy this snapshot restores FROM.
	RootfsPath string `json:"rootfs_path"`
	// SourceRootfsPath is the disk path baked into the Firecracker snapshot —
	// a restore must place its rootfs copy here, or Firecracker can't reattach
	// the block device.
	SourceRootfsPath string     `json:"source_rootfs_path"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	// Durability is "local" until the immutable artifacts and commit marker
	// are present in the configured object store, then "durable".
	Durability string `json:"durability,omitempty"`
	// Golden marks the server-managed pristine snapshot that POST /sandboxes
	// clones from. At most one snapshot is golden (partial unique index).
	Golden bool `json:"golden,omitempty"`
	// BaseMtime/BaseSize record the base rootfs stat at snapshot time, so a
	// rebuilt base image (e.g. after install-agent) invalidates a golden
	// snapshot on the next server startup.
	BaseMtime int64 `json:"-"`
	BaseSize  int64 `json:"-"`
	// Format is how the artifacts are stored: "full" (self-contained mem +
	// rootfs) or "diff" (mem = dirty pages since the base snapshot, rootfs =
	// changed extents vs the base's rootfs; both require the base to
	// materialize). Empty on pre-migration rows — treat as "full".
	Format string `json:"format,omitempty"`
	// BaseID is the snapshot this diff is relative to (the golden snapshot
	// the source sandbox was cloned from). Empty for format=full.
	BaseID string `json:"base_id,omitempty"`
	// Vcpus/MemMIB record the source sandbox's resource overrides (0 =
	// template default). Firecracker bakes vcpus/mem into the snapshot, so
	// restores and fan-out clones inherit them — these fields let their rows
	// report the truth.
	Vcpus  int64 `json:"vcpus,omitempty"`
	MemMIB int64 `json:"mem_mib,omitempty"`
}

// Snapshot formats.
const (
	FormatFull = "full"
	FormatDiff = "diff"
)

// Pools defines the resource ranges from which sandboxes draw on creation.
type Pools struct {
	TapPrefix  string // e.g. "fc"
	TapMax     int    // total slots; tap names = TapPrefix + "0..TapMax-1"
	GuestIPMin string // e.g. "172.16.0.10"
	GuestIPMax string // e.g. "172.16.0.73"
	PortMin    int    // host port range start, e.g. 5200
	PortMax    int    // host port range end (inclusive), e.g. 5263
}

// Slots returns the host's effective sandbox capacity: the smaller of the tap
// and guest-IP pools. Ports are allocated only when explicitly exposed and do
// not constrain sandbox creation.
func (p Pools) Slots() int {
	n := p.TapMax
	if c := p.ipPoolSize(); c < n {
		n = c
	}
	if n < 0 {
		n = 0
	}
	return n
}

// ipPoolSize is the number of guest IPs in the pool, or TapMax when the range
// is unparsable (so a bad config degrades to tap-bound rather than zero).
func (p Pools) ipPoolSize() int {
	minIP, err := ipToUint32(p.GuestIPMin)
	if err != nil {
		return p.TapMax
	}
	maxIP, err := ipToUint32(p.GuestIPMax)
	if err != nil {
		return p.TapMax
	}
	return int(maxIP-minIP) + 1
}

// FreeSlots returns how many new sandboxes Create could allocate right now:
// the smaller of tap/IP availability, further bounded by the memory budget
// when one is configured. Explicit port mappings do not constrain creation:
// a sandbox can be created even when no additional host port can be exposed.
func (r *Registry) FreeSlots(ctx context.Context) (int, error) {
	var running int
	var committedMem int64
	err := r.rdb.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sandboxes WHERE status IN (?1, ?2, ?5, ?6, ?7)),
			(SELECT COALESCE(SUM(CASE WHEN mem_mib = 0 THEN ?3 ELSE mem_mib END + ?4), 0)
			   FROM sandboxes WHERE status IN (?1, ?2, ?5, ?6, ?7))`,
		StatusRunning, StatusWarming, r.mem.TemplateMemMIB, r.mem.OverheadMIB,
		StatusPreparing, StatusStarting, StatusStopping,
	).Scan(&running, &committedMem)
	if err != nil {
		return 0, err
	}
	return r.freeSlotsFor(running, committedMem), nil
}

func (r *Registry) freeSlotsFor(running int, committedMem int64) int {
	free := r.pools.TapMax - running
	if f := r.pools.ipPoolSize() - running; f < free {
		free = f
	}
	if per := r.mem.TemplateMemMIB + r.mem.OverheadMIB; r.mem.BudgetMIB > 0 && per > 0 {
		if f := int((r.mem.BudgetMIB - committedMem) / per); f < free {
			free = f
		}
	}
	if free < 0 {
		free = 0
	}
	return free
}

// Stats is a point-in-time snapshot of a host's occupancy, for the /metrics
// endpoint. It mirrors the counts FreeSlots derives capacity from, but exposes
// the raw numbers (per-pool used/total, committed memory) so an operator can
// see WHICH pool is the binding constraint, not just the free-slot minimum.
type Stats struct {
	Running    int // status=running: holds a tap, IP, and guest memory
	Starting   int // status=starting: user VM still completing readiness
	Stopping   int // status=stopping: teardown in progress, no longer routed
	Warming    int // status=warming: hidden ready VM that also holds capacity
	Preparing  int // status=preparing: hidden VM still completing readiness
	Hibernated int // status=hibernated: holds no VM slot or memory
	SlotsFree  int // == FreeSlots: smallest per-pool availability, mem-bounded

	// Pool occupancy. Taps/IPs are held only by running sandboxes. Ports are
	// held only by explicit mappings, including while a sandbox is hibernated.
	TapUsed, TapTotal   int
	IPUsed, IPTotal     int
	PortUsed, PortTotal int

	CommittedMemMIB int64 // sum of running sandboxes' effective mem_mib + overhead
	MemBudgetMIB    int64 // admission ceiling; 0 = disabled
}

// Stats returns the host occupancy snapshot in a single query. It is cheap
// (four COUNT/SUMs over the small sandboxes tables) and read-only, safe to hit
// on every Prometheus scrape.
func (r *Registry) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := r.rdb.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?1),
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?2),
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?5),
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?6),
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?7),
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?8),
			(SELECT COUNT(*) FROM sandbox_ports WHERE host_port IS NOT NULL),
			(SELECT COALESCE(SUM(CASE WHEN mem_mib = 0 THEN ?3 ELSE mem_mib END + ?4), 0)
			   FROM sandboxes WHERE status IN (?1, ?5, ?6, ?7, ?8))`,
		StatusRunning, StatusHibernated, r.mem.TemplateMemMIB, r.mem.OverheadMIB,
		StatusWarming, StatusPreparing, StatusStarting, StatusStopping,
	).Scan(&s.Running, &s.Hibernated, &s.Warming, &s.Preparing, &s.Starting, &s.Stopping, &s.PortUsed, &s.CommittedMemMIB)
	if err != nil {
		return Stats{}, err
	}
	// Derive free capacity from the same aggregate query. Calling FreeSlots
	// here would take a second SQLite snapshot and can pair an older Running
	// count with newer capacity while deletes are committing.
	occupied := s.Running + s.Starting + s.Stopping + s.Warming + s.Preparing
	s.SlotsFree = r.freeSlotsFor(occupied, s.CommittedMemMIB)
	s.TapUsed, s.TapTotal = occupied, r.pools.TapMax
	s.IPUsed, s.IPTotal = occupied, r.pools.ipPoolSize()
	s.PortTotal = r.pools.PortMax - r.pools.PortMin + 1
	s.MemBudgetMIB = r.mem.BudgetMIB
	return s, nil
}

// MemAccounting configures memory-aware admission: the sum of committed guest
// memory (each running sandbox's effective mem_mib + OverheadMIB of VMM
// overhead) must stay within BudgetMIB. Hibernated sandboxes hold no memory —
// their VM is dead. BudgetMIB <= 0 disables admission and the FreeSlots
// memory bound entirely (tests, hosts with no configured limit).
type MemAccounting struct {
	TemplateMemMIB int64 // resolves rows/requests with mem_mib=0 (template default)
	BudgetMIB      int64 // committed-memory ceiling; <=0 = disabled
	OverheadMIB    int64 // per-VM firecracker/VMM overhead charged on top of guest mem
}

// Registry wraps the SQLite-backed sandbox state.
//
// Two handles onto the same database file, and WHICH ONE A METHOD USES IS PART
// OF ITS CONTRACT — see the rule documented on Open:
//
//	db  — every write and every transaction. Single connection (see Open).
//	rdb — pure reads only. Read-only at the driver level, N connections.
type Registry struct {
	db    *sql.DB
	rdb   *sql.DB
	pools Pools
	mem   MemAccounting

	// portReads counts port-mapping reads. It exists to keep one invariant
	// testable: sampling the host's public routes must cost ONE query
	// (PublicRoutes), never one Ports() per sandbox — that loop made the
	// heartbeat's cost grow with inventory. Two atomic adds on a path that
	// already does SQLite work.
	portReads struct {
		publicRoutes atomic.Int64
		ports        atomic.Int64
	}
}

// PortReadCounts reports how many PublicRoutes and per-sandbox Ports queries
// this registry has served. See Registry.portReads.
func (r *Registry) PortReadCounts() (publicRoutes, ports int64) {
	return r.portReads.publicRoutes.Load(), r.portReads.ports.Load()
}

// SetMemAccounting installs the memory-admission parameters. Call once before
// serving; the zero value leaves admission disabled.
func (r *Registry) SetMemAccounting(a MemAccounting) { r.mem = a }

// Open initializes the database (creating it if needed) and applies migrations.
func Open(dbPath string, pools Pools) (*Registry, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir db parent: %w", err)
	}
	dsn := dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Serialize on a single connection. Create() runs SELECTs then an INSERT in
	// one transaction; with multiple connections, concurrent creates (e.g. a
	// burst of POST /sandboxes placed on the same host) deadlock on the
	// write-lock upgrade and fail with SQLITE_BUSY — busy_timeout can't resolve a
	// lock-upgrade conflict. One connection makes registry ops queue instead.
	// Creates are bottlenecked on rootfs copy + VM boot, so making them queue
	// behind each other here is free; cross-host parallelism is unaffected
	// (each host has its own DB). What is NOT free is putting the data plane
	// behind the same connection — see the read-only handle below.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	r := &Registry{db: db, pools: pools}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}

	// Second handle, READ-ONLY, with real concurrency. The data plane reads the
	// registry on every unit of traffic: each accepted forwarded-port
	// connection and each CONNECT tunnel resolves the sandbox row, every
	// heartbeat samples capacity, every metrics scrape runs an aggregate. On
	// one connection all of that queues behind Create/Wake transactions (two
	// full pool scans plus an insert), so traffic slowed down CREATES and the
	// gateway saw capacity pushback caused by load rather than by capacity.
	//
	// THE RULE for routing a method here — undo it and the races the single
	// connection was protecting come back:
	//
	//  1. Only statements that run OUTSIDE a transaction. A read issued from
	//     inside a write TX must see that TX's own uncommitted rows, which a
	//     different connection cannot; it would also deadlock the single
	//     writer against itself.
	//  2. Only reads whose result is not the basis of a resource allocation or
	//     admission decision. Those reads (loadUsed's tap/IP/port scans,
	//     checkMemBudget) must stay INSIDE the allocating TX on this handle,
	//     which is where they already are. They are correct because of the
	//     transaction, never because of the connection count.
	//
	// What the split does NOT weaken: the single connection never gave a
	// read-then-write PAIR of separate calls any atomicity (another goroutine
	// could always interleave between them), and in WAL mode a fresh read sees
	// every commit that completed before it started. So a pure read is exactly
	// as fresh here as it was on the writer.
	//
	// Read-only is enforced by SQLite (mode=ro), not by convention: a routing
	// mistake fails loudly with "attempt to write a readonly database" instead
	// of silently reintroducing multi-writer lock upgrades — the exact
	// SQLITE_BUSY failure the single connection exists to prevent. Opened after
	// migrate() so the WAL and shm files exist (a read-only connection can
	// create neither).
	rdsn := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"}
	rdb, err := sql.Open("sqlite", rdsn.String())
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(readerConns())
	rdb.SetMaxIdleConns(readerConns())
	rdb.SetConnMaxIdleTime(5 * time.Minute)
	if err := rdb.Ping(); err != nil {
		_ = rdb.Close()
		_ = db.Close()
		return nil, err
	}
	r.rdb = rdb
	return r, nil
}

// readerConns sizes the read-only pool. Readers are short CPU-bound SQLite
// queries against a page cache, so scaling with cores is the right shape;
// clamped low because each connection costs a page cache and a host running
// hundreds of sandboxes has better uses for its RAM.
func readerConns() int {
	n := runtime.NumCPU()
	if n < 4 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return n
}

// Close releases both database handles.
func (r *Registry) Close() error {
	var firstErr error
	if r.rdb != nil {
		firstErr = r.rdb.Close()
	}
	if err := r.db.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// Pools returns the configured pools.
func (r *Registry) Pools() Pools { return r.pools }

func (r *Registry) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS sandboxes (
		id          TEXT PRIMARY KEY,
		pid         INTEGER NOT NULL,
		vm_id       TEXT NOT NULL,
		socket_path TEXT NOT NULL,
		tap_device  TEXT NOT NULL,
		guest_ip    TEXT NOT NULL,
		rootfs_path TEXT NOT NULL,
		status      TEXT NOT NULL,
		created_at  INTEGER NOT NULL,
		stopped_at  INTEGER,
		expires_at  INTEGER,
		base_snapshot_id TEXT NOT NULL DEFAULT '',
		hibernate_after_sec INTEGER NOT NULL DEFAULT 0,
		vcpus       INTEGER NOT NULL DEFAULT 0,
		mem_mib     INTEGER NOT NULL DEFAULT 0,
		name        TEXT NOT NULL DEFAULT ''
		, metadata  TEXT NOT NULL DEFAULT '{}'
		, source_type TEXT NOT NULL DEFAULT 'default'
		, source_id TEXT NOT NULL DEFAULT ''
		, last_tap  TEXT NOT NULL DEFAULT ''
		, last_ip   TEXT NOT NULL DEFAULT ''
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_tap_running  ON sandboxes(tap_device) WHERE status IN ('running', 'starting', 'stopping', 'preparing', 'warming') AND tap_device <> '';
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_ip_running   ON sandboxes(guest_ip)   WHERE status IN ('running', 'starting', 'stopping', 'preparing', 'warming') AND guest_ip <> '';
	CREATE TABLE IF NOT EXISTS sandbox_ports (
		sandbox_id TEXT NOT NULL,
		guest_port INTEGER NOT NULL,
		host_port  INTEGER,
		public_port INTEGER,
		PRIMARY KEY (sandbox_id, guest_port)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_host_port ON sandbox_ports(host_port) WHERE host_port IS NOT NULL;
	CREATE TABLE IF NOT EXISTS snapshots (
		id                 TEXT PRIMARY KEY,
		source_id          TEXT NOT NULL,
		tap_device         TEXT NOT NULL,
		guest_ip           TEXT NOT NULL,
		guest_mac          TEXT NOT NULL DEFAULT '',
		mem_path           TEXT NOT NULL,
		state_path         TEXT NOT NULL,
		rootfs_path        TEXT NOT NULL,
		source_rootfs_path TEXT NOT NULL DEFAULT '',
		created_at         INTEGER NOT NULL,
		golden             INTEGER NOT NULL DEFAULT 0,
		base_mtime         INTEGER NOT NULL DEFAULT 0,
		base_size          INTEGER NOT NULL DEFAULT 0,
		format             TEXT NOT NULL DEFAULT 'full',
		base_id            TEXT NOT NULL DEFAULT '',
		vcpus              INTEGER NOT NULL DEFAULT 0,
		mem_mib            INTEGER NOT NULL DEFAULT 0,
		name               TEXT NOT NULL DEFAULT ''
		, expires_at        INTEGER
		, durability       TEXT NOT NULL DEFAULT 'local'
	);
	`
	if _, err := r.db.Exec(schema); err != nil {
		return err
	}
	// Warm-pool VMs hold real taps/IPs just like routed VMs. Rebuild the old
	// running-only partial indexes on upgraded registries. The `<> ''` terms
	// exempt rows that hold NO identity: a row can legitimately be
	// identity-less while it holds capacity-bearing status (a hibernated row
	// moving to 'stopping' for teardown), and two such rows must not collide
	// with each other on the empty string.
	if _, err := r.db.Exec(`
		DROP INDEX IF EXISTS uniq_tap_running;
		DROP INDEX IF EXISTS uniq_ip_running;
		CREATE UNIQUE INDEX uniq_tap_running ON sandboxes(tap_device) WHERE status IN ('running', 'starting', 'stopping', 'preparing', 'warming') AND tap_device <> '';
		CREATE UNIQUE INDEX uniq_ip_running ON sandboxes(guest_ip) WHERE status IN ('running', 'starting', 'stopping', 'preparing', 'warming') AND guest_ip <> '';
	`); err != nil {
		return err
	}
	if err := r.makeHostPortsNullable(); err != nil {
		return err
	}
	if _, err := r.db.Exec(`ALTER TABLE sandbox_ports ADD COLUMN public_port INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	if _, err := r.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uniq_public_port ON sandbox_ports(public_port) WHERE public_port IS NOT NULL`); err != nil {
		return err
	}
	// source_rootfs_path was added after the snapshots table first shipped.
	if _, err := r.db.Exec(`ALTER TABLE snapshots ADD COLUMN source_rootfs_path TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	if _, err := r.db.Exec(`ALTER TABLE snapshots ADD COLUMN guest_mac TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	// expires_at was added after v1 databases shipped. ALTER TABLE has no
	// IF NOT EXISTS, so ignore the duplicate-column error on migrated DBs.
	if _, err := r.db.Exec(`ALTER TABLE sandboxes ADD COLUMN expires_at INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	// golden/base_mtime/base_size were added with hot create.
	for _, col := range []string{
		`ALTER TABLE snapshots ADD COLUMN golden INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE snapshots ADD COLUMN base_mtime INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE snapshots ADD COLUMN base_size INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// After the ALTERs so it can be created on pre-golden databases too.
	if _, err := r.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uniq_golden_snapshot ON snapshots(golden) WHERE golden = 1`); err != nil {
		return err
	}
	// format/base_id (snapshots) and base_snapshot_id (sandboxes) were added
	// with diff-based GCS snapshot durability.
	for _, col := range []string{
		`ALTER TABLE snapshots ADD COLUMN format TEXT NOT NULL DEFAULT 'full'`,
		`ALTER TABLE snapshots ADD COLUMN base_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sandboxes ADD COLUMN base_snapshot_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// hibernate_after_sec was added with the per-sandbox hibernation override.
	if _, err := r.db.Exec(`ALTER TABLE sandboxes ADD COLUMN hibernate_after_sec INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	// Primary ports used to live on sandboxes.host_port. Port forwarding is now
	// exclusively opt-in through sandbox_ports. Drop the old indexes and column
	// when upgrading an existing registry.
	if _, err := r.db.Exec(`DROP INDEX IF EXISTS uniq_port_running`); err != nil {
		return err
	}
	if _, err := r.db.Exec(`DROP INDEX IF EXISTS uniq_port_held`); err != nil {
		return err
	}
	if _, err := r.db.Exec(`DROP INDEX IF EXISTS uniq_extra_host_port`); err != nil {
		return err
	}
	if err := r.dropLegacyHostPortColumn(); err != nil {
		return err
	}
	// vcpus/mem_mib were added with per-sandbox resource overrides (0 =
	// template default; snapshots record the source's values).
	for _, col := range []string{
		`ALTER TABLE sandboxes ADD COLUMN vcpus INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE sandboxes ADD COLUMN mem_mib INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE snapshots ADD COLUMN vcpus INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE snapshots ADD COLUMN mem_mib INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// name was added with sandbox/snapshot display names.
	for _, col := range []string{
		`ALTER TABLE sandboxes ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE snapshots ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE snapshots ADD COLUMN expires_at INTEGER`,
		`ALTER TABLE snapshots ADD COLUMN durability TEXT NOT NULL DEFAULT 'local'`,
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	for _, col := range []string{
		`ALTER TABLE sandboxes ADD COLUMN metadata TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE sandboxes ADD COLUMN source_type TEXT NOT NULL DEFAULT 'default'`,
		`ALTER TABLE sandboxes ADD COLUMN source_id TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// last_tap/last_ip split a hibernated sandbox's REMEMBERED identity from the
	// live tap_device/guest_ip the partial unique indexes bind. Hibernated rows
	// used to keep their identity populated, which is a lie (the frozen VM holds
	// neither) with a sharp edge: once occupancy forced the pickers' second pass
	// to hand that tap/IP to a running sandbox, MarkStopping — the first thing
	// destroy does — moved the hibernated row into the index set and failed with
	// UNIQUE constraint failed, permanently. The sandbox became undeletable and
	// the TTL reaper retried it every 10 s forever.
	for _, col := range []string{
		`ALTER TABLE sandboxes ADD COLUMN last_tap TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sandboxes ADD COLUMN last_ip TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	// Normalize rows an older binary froze with their identity still attached:
	// move it into last_tap/last_ip so the pickers keep soft-avoiding it (cheap
	// same-identity wakes) while the row itself holds nothing the indexes bind.
	// Runs on every open — cheap, idempotent, and it repairs rows left behind by
	// a downgrade to a binary that predates this column pair.
	if _, err := r.db.Exec(`
		UPDATE sandboxes SET last_tap = tap_device, last_ip = guest_ip, tap_device = '', guest_ip = ''
		 WHERE status = 'hibernated' AND (tap_device <> '' OR guest_ip <> '')`); err != nil {
		return err
	}
	// The billable-usage ledger is a separate table on purpose: Destroy deletes
	// the sandboxes row, so usage kept there would vanish with every sandbox.
	if err := r.migrateUsage(); err != nil {
		return fmt.Errorf("migrate usage ledger: %w", err)
	}
	return nil
}

// makeHostPortsNullable upgrades the pre-ingress sandbox_ports table. SQLite
// cannot drop a NOT NULL constraint in place, so preserve the rows while
// rebuilding the small mapping table. The partial unique index keeps the old
// host-port uniqueness guarantee without making NULL (URL-only) rows collide.
func (r *Registry) makeHostPortsNullable() error {
	rows, err := r.db.Query(`PRAGMA table_info(sandbox_ports)`)
	if err != nil {
		return err
	}
	hostPortNotNull := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "host_port" {
			hostPortNotNull = notNull != 0
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !hostPortNotNull {
		_, err := r.db.Exec(`DROP INDEX IF EXISTS uniq_host_port;
			CREATE UNIQUE INDEX IF NOT EXISTS uniq_host_port ON sandbox_ports(host_port) WHERE host_port IS NOT NULL`)
		return err
	}

	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DROP INDEX IF EXISTS uniq_host_port`,
		`ALTER TABLE sandbox_ports RENAME TO sandbox_ports_old`,
		`CREATE TABLE sandbox_ports (
			sandbox_id TEXT NOT NULL,
			guest_port INTEGER NOT NULL,
			host_port INTEGER,
			public_port INTEGER,
			PRIMARY KEY (sandbox_id, guest_port)
		)`,
		`INSERT INTO sandbox_ports (sandbox_id, guest_port, host_port)
			SELECT sandbox_id, guest_port, host_port FROM sandbox_ports_old`,
		`DROP TABLE sandbox_ports_old`,
		`CREATE UNIQUE INDEX uniq_host_port ON sandbox_ports(host_port) WHERE host_port IS NOT NULL`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Registry) dropLegacyHostPortColumn() error {
	rows, err := r.db.Query(`PRAGMA table_info(sandboxes)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == "host_port" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = r.db.Exec(`ALTER TABLE sandboxes DROP COLUMN host_port`)
	return err
}

// Create allocates a tap/IP from the pools and inserts a running row. It is
// retained for registry callers that create already-ready resources; server
// bring-up paths use CreateStarting below.
// auto-destroy by the server's reaper. baseSnapshotID records the golden
// snapshot the sandbox is cloned from ("" for cold boots and user fan-outs) —
// it makes the sandbox diff-snapshottable. hibernateAfterSec is the
// per-sandbox idle-hibernation override (>0 seconds, -1 never, 0 host default).
// vcpus/memMIB are per-sandbox resource overrides (0 = template default).
// name is the free-form display label ("" = unnamed).
func (r *Registry) Create(ctx context.Context, id, name, rootfsPath string, expiresAt *time.Time, baseSnapshotID string, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
	return r.create(ctx, StatusRunning, id, name, rootfsPath, expiresAt, baseSnapshotID, hibernateAfterSec, vcpus, memMIB)
}

// CreateStarting reserves capacity without publishing a user sandbox. The
// server calls MarkRunning only after all readiness gates complete.
func (r *Registry) CreateStarting(ctx context.Context, id, name, rootfsPath string, expiresAt *time.Time, baseSnapshotID string, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
	return r.create(ctx, StatusStarting, id, name, rootfsPath, expiresAt, baseSnapshotID, hibernateAfterSec, vcpus, memMIB)
}

// CreateWarm allocates a hidden, capacity-holding row for a pre-started VM.
func (r *Registry) CreateWarm(ctx context.Context, id, rootfsPath, baseSnapshotID string, vcpus, memMIB int64) (Sandbox, error) {
	return r.create(ctx, StatusPreparing, id, "", rootfsPath, nil, baseSnapshotID, -1, vcpus, memMIB)
}

func (r *Registry) create(ctx context.Context, status, id, name, rootfsPath string, expiresAt *time.Time, baseSnapshotID string, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback()

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return Sandbox{}, err
	}
	tap, err := pickFreeTap(used, r.pools)
	if err != nil {
		return Sandbox{}, err
	}
	ip, err := pickFreeIP(used, r.pools)
	if err != nil {
		return Sandbox{}, err
	}
	if err := r.checkMemBudget(ctx, tx, memMIB); err != nil {
		return Sandbox{}, err
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sandboxes (id, name, pid, vm_id, socket_path, tap_device, guest_ip, rootfs_path, status, created_at, expires_at, base_snapshot_id, hibernate_after_sec, vcpus, mem_mib)
		 VALUES (?, ?, 0, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, tap, ip, rootfsPath, status, now.Unix(), unixOrNil(expiresAt), baseSnapshotID, hibernateAfterSec, vcpus, memMIB)
	if err != nil {
		return Sandbox{}, fmt.Errorf("insert sandbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, err
	}
	return Sandbox{
		ID:                id,
		Name:              name,
		TapDevice:         tap,
		GuestIP:           ip,
		RootfsPath:        rootfsPath,
		Status:            status,
		CreatedAt:         now,
		ExpiresAt:         expiresAt,
		BaseSnapshotID:    baseSnapshotID,
		HibernateAfterSec: hibernateAfterSec,
		Vcpus:             vcpus,
		MemMIB:            memMIB,
	}, nil
}

// CreateRestore inserts a running row for an already-ready restored sandbox.
// Unlike Create, the tap and guest IP are fixed (the snapshot baked them in) —
// The partial unique indexes guarantee the tap/IP aren't already taken by a
// running sandbox, so a restore
// fails cleanly if the source (or a prior restore of the same snapshot) is
// still live. vcpus/memMIB carry the snapshot's recorded resources — the
// restore can't change them (they're baked into the snapshot), it just
// reports them.
func (r *Registry) CreateRestore(ctx context.Context, id, name, rootfsPath, tap, ip string, expiresAt *time.Time, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
	return r.createRestore(ctx, StatusRunning, id, name, rootfsPath, tap, ip, expiresAt, hibernateAfterSec, vcpus, memMIB)
}

// CreateRestoreStarting reserves a snapshot's fixed identity without routing
// it until restore readiness completes.
func (r *Registry) CreateRestoreStarting(ctx context.Context, id, name, rootfsPath, tap, ip string, expiresAt *time.Time, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
	return r.createRestore(ctx, StatusStarting, id, name, rootfsPath, tap, ip, expiresAt, hibernateAfterSec, vcpus, memMIB)
}

func (r *Registry) createRestore(ctx context.Context, status, id, name, rootfsPath, tap, ip string, expiresAt *time.Time, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback()

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return Sandbox{}, err
	}
	if used.taps[tap] {
		return Sandbox{}, fmt.Errorf("tap %s in use (source sandbox still running?)", tap)
	}
	if used.ips[ip] {
		return Sandbox{}, fmt.Errorf("guest IP %s in use (source sandbox still running?)", ip)
	}
	if err := r.checkMemBudget(ctx, tx, memMIB); err != nil {
		return Sandbox{}, err
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sandboxes (id, name, pid, vm_id, socket_path, tap_device, guest_ip, rootfs_path, status, created_at, expires_at, hibernate_after_sec, vcpus, mem_mib)
		 VALUES (?, ?, 0, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, tap, ip, rootfsPath, status, now.Unix(), unixOrNil(expiresAt), hibernateAfterSec, vcpus, memMIB)
	if err != nil {
		return Sandbox{}, fmt.Errorf("insert restored sandbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, err
	}
	return Sandbox{
		ID:                id,
		Name:              name,
		TapDevice:         tap,
		GuestIP:           ip,
		RootfsPath:        rootfsPath,
		Status:            status,
		CreatedAt:         now,
		ExpiresAt:         expiresAt,
		HibernateAfterSec: hibernateAfterSec,
		Vcpus:             vcpus,
		MemMIB:            memMIB,
	}, nil
}

// FinishStart records runtime details after firecracker is up.
func (r *Registry) FinishStart(ctx context.Context, id string, pid int, vmID, socketPath string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET pid=?, vm_id=?, socket_path=? WHERE id=?`,
		pid, vmID, socketPath, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %s not found", id)
	}
	return nil
}

// MarkRunning publishes a user sandbox only after its VM and agent are fully
// ready. Keeping this separate from FinishStart prevents list and heartbeat
// readers from observing a half-initialized endpoint.
func (r *Registry) MarkRunning(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=? WHERE id=? AND status=?`,
		StatusRunning, id, StatusStarting)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("sandbox %s is not starting", id)
	}
	return nil
}

// MarkStopping removes a capacity-holding sandbox from routing before teardown.
// Hibernated rows transition too (destroy must un-route them before it starts
// deleting artifacts); that is safe only because a hibernated row holds no live
// tap/IP — see Hibernate. Re-populating those columns while hibernated would
// make this statement collide with uniq_tap_running/uniq_ip_running against
// whichever running sandbox has since been handed that identity.
func (r *Registry) MarkStopping(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=? WHERE id=? AND status IN (?, ?, ?, ?, ?)`,
		StatusStopping, id, StatusRunning, StatusStarting, StatusPreparing, StatusWarming, StatusHibernated)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	var status string
	if err := r.db.QueryRowContext(ctx, `SELECT status FROM sandboxes WHERE id=?`, id).Scan(&status); err != nil {
		return err
	}
	if status == StatusStopping {
		return nil
	}
	return fmt.Errorf("sandbox %s cannot stop from %s", id, status)
}

// SetName updates a sandbox's display name; "" clears it.
func (r *Registry) SetName(ctx context.Context, id, name string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE sandboxes SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %s not found", id)
	}
	return nil
}

// SetSnapshotName updates a snapshot's display name; "" clears it.
func (r *Registry) SetSnapshotName(ctx context.Context, id, name string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE snapshots SET name=? WHERE id=?`, name, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("snapshot %s not found", id)
	}
	return nil
}

// SetExpiry updates a sandbox's auto-destroy deadline; nil clears it.
func (r *Registry) SetExpiry(ctx context.Context, id string, t *time.Time) error {
	res, err := r.db.ExecContext(ctx, `UPDATE sandboxes SET expires_at=? WHERE id=?`, unixOrNil(t), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %s not found", id)
	}
	return nil
}

// Expired returns running, hibernated, or partially-stopped sandboxes whose
// expires_at has passed. Including stopping makes transient teardown failures
// retryable by the next reaper pass.
func (r *Registry) Expired(ctx context.Context, now time.Time) ([]Sandbox, error) {
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		 WHERE status IN (?, ?, ?) AND expires_at IS NOT NULL AND expires_at < ?`,
		StatusRunning, StatusHibernated, StatusStopping, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

// Hibernate marks a running sandbox as hibernated. The caller has already
// frozen the VM and released its host-side resources, so the row must stop
// claiming an identity it no longer holds: tap_device/guest_ip MOVE to
// last_tap/last_ip. That is what lets a new sandbox take the pair (Wake handles
// that with a fresh identity + the reidentifying clone path) while the pickers
// still soft-avoid it, and it is what keeps a later status transition —
// MarkStopping, on destroy — from colliding with the partial unique indexes
// against the sandbox that took the identity over. Explicit port mappings and
// their wake-on-connect listeners are stored separately and remain intact.
func (r *Registry) Hibernate(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=?, stopped_at=?, pid=0, vm_id='', socket_path='',
		    last_tap=tap_device, last_ip=guest_ip, tap_device='', guest_ip=''
		 WHERE id=? AND status=?`,
		StatusHibernated, time.Now().Unix(), id, StatusRunning)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %s not running", id)
	}
	return nil
}

// RollbackWake returns a sandbox whose wake FAILED to hibernated, keeping
// last_tap/last_ip — the identity its frozen memory actually has — intact.
// It exists because Hibernate would overwrite them with the row's current
// identity, which after a clone-path Wake is a freshly allocated pair the
// frozen memory image has never seen. Doing that corrupts the mapping
// permanently: the next Wake finds the new pair free, reports sameIdentity,
// plain-restores the snapshot on its OLD baked IP, and then polls the new one
// until the agent gate times out — a sandbox that can never be woken again.
func (r *Registry) RollbackWake(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=?, stopped_at=?, pid=0, vm_id='', socket_path='',
		    tap_device='', guest_ip=''
		 WHERE id=? AND status=?`,
		StatusHibernated, time.Now().Unix(), id, StatusRunning)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sandbox %s not running", id)
	}
	return nil
}

// Wake flips a hibernated sandbox back to running, reusing its frozen identity
// (last_tap/last_ip) when possible. Returns sameIdentity=true when that tap AND
// guest IP were still free (the caller can plain-restore the snapshot, whose
// memory has that identity baked in); otherwise fresh ones are allocated and the
// caller must go through the reidentifying clone path. The frozen pair is
// re-written on every wake so RollbackWake can restore it if the caller fails.
func (r *Registry) Wake(ctx context.Context, id string) (Sandbox, bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Sandbox{}, false, err
	}
	defer tx.Rollback()

	sb, err := scanSandbox(tx.QueryRowContext(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id=?`, id))
	if err != nil {
		return Sandbox{}, false, err
	}
	if sb.Status != StatusHibernated {
		return Sandbox{}, false, fmt.Errorf("sandbox %s is %s, not hibernated", id, sb.Status)
	}
	// Waking re-materializes the snapshot's baked memory: it must pass the
	// same admission as a create, or poking a frozen big-mem sandbox would
	// push the host past its cgroup and OOM an arbitrary running guest. On
	// rejection the TX rolls back — the row stays hibernated, artifacts
	// intact, wakeable once capacity frees.
	if err := r.checkMemBudget(ctx, tx, sb.MemMIB); err != nil {
		return Sandbox{}, false, err
	}

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return Sandbox{}, false, err
	}
	// A hibernated row's live identity columns are empty; last_tap/last_ip name
	// the pair baked into its memory image. Fall back to the live columns for a
	// row frozen by a binary that predates them (migrate() normalizes those, so
	// this only covers a downgrade/upgrade straddle) and persist what we resolved,
	// so RollbackWake has the frozen identity to put back.
	frozenTap, frozenIP := sb.LastTap, sb.LastIP
	if frozenTap == "" && frozenIP == "" {
		frozenTap, frozenIP = sb.TapDevice, sb.GuestIP
	}
	same := frozenTap != "" && frozenIP != "" && !used.taps[frozenTap] && !used.ips[frozenIP]
	if same {
		sb.TapDevice, sb.GuestIP = frozenTap, frozenIP
	} else {
		if sb.TapDevice, err = pickFreeTap(used, r.pools); err != nil {
			return Sandbox{}, false, err
		}
		if sb.GuestIP, err = pickFreeIP(used, r.pools); err != nil {
			return Sandbox{}, false, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sandboxes SET status=?, stopped_at=NULL, tap_device=?, guest_ip=?, last_tap=?, last_ip=? WHERE id=?`,
		StatusRunning, sb.TapDevice, sb.GuestIP, frozenTap, frozenIP, id); err != nil {
		return Sandbox{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, false, err
	}
	sb.Status = StatusRunning
	sb.StoppedAt = nil
	sb.LastTap, sb.LastIP = frozenTap, frozenIP
	return sb, same, nil
}

// ListRouted returns the sandboxes this host must answer for: running and
// hibernated (a hibernated sandbox is still addressable — a request wakes it).
func (r *Registry) ListRouted(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE status IN (?, ?) ORDER BY created_at DESC`,
		StatusRunning, StatusHibernated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

// RoutedCapacity returns the routed sandboxes and allocatable capacity from
// one SQLite read snapshot. Heartbeats must not call ListRouted and FreeSlots
// separately: concurrent destroys can otherwise make the first read report
// (for example) 7 running sandboxes while the later read reports 46 free
// slots, an impossible 53/48 accounting state.
func (r *Registry) RoutedCapacity(ctx context.Context) ([]Sandbox, int, error) {
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE status IN (?, ?, ?, ?, ?, ?) ORDER BY created_at DESC`,
		StatusRunning, StatusHibernated, StatusPreparing, StatusWarming, StatusStarting, StatusStopping)
	if err != nil {
		return nil, 0, err
	}
	all, err := collectSandboxes(rows)
	rows.Close()
	if err != nil {
		return nil, 0, err
	}

	occupied := 0
	var committedMem int64
	routed := make([]Sandbox, 0, len(all))
	for _, sb := range all {
		if sb.Status == StatusWarming || sb.Status == StatusPreparing ||
			sb.Status == StatusStarting || sb.Status == StatusStopping {
			occupied++
		} else {
			routed = append(routed, sb)
		}
		if sb.Status != StatusRunning && sb.Status != StatusWarming && sb.Status != StatusPreparing &&
			sb.Status != StatusStarting && sb.Status != StatusStopping {
			continue
		}
		if sb.Status == StatusRunning {
			occupied++
		}
		mem := sb.MemMIB
		if mem == 0 {
			mem = r.mem.TemplateMemMIB
		}
		committedMem += mem + r.mem.OverheadMIB
	}
	return routed, r.freeSlotsFor(occupied, committedMem), nil
}

// Destroy removes a sandbox row outright, along with its port mappings.
func (r *Registry) Destroy(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_ports WHERE sandbox_id=?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM sandboxes WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %s not found", id)
	}
	return tx.Commit()
}

// AddPort allocates a host port from the shared pool and records a mapping.
// It preserves the original API used by callers that explicitly need a worker
// host port; a URL-only row is upgraded in place.
func (r *Registry) AddPort(ctx context.Context, id string, guestPort int) (int, error) {
	pm, err := r.addPort(ctx, id, guestPort, true)
	return pm.HostPort, err
}

// AddURLPort records an exposure without consuming the worker host-port pool.
// An existing host-port mapping is returned unchanged: every explicit mapping
// is authorized for the ingress tunnel, so downgrading it would be surprising.
func (r *Registry) AddURLPort(ctx context.Context, id string, guestPort int) (PortMapping, error) {
	return r.addPort(ctx, id, guestPort, false)
}

func (r *Registry) addPort(ctx context.Context, id string, guestPort int, allocateHost bool) (PortMapping, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return PortMapping{}, err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM sandboxes WHERE id=?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PortMapping{}, fmt.Errorf("sandbox %s not found", id)
		}
		return PortMapping{}, err
	}

	var existing sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT host_port FROM sandbox_ports WHERE sandbox_id=? AND guest_port=?`, id, guestPort).Scan(&existing)
	exists := err == nil
	if err == nil {
		if existing.Valid || !allocateHost {
			return portMapping(guestPort, existing), nil
		}
		// Upgrade a URL-only mapping below after allocating a host port.
	} else if !errors.Is(err, sql.ErrNoRows) {
		return PortMapping{}, err
	}
	if !allocateHost {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sandbox_ports (sandbox_id, guest_port, host_port) VALUES (?, ?, NULL)`,
			id, guestPort); err != nil {
			return PortMapping{}, fmt.Errorf("insert URL port mapping: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return PortMapping{}, err
		}
		return PortMapping{GuestPort: guestPort, Mode: "url"}, nil
	}

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return PortMapping{}, err
	}
	port, err := pickFreePort(used, r.pools)
	if err != nil {
		return PortMapping{}, err
	}
	var writeErr error
	if exists {
		res, updateErr := tx.ExecContext(ctx,
			`UPDATE sandbox_ports SET host_port=? WHERE sandbox_id=? AND guest_port=?`,
			port, id, guestPort)
		writeErr = updateErr
		if updateErr == nil {
			if n, _ := res.RowsAffected(); n == 0 {
				_, writeErr = tx.ExecContext(ctx,
					`INSERT INTO sandbox_ports (sandbox_id, guest_port, host_port) VALUES (?, ?, ?)`,
					id, guestPort, port)
			}
		}
	} else {
		_, writeErr = tx.ExecContext(ctx,
			`INSERT INTO sandbox_ports (sandbox_id, guest_port, host_port) VALUES (?, ?, ?)`,
			id, guestPort, port)
	}
	if writeErr != nil {
		return PortMapping{}, fmt.Errorf("insert port mapping: %w", writeErr)
	}
	if err := tx.Commit(); err != nil {
		return PortMapping{}, err
	}
	return PortMapping{GuestPort: guestPort, HostPort: port, Mode: "host_port"}, nil
}

// PublicRoute is one raw-ingress route: a fleet-wide public TCP port pointing
// at a guest port of a specific sandbox.
type PublicRoute struct {
	SandboxID  string
	GuestPort  int
	PublicPort int
}

// PublicRoutes returns every mapping that carries a public port, for the whole
// host, in ONE query. The heartbeat used to call Ports() once per routed
// sandbox: O(N) round trips through the registry every 5 s, which on a host
// with a few hundred sandboxes is the single biggest consumer of registry time
// and (before the reader split) queued in front of creates. Only rows with a
// public port exist here, so the result stays small even when N is large; the
// caller filters against the routed set it already has, which keeps
// "advertised routes ⊆ advertised sandboxes" a property of the caller's own
// snapshot rather than of a status JOIN that could drift from it.
func (r *Registry) PublicRoutes(ctx context.Context) ([]PublicRoute, error) {
	r.portReads.publicRoutes.Add(1)
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT sandbox_id, guest_port, public_port FROM sandbox_ports
		 WHERE public_port IS NOT NULL ORDER BY public_port`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublicRoute
	for rows.Next() {
		var pr PublicRoute
		if err := rows.Scan(&pr.SandboxID, &pr.GuestPort, &pr.PublicPort); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

// Ports returns all explicitly exposed port mappings of a sandbox.
func (r *Registry) Ports(ctx context.Context, id string) ([]PortMapping, error) {
	r.portReads.ports.Add(1)
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT guest_port, host_port, public_port FROM sandbox_ports WHERE sandbox_id=? ORDER BY guest_port`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortMapping
	for rows.Next() {
		var guestPort int
		var hostPort, publicPort sql.NullInt64
		if err := rows.Scan(&guestPort, &hostPort, &publicPort); err != nil {
			return nil, err
		}
		pm := portMapping(guestPort, hostPort)
		if publicPort.Valid {
			pm.PublicPort = int(publicPort.Int64)
			if pm.HostPort == 0 {
				pm.Mode = "raw"
			}
		}
		out = append(out, pm)
	}
	return out, rows.Err()
}

// SetPublicPort attaches the fleet-wide raw TCP allocation chosen by the
// gateway to an existing explicit exposure.
func (r *Registry) SetPublicPort(ctx context.Context, id string, guestPort, publicPort int) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandbox_ports SET public_port=? WHERE sandbox_id=? AND guest_port=?`,
		publicPort, id, guestPort)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("sandbox %s does not expose guest port %d", id, guestPort)
	}
	return nil
}

func portMapping(guestPort int, hostPort sql.NullInt64) PortMapping {
	pm := PortMapping{GuestPort: guestPort, Mode: "url"}
	if hostPort.Valid {
		pm.HostPort = int(hostPort.Int64)
		pm.Mode = "host_port"
	}
	return pm
}

// DeletePort removes one port mapping (used to roll back a failed expose).
func (r *Registry) DeletePort(ctx context.Context, id string, guestPort int) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM sandbox_ports WHERE sandbox_id=? AND guest_port=?`, id, guestPort)
	return err
}

// --- snapshots ---

// snapshotCols is the column list every snapshot SELECT uses, in scan order.
const snapshotCols = `id, source_id, tap_device, guest_ip, guest_mac, mem_path, state_path, rootfs_path, source_rootfs_path, created_at, golden, base_mtime, base_size, format, base_id, vcpus, mem_mib, name, expires_at, durability`

// CreateSnapshot records a snapshot's metadata. The artifact files
// (mem/state/rootfs) are written by the caller before this is called.
func (r *Registry) CreateSnapshot(ctx context.Context, s Snapshot) error {
	golden := 0
	if s.Golden {
		golden = 1
	}
	format := s.Format
	if format == "" {
		format = FormatFull
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO snapshots (id, source_id, tap_device, guest_ip, guest_mac, mem_path, state_path, rootfs_path, source_rootfs_path, created_at, golden, base_mtime, base_size, format, base_id, vcpus, mem_mib, name, expires_at, durability)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.SourceID, s.TapDevice, s.GuestIP, s.GuestMAC, s.MemPath, s.StatePath, s.RootfsPath, s.SourceRootfsPath, s.CreatedAt.Unix(), golden, s.BaseMtime, s.BaseSize, format, s.BaseID, s.Vcpus, s.MemMIB, s.Name, unixOrNil(s.ExpiresAt), snapshotDurability(s.Durability))
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// GoldenSnapshot returns the snapshot marked golden (sql.ErrNoRows if none).
func (r *Registry) GoldenSnapshot(ctx context.Context) (Snapshot, error) {
	row := r.rdb.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE golden=1`)
	return scanSnapshot(row)
}

// GetSnapshot returns a snapshot by id.
func (r *Registry) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	row := r.rdb.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id=?`, id)
	return scanSnapshot(row)
}

// ListSnapshots returns all snapshots (most recent first).
func (r *Registry) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT `+snapshotCols+` FROM snapshots ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetSnapshotPublicFields persists mutable management metadata without
// changing the immutable snapshot artifacts.
func (r *Registry) SetSnapshotPublicFields(ctx context.Context, id, name string, expiresAt *time.Time) (Snapshot, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE snapshots SET name=?, expires_at=? WHERE id=? AND golden=0`,
		name, unixOrNil(expiresAt), id)
	if err != nil {
		return Snapshot{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Snapshot{}, sql.ErrNoRows
	}
	return r.GetSnapshot(ctx, id)
}

// SetSnapshotDurability records whether the object-store commit marker exists.
func (r *Registry) SetSnapshotDurability(ctx context.Context, id, durability string) error {
	if durability != "local" && durability != "durable" {
		return fmt.Errorf("invalid snapshot durability %q", durability)
	}
	res, err := r.db.ExecContext(ctx, `UPDATE snapshots SET durability=? WHERE id=?`, durability, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ExpiredSnapshots returns non-golden snapshots whose retention deadline has
// passed. Deletion still performs dependency checks.
func (r *Registry) ExpiredSnapshots(ctx context.Context, now time.Time) ([]Snapshot, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE golden=0 AND expires_at IS NOT NULL AND expires_at < ? ORDER BY expires_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeleteSnapshot removes a snapshot row. The caller removes the artifact files.
func (r *Registry) DeleteSnapshot(ctx context.Context, id string) error {
	dependencies, err := r.SnapshotDependencyCount(ctx, id)
	if err != nil {
		return err
	}
	if dependencies > 0 {
		return fmt.Errorf("%w: snapshot %s has %d dependent resources", ErrSnapshotInUse, id, dependencies)
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("snapshot %s not found", id)
	}
	return nil
}

// SnapshotDependencyCount reports rows that make id unsafe to delete. The
// server calls this while holding its per-snapshot operation lock before it
// cancels durability or removes the GCS commit marker.
func (r *Registry) SnapshotDependencyCount(ctx context.Context, id string) (int, error) {
	var dependencies int
	if err := r.rdb.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM snapshots WHERE base_id=?1) +
		  (SELECT COUNT(*) FROM sandboxes WHERE source_type='snapshot' AND source_id=?1)`,
		id).Scan(&dependencies); err != nil {
		return 0, err
	}
	return dependencies, nil
}

func scanSnapshot(r rowScanner) (Snapshot, error) {
	var s Snapshot
	var createdAt int64
	var expiresAt sql.NullInt64
	var golden int
	err := r.Scan(&s.ID, &s.SourceID, &s.TapDevice, &s.GuestIP, &s.GuestMAC, &s.MemPath, &s.StatePath, &s.RootfsPath, &s.SourceRootfsPath, &createdAt, &golden, &s.BaseMtime, &s.BaseSize, &s.Format, &s.BaseID, &s.Vcpus, &s.MemMIB, &s.Name, &expiresAt, &s.Durability)
	if err != nil {
		return s, err
	}
	s.CreatedAt = time.Unix(createdAt, 0)
	if expiresAt.Valid {
		value := time.Unix(expiresAt.Int64, 0)
		s.ExpiresAt = &value
	}
	s.Golden = golden == 1
	if s.Format == "" {
		s.Format = FormatFull
	}
	return s, nil
}

func snapshotDurability(value string) string {
	if value == "durable" {
		return value
	}
	return "local"
}

// sandboxCols is the column list every sandbox SELECT uses, in scanSandbox order.
const sandboxCols = `id, pid, vm_id, socket_path, tap_device, guest_ip, rootfs_path, status, created_at, stopped_at, expires_at, base_snapshot_id, hibernate_after_sec, vcpus, mem_mib, name, metadata, source_type, source_id, last_tap, last_ip`

// Get returns the sandbox row for the given ID.
func (r *Registry) Get(ctx context.Context, id string) (Sandbox, error) {
	row := r.rdb.QueryRowContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id=?`, id)
	return scanSandbox(row)
}

// All returns every row regardless of status (most recent first).
// Used by startup reconciliation to find stale state from a previous server run.
func (r *Registry) All(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

// List returns all running sandboxes (most recent first).
func (r *Registry) List(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.rdb.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE status=? ORDER BY created_at DESC`, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSandbox(r rowScanner) (Sandbox, error) {
	var sb Sandbox
	var createdAt int64
	var stoppedAt, expiresAt sql.NullInt64
	var metadata string
	err := r.Scan(&sb.ID, &sb.PID, &sb.VMID, &sb.SocketPath, &sb.TapDevice, &sb.GuestIP, &sb.RootfsPath, &sb.Status, &createdAt, &stoppedAt, &expiresAt, &sb.BaseSnapshotID, &sb.HibernateAfterSec, &sb.Vcpus, &sb.MemMIB, &sb.Name, &metadata, &sb.SourceType, &sb.SourceID, &sb.LastTap, &sb.LastIP)
	if err != nil {
		return sb, err
	}
	sb.CreatedAt = time.Unix(createdAt, 0)
	if stoppedAt.Valid {
		t := time.Unix(stoppedAt.Int64, 0)
		sb.StoppedAt = &t
	}
	if expiresAt.Valid {
		t := time.Unix(expiresAt.Int64, 0)
		sb.ExpiresAt = &t
	}
	if err := json.Unmarshal([]byte(metadata), &sb.Metadata); err != nil {
		return sb, fmt.Errorf("decode sandbox metadata: %w", err)
	}
	if sb.Metadata == nil {
		sb.Metadata = map[string]string{}
	}
	return sb, nil
}

// SetPublicFields records v1-only descriptive state after a legacy creation
// path has allocated the sandbox. It never changes runtime placement.
func (r *Registry) SetPublicFields(ctx context.Context, id, sourceType, sourceID string, metadata map[string]string) (Sandbox, error) {
	if sourceType == "" {
		sourceType = "default"
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return Sandbox{}, err
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE sandboxes SET source_type=?, source_id=?, metadata=? WHERE id=?`,
		sourceType, sourceID, string(encoded), id); err != nil {
		return Sandbox{}, err
	}
	return r.Get(ctx, id)
}

// UpdatePublicFields atomically changes the mutable v1 sandbox fields.
func (r *Registry) UpdatePublicFields(ctx context.Context, id, name string, metadata map[string]string, expiresAt *time.Time, idleTimeout int) (Sandbox, error) {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return Sandbox{}, err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE sandboxes SET name=?, metadata=?, expires_at=?, hibernate_after_sec=? WHERE id=?`,
		name, string(encoded), unixOrNil(expiresAt), idleTimeout, id)
	if err != nil {
		return Sandbox{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Sandbox{}, sql.ErrNoRows
	}
	return r.Get(ctx, id)
}

// MarkWarmReady makes a fully initialized preparing VM claimable. The caller
// invokes this only after finishClone has completed every readiness/security
// gate.
func (r *Registry) MarkWarmReady(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=? WHERE id=? AND status=?`,
		StatusWarming, id, StatusPreparing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("sandbox %s is not preparing", id)
	}
	return nil
}

// ClaimWarm atomically promotes the oldest hidden ready VM into a routed
// sandbox and applies the request's mutable fields.
func (r *Registry) ClaimWarm(ctx context.Context, name string, expiresAt *time.Time, idleTimeout int) (Sandbox, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback()

	sb, err := scanSandbox(tx.QueryRowContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE status=? ORDER BY created_at LIMIT 1`,
		StatusWarming))
	if err != nil {
		return Sandbox{}, err
	}
	claimedAt := time.Now()
	res, err := tx.ExecContext(ctx,
		`UPDATE sandboxes SET status=?, name=?, created_at=?, expires_at=?, hibernate_after_sec=? WHERE id=? AND status=?`,
		StatusRunning, name, claimedAt.Unix(), unixOrNil(expiresAt), idleTimeout, sb.ID, StatusWarming)
	if err != nil {
		return Sandbox{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Sandbox{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, err
	}
	sb.Status = StatusRunning
	sb.Name = name
	sb.CreatedAt = claimedAt
	sb.ExpiresAt = expiresAt
	sb.HibernateAfterSec = idleTimeout
	return sb, nil
}

// WarmCount reports hidden ready VMs, used by the background replenisher.
func (r *Registry) WarmCount(ctx context.Context) (int, error) {
	var n int
	err := r.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM sandboxes WHERE status=?`, StatusWarming).Scan(&n)
	return n, err
}

// WarmInventory reports ready and still-preparing pool entries.
func (r *Registry) WarmInventory(ctx context.Context) (ready, preparing int, err error) {
	err = r.rdb.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sandboxes WHERE status=?1),
			(SELECT COUNT(*) FROM sandboxes WHERE status=?2)`,
		StatusWarming, StatusPreparing).Scan(&ready, &preparing)
	return
}

func collectSandboxes(rows *sql.Rows) ([]Sandbox, error) {
	var out []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// unixOrNil converts an optional time to a nullable SQL value.
func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

type usedResources struct {
	taps  map[string]bool
	ips   map[string]bool
	ports map[int]bool
	// soft* hold the REMEMBERED tap/IP of HIBERNATED sandboxes (last_tap/
	// last_ip — their live identity columns are empty). They're free to take
	// (the frozen VM isn't using them), but the pickers avoid them while other
	// pool entries remain, so a wake almost always finds its old tap/IP
	// unclaimed and can restore the same identity (skipping the reidentify
	// dance).
	softTaps map[string]bool
	softIPs  map[string]bool
}

func loadUsed(ctx context.Context, tx *sql.Tx) (usedResources, error) {
	u := usedResources{
		taps:  map[string]bool{},
		ips:   map[string]bool{},
		ports: map[int]bool{},

		softTaps: map[string]bool{},
		softIPs:  map[string]bool{},
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT tap_device, guest_ip, last_tap, last_ip, status FROM sandboxes WHERE status IN (?, ?, ?, ?, ?, ?)`,
		StatusRunning, StatusPreparing, StatusWarming, StatusHibernated, StatusStarting, StatusStopping)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var tap, ip, lastTap, lastIP, status string
		if err := rows.Scan(&tap, &ip, &lastTap, &lastIP, &status); err != nil {
			return u, err
		}
		if status == StatusHibernated {
			// The remembered pair, not the live columns (which are empty for a
			// frozen row — the tap fallback only matters for a row written by a
			// binary that predates last_tap/last_ip).
			if lastTap == "" && lastIP == "" {
				lastTap, lastIP = tap, ip
			}
			u.softTaps[lastTap] = true
			u.softIPs[lastIP] = true
			continue
		}
		// A row can hold a capacity-bearing status with no identity: a
		// hibernated sandbox moves to 'stopping' for teardown. Don't let the
		// empty string reserve a pool entry.
		if tap != "" {
			u.taps[tap] = true
		}
		if ip != "" {
			u.ips[ip] = true
		}
	}
	if err := rows.Err(); err != nil {
		return u, err
	}

	// Explicitly exposed ports are hard-reserved, including during hibernation.
	extra, err := tx.QueryContext(ctx, `SELECT host_port FROM sandbox_ports WHERE host_port IS NOT NULL`)
	if err != nil {
		return u, err
	}
	defer extra.Close()
	for extra.Next() {
		var port int
		if err := extra.Scan(&port); err != nil {
			return u, err
		}
		u.ports[port] = true
	}
	return u, extra.Err()
}

// ErrPoolExhausted marks capacity-class allocation failures: every entry of a
// resource pool (tap/IP/port) is in use. Handlers map it to 503 + Retry-After
// (it clears as sandboxes are destroyed or the fleet scales), and the gateway
// fails a create over to another host on it — unlike a genuine 500.
var (
	ErrPoolExhausted = errors.New("pool exhausted")
	ErrSnapshotInUse = errors.New("snapshot in use")
)

// ErrUsageOpen marks an attempt to open a second billable interval for a
// sandbox that already has one open. It is a bug signal, not a capacity
// condition: two open intervals would double-bill one VM, so the ledger's
// partial unique index refuses it and callers log rather than retry.
var ErrUsageOpen = errors.New("usage interval already open")

// ErrMemExhausted marks a memory-budget admission rejection. It wraps
// ErrPoolExhausted so every existing capacity path (503 + Retry-After,
// gateway failover, fanout classification) fires unchanged.
var ErrMemExhausted = fmt.Errorf("memory budget exhausted: %w", ErrPoolExhausted)

// checkMemBudget rejects an admission that would push committed guest memory
// (running rows only — hibernated VMs are dead) past the configured budget.
// memMIB=0 resolves to the template default. No-op when admission is disabled.
// Runs inside the caller's transaction; the single-connection DB serializes
// admissions, so the read-then-insert is race-free.
func (r *Registry) checkMemBudget(ctx context.Context, tx *sql.Tx, memMIB int64) error {
	if r.mem.BudgetMIB <= 0 {
		return nil
	}
	if memMIB == 0 {
		memMIB = r.mem.TemplateMemMIB
	}
	need := memMIB + r.mem.OverheadMIB
	var committed int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CASE WHEN mem_mib = 0 THEN ? ELSE mem_mib END + ?), 0)
		   FROM sandboxes WHERE status IN (?, ?, ?, ?, ?)`,
		r.mem.TemplateMemMIB, r.mem.OverheadMIB,
		StatusRunning, StatusPreparing, StatusWarming, StatusStarting, StatusStopping,
	).Scan(&committed); err != nil {
		return err
	}
	if committed+need > r.mem.BudgetMIB {
		return fmt.Errorf("need %d MiB with %d of %d MiB budget committed: %w",
			need, committed, r.mem.BudgetMIB, ErrMemExhausted)
	}
	return nil
}

// The tap/IP pickers scan their pool twice: first skipping identities parked
// by hibernated sandboxes (soft), then — only when the pool is otherwise
// exhausted — allowing them. Hibernated taps/IPs are legitimately free;
// avoiding them just keeps same-identity wakes cheap. Explicit ports are
// always hard-used because their listeners stay bound.

func pickFreeTap(used usedResources, p Pools) (string, error) {
	for _, avoidSoft := range []bool{true, false} {
		for i := 0; i < p.TapMax; i++ {
			name := fmt.Sprintf("%s%d", p.TapPrefix, i)
			if !used.taps[name] && !(avoidSoft && used.softTaps[name]) {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("tap pool exhausted: %w", ErrPoolExhausted)
}

func pickFreeIP(used usedResources, p Pools) (string, error) {
	minIP, err := ipToUint32(p.GuestIPMin)
	if err != nil {
		return "", err
	}
	maxIP, err := ipToUint32(p.GuestIPMax)
	if err != nil {
		return "", err
	}
	for _, avoidSoft := range []bool{true, false} {
		for n := minIP; n <= maxIP; n++ {
			s := uint32ToIP(n)
			if !used.ips[s] && !(avoidSoft && used.softIPs[s]) {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("ip pool exhausted: %w", ErrPoolExhausted)
}

func pickFreePort(used usedResources, p Pools) (int, error) {
	for port := p.PortMin; port <= p.PortMax; port++ {
		if !used.ports[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("port pool exhausted: %w", ErrPoolExhausted)
}

func ipToUint32(s string) (uint32, error) {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return 0, fmt.Errorf("invalid IPv4 %q", s)
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3]), nil
}

func uint32ToIP(n uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}
