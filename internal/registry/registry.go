package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusRunning  = "running"
	StatusStopping = "stopping"
	// StatusHibernated marks an idle sandbox frozen to disk: its VM is gone and
	// its tap/IP are released back to the pools (their partial unique indexes
	// only bind status='running'), but its host port(s) stay reserved — the
	// server keeps the userspace port-forward listeners bound so a connection
	// can wake it (uniq_port_held covers hibernated rows too). The row, rootfs
	// file, and hibernation snapshot survive — including across server
	// restarts. Any agent-bound request or forwarded-port connection wakes it.
	StatusHibernated = "hibernated"
)

// Sandbox represents a row in the sandboxes table.
type Sandbox struct {
	ID string `json:"id"`
	// Name is a free-form display label, settable at create time and via
	// POST /sandboxes/{id}/rename. Not unique, not a lookup key; "" = unnamed.
	Name       string     `json:"name,omitempty"`
	PID        int        `json:"pid"`
	VMID       string     `json:"vm_id"`
	SocketPath string     `json:"socket_path"`
	TapDevice  string     `json:"tap_device"`
	GuestIP    string     `json:"guest_ip"`
	HostPort   int        `json:"host_port"`
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
}

// PortMapping is one exposed guest port → host port pair.
type PortMapping struct {
	GuestPort int `json:"guest_port"`
	HostPort  int `json:"host_port"`
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
	MemPath   string `json:"mem_path"`
	StatePath string `json:"state_path"`
	// RootfsPath is the frozen rootfs copy this snapshot restores FROM.
	RootfsPath string `json:"rootfs_path"`
	// SourceRootfsPath is the disk path baked into the Firecracker snapshot —
	// a restore must place its rootfs copy here, or Firecracker can't reattach
	// the block device.
	SourceRootfsPath string    `json:"source_rootfs_path"`
	CreatedAt        time.Time `json:"created_at"`
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

// Slots returns the host's effective sandbox capacity: the smallest of the
// three pools, since every sandbox consumes one tap, one IP, and one primary
// host port. (Extra exposed ports draw from the same port pool, so this is an
// upper bound on concurrently-running sandboxes, good enough for placement.)
func (p Pools) Slots() int {
	n := p.TapMax
	if c := p.PortMax - p.PortMin + 1; c < n {
		n = c
	}
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
// the smallest per-pool availability. Running sandboxes hold a tap, an IP, and
// a port; hibernated sandboxes hold ONLY their port (the wake-on-connect
// listener stays bound — see loadUsed), their tap/IP being soft-reserved and
// allocatable. Extra exposed ports draw from the same port pool. This is what
// the heartbeat must advertise: Slots()-running overstates capacity whenever
// hibernated port-holds make ports the binding pool.
func (r *Registry) FreeSlots(ctx context.Context) (int, error) {
	var running, portsHeld, extraPorts int
	err := r.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM sandboxes WHERE status = ?),
			(SELECT COUNT(*) FROM sandboxes WHERE status IN (?, ?)),
			(SELECT COUNT(*) FROM sandbox_ports)`,
		StatusRunning, StatusRunning, StatusHibernated,
	).Scan(&running, &portsHeld, &extraPorts)
	if err != nil {
		return 0, err
	}
	free := r.pools.TapMax - running
	if f := r.pools.ipPoolSize() - running; f < free {
		free = f
	}
	if f := (r.pools.PortMax - r.pools.PortMin + 1) - portsHeld - extraPorts; f < free {
		free = f
	}
	if free < 0 {
		free = 0
	}
	return free, nil
}

// Registry wraps the SQLite-backed sandbox state.
type Registry struct {
	db    *sql.DB
	pools Pools
}

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
	// They're sub-millisecond and creates are bottlenecked on rootfs copy + VM
	// boot, so this isn't a throughput concern; cross-host parallelism is
	// unaffected (each host has its own DB).
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
	return r, nil
}

// Close releases the database handle.
func (r *Registry) Close() error { return r.db.Close() }

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
		host_port   INTEGER NOT NULL,
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
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_tap_running  ON sandboxes(tap_device) WHERE status = 'running';
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_ip_running   ON sandboxes(guest_ip)   WHERE status = 'running';
	CREATE TABLE IF NOT EXISTS sandbox_ports (
		sandbox_id TEXT NOT NULL,
		guest_port INTEGER NOT NULL,
		host_port  INTEGER NOT NULL,
		PRIMARY KEY (sandbox_id, guest_port)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_extra_host_port ON sandbox_ports(host_port);
	CREATE TABLE IF NOT EXISTS snapshots (
		id                 TEXT PRIMARY KEY,
		source_id          TEXT NOT NULL,
		tap_device         TEXT NOT NULL,
		guest_ip           TEXT NOT NULL,
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
	);
	`
	if _, err := r.db.Exec(schema); err != nil {
		return err
	}
	// source_rootfs_path was added after the snapshots table first shipped.
	if _, err := r.db.Exec(`ALTER TABLE snapshots ADD COLUMN source_rootfs_path TEXT NOT NULL DEFAULT ''`); err != nil &&
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
	// Host ports moved from kernel DNAT to in-process listeners, which stay
	// bound while a sandbox is hibernated (wake-on-connect). A hibernated
	// sandbox therefore holds its port outright: uniq_port_held replaces the
	// running-only uniq_port_running. Old databases may carry collisions from
	// the soft-reservation era (a full pool let creates squat hibernated
	// ports), so dedup before creating the stricter index.
	if _, err := r.db.Exec(`DROP INDEX IF EXISTS uniq_port_running`); err != nil {
		return err
	}
	if err := r.dedupHibernatedPorts(); err != nil {
		return fmt.Errorf("dedup hibernated ports: %w", err)
	}
	if _, err := r.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uniq_port_held ON sandboxes(host_port) WHERE status IN ('running','hibernated')`); err != nil {
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
	} {
		if _, err := r.db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// dedupHibernatedPorts reassigns the host_port of hibernated rows that collide
// with another routed row's port (or an extra exposed port). Only the
// pre-proxy scheme could produce such rows — hibernated ports were then
// soft-reserved, and nothing listened on them, so moving one here (before any
// listener opens) is exactly what the old wake path would have done.
func (r *Registry) dedupHibernatedPorts() error {
	tx, err := r.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	seen := map[int]bool{}
	for _, q := range []string{
		`SELECT host_port FROM sandboxes WHERE status = 'running'`,
		`SELECT host_port FROM sandbox_ports`,
	} {
		rows, err := tx.Query(q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var port int
			if err := rows.Scan(&port); err != nil {
				rows.Close()
				return err
			}
			seen[port] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}

	rows, err := tx.Query(`SELECT id, host_port FROM sandboxes WHERE status = 'hibernated' ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	var collided []string
	for rows.Next() {
		var id string
		var port int
		if err := rows.Scan(&id, &port); err != nil {
			rows.Close()
			return err
		}
		if seen[port] {
			collided = append(collided, id)
			continue
		}
		seen[port] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range collided {
		port, err := pickFreePort(usedResources{ports: seen}, r.pools)
		if err != nil {
			return fmt.Errorf("reassign port of hibernated sandbox %s: %w", id, err)
		}
		if _, err := tx.Exec(`UPDATE sandboxes SET host_port=? WHERE id=?`, port, id); err != nil {
			return err
		}
		seen[port] = true
	}
	return tx.Commit()
}

// Create allocates a tap/IP/port from the pools and inserts a 'running' row
// for the new sandbox. PID/VMID/SocketPath are filled in later via FinishStart
// once firecracker is up. A non-nil expiresAt marks the sandbox for
// auto-destroy by the server's reaper. baseSnapshotID records the golden
// snapshot the sandbox is cloned from ("" for cold boots and user fan-outs) —
// it makes the sandbox diff-snapshottable. hibernateAfterSec is the
// per-sandbox idle-hibernation override (>0 seconds, -1 never, 0 host default).
// vcpus/memMIB are per-sandbox resource overrides (0 = template default).
// name is the free-form display label ("" = unnamed).
func (r *Registry) Create(ctx context.Context, id, name, rootfsPath string, expiresAt *time.Time, baseSnapshotID string, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
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
	port, err := pickFreePort(used, r.pools)
	if err != nil {
		return Sandbox{}, err
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sandboxes (id, name, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, expires_at, base_snapshot_id, hibernate_after_sec, vcpus, mem_mib)
		 VALUES (?, ?, 0, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, tap, ip, port, rootfsPath, StatusRunning, now.Unix(), unixOrNil(expiresAt), baseSnapshotID, hibernateAfterSec, vcpus, memMIB)
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
		HostPort:          port,
		RootfsPath:        rootfsPath,
		Status:            StatusRunning,
		CreatedAt:         now,
		ExpiresAt:         expiresAt,
		BaseSnapshotID:    baseSnapshotID,
		HibernateAfterSec: hibernateAfterSec,
		Vcpus:             vcpus,
		MemMIB:            memMIB,
	}, nil
}

// CreateRestore inserts a 'running' row for a sandbox restored from a snapshot.
// Unlike Create, the tap and guest IP are fixed (the snapshot baked them in) —
// only the host port is freshly allocated. The partial unique indexes still
// guarantee the tap/IP aren't already taken by a running sandbox, so a restore
// fails cleanly if the source (or a prior restore of the same snapshot) is
// still live. vcpus/memMIB carry the snapshot's recorded resources — the
// restore can't change them (they're baked into the snapshot), it just
// reports them.
func (r *Registry) CreateRestore(ctx context.Context, id, name, rootfsPath, tap, ip string, expiresAt *time.Time, hibernateAfterSec int, vcpus, memMIB int64) (Sandbox, error) {
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
	port, err := pickFreePort(used, r.pools)
	if err != nil {
		return Sandbox{}, err
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sandboxes (id, name, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, expires_at, hibernate_after_sec, vcpus, mem_mib)
		 VALUES (?, ?, 0, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, tap, ip, port, rootfsPath, StatusRunning, now.Unix(), unixOrNil(expiresAt), hibernateAfterSec, vcpus, memMIB)
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
		HostPort:          port,
		RootfsPath:        rootfsPath,
		Status:            StatusRunning,
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

// Expired returns running or hibernated sandboxes whose expires_at has passed
// (a hibernated sandbox's TTL keeps counting — it's frozen, not immortal).
func (r *Registry) Expired(ctx context.Context, now time.Time) ([]Sandbox, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		 WHERE status IN (?, ?) AND expires_at IS NOT NULL AND expires_at < ?`,
		StatusRunning, StatusHibernated, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

// Hibernate marks a running sandbox as hibernated. The caller has already
// frozen the VM and released its host-side resources; from here the partial
// unique indexes stop binding the row's tap/IP, so new sandboxes may take
// them (Wake handles that with a fresh identity). The host port stays bound —
// uniq_port_held covers hibernated rows, because the server keeps the
// wake-on-connect listener on it.
func (r *Registry) Hibernate(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sandboxes SET status=?, stopped_at=?, pid=0, vm_id='', socket_path='' WHERE id=? AND status=?`,
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

// Wake flips a hibernated sandbox back to running, reusing its old identity
// when possible. Returns sameIdentity=true when the old tap AND guest IP were
// still free (the caller can plain-restore the snapshot, whose memory has that
// identity baked in); otherwise fresh ones are allocated and the caller must
// go through the reidentifying clone path. The host port is always kept: it
// stays hard-reserved throughout hibernation (the wake-on-connect listener
// never let go of it).
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

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return Sandbox{}, false, err
	}
	same := !used.taps[sb.TapDevice] && !used.ips[sb.GuestIP]
	if !same {
		if sb.TapDevice, err = pickFreeTap(used, r.pools); err != nil {
			return Sandbox{}, false, err
		}
		if sb.GuestIP, err = pickFreeIP(used, r.pools); err != nil {
			return Sandbox{}, false, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sandboxes SET status=?, stopped_at=NULL, tap_device=?, guest_ip=?, host_port=? WHERE id=?`,
		StatusRunning, sb.TapDevice, sb.GuestIP, sb.HostPort, id); err != nil {
		return Sandbox{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, false, err
	}
	sb.Status = StatusRunning
	sb.StoppedAt = nil
	return sb, same, nil
}

// ListRouted returns the sandboxes this host must answer for: running and
// hibernated (a hibernated sandbox is still addressable — a request wakes it).
func (r *Registry) ListRouted(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE status IN (?, ?) ORDER BY created_at DESC`,
		StatusRunning, StatusHibernated)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

// Destroy removes a sandbox row outright, along with its extra port mappings.
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

// AddPort allocates a host port from the shared pool and records a
// guestPort → hostPort mapping for the sandbox. If the mapping already
// exists, the existing host port is returned.
func (r *Registry) AddPort(ctx context.Context, id string, guestPort int) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM sandboxes WHERE id=?`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("sandbox %s not found", id)
		}
		return 0, err
	}

	var existing int
	err = tx.QueryRowContext(ctx,
		`SELECT host_port FROM sandbox_ports WHERE sandbox_id=? AND guest_port=?`, id, guestPort).Scan(&existing)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return 0, err
	}
	port, err := pickFreePort(used, r.pools)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sandbox_ports (sandbox_id, guest_port, host_port) VALUES (?, ?, ?)`,
		id, guestPort, port); err != nil {
		return 0, fmt.Errorf("insert port mapping: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return port, nil
}

// Ports returns the extra port mappings of a sandbox (the implicit primary
// guest-port mapping lives on the sandbox row itself).
func (r *Registry) Ports(ctx context.Context, id string) ([]PortMapping, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT guest_port, host_port FROM sandbox_ports WHERE sandbox_id=? ORDER BY guest_port`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortMapping
	for rows.Next() {
		var pm PortMapping
		if err := rows.Scan(&pm.GuestPort, &pm.HostPort); err != nil {
			return nil, err
		}
		out = append(out, pm)
	}
	return out, rows.Err()
}

// DeletePort removes one extra port mapping (used to roll back a failed expose).
func (r *Registry) DeletePort(ctx context.Context, id string, guestPort int) error {
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM sandbox_ports WHERE sandbox_id=? AND guest_port=?`, id, guestPort)
	return err
}

// --- snapshots ---

// snapshotCols is the column list every snapshot SELECT uses, in scan order.
const snapshotCols = `id, source_id, tap_device, guest_ip, mem_path, state_path, rootfs_path, source_rootfs_path, created_at, golden, base_mtime, base_size, format, base_id, vcpus, mem_mib, name`

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
		`INSERT INTO snapshots (id, source_id, tap_device, guest_ip, mem_path, state_path, rootfs_path, source_rootfs_path, created_at, golden, base_mtime, base_size, format, base_id, vcpus, mem_mib, name)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.SourceID, s.TapDevice, s.GuestIP, s.MemPath, s.StatePath, s.RootfsPath, s.SourceRootfsPath, s.CreatedAt.Unix(), golden, s.BaseMtime, s.BaseSize, format, s.BaseID, s.Vcpus, s.MemMIB, s.Name)
	if err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}
	return nil
}

// GoldenSnapshot returns the snapshot marked golden (sql.ErrNoRows if none).
func (r *Registry) GoldenSnapshot(ctx context.Context) (Snapshot, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE golden=1`)
	return scanSnapshot(row)
}

// GetSnapshot returns a snapshot by id.
func (r *Registry) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id=?`, id)
	return scanSnapshot(row)
}

// ListSnapshots returns all snapshots (most recent first).
func (r *Registry) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+snapshotCols+` FROM snapshots ORDER BY created_at DESC`)
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

func scanSnapshot(r rowScanner) (Snapshot, error) {
	var s Snapshot
	var createdAt int64
	var golden int
	err := r.Scan(&s.ID, &s.SourceID, &s.TapDevice, &s.GuestIP, &s.MemPath, &s.StatePath, &s.RootfsPath, &s.SourceRootfsPath, &createdAt, &golden, &s.BaseMtime, &s.BaseSize, &s.Format, &s.BaseID, &s.Vcpus, &s.MemMIB, &s.Name)
	if err != nil {
		return s, err
	}
	s.CreatedAt = time.Unix(createdAt, 0)
	s.Golden = golden == 1
	if s.Format == "" {
		s.Format = FormatFull
	}
	return s, nil
}

// sandboxCols is the column list every sandbox SELECT uses, in scanSandbox order.
const sandboxCols = `id, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, stopped_at, expires_at, base_snapshot_id, hibernate_after_sec, vcpus, mem_mib, name`

// Get returns the sandbox row for the given ID.
func (r *Registry) Get(ctx context.Context, id string) (Sandbox, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes WHERE id=?`, id)
	return scanSandbox(row)
}

// All returns every row regardless of status (most recent first).
// Used by startup reconciliation to find stale state from a previous server run.
func (r *Registry) All(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSandboxes(rows)
}

// List returns all running sandboxes (most recent first).
func (r *Registry) List(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.db.QueryContext(ctx,
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
	err := r.Scan(&sb.ID, &sb.PID, &sb.VMID, &sb.SocketPath, &sb.TapDevice, &sb.GuestIP, &sb.HostPort, &sb.RootfsPath, &sb.Status, &createdAt, &stoppedAt, &expiresAt, &sb.BaseSnapshotID, &sb.HibernateAfterSec, &sb.Vcpus, &sb.MemMIB, &sb.Name)
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
	return sb, nil
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
	// soft* hold the tap/IP of HIBERNATED sandboxes. They're free to take (the
	// frozen VM isn't using them), but the pickers avoid them while other pool
	// entries remain, so a wake almost always finds its old tap/IP unclaimed
	// and can restore the same identity (skipping the reidentify dance). Host
	// ports have no soft tier: a hibernated sandbox's ports are HARD-used —
	// the server keeps its wake-on-connect listeners bound to them.
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
		`SELECT tap_device, guest_ip, host_port, status FROM sandboxes WHERE status IN (?, ?)`,
		StatusRunning, StatusHibernated)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var tap, ip, status string
		var port int
		if err := rows.Scan(&tap, &ip, &port, &status); err != nil {
			return u, err
		}
		if status == StatusHibernated {
			u.softTaps[tap] = true
			u.softIPs[ip] = true
			u.ports[port] = true
			continue
		}
		u.taps[tap] = true
		u.ips[ip] = true
		u.ports[port] = true
	}
	if err := rows.Err(); err != nil {
		return u, err
	}

	// Extra exposed ports draw from the same host-port pool as primary ports.
	extra, err := tx.QueryContext(ctx, `SELECT host_port FROM sandbox_ports`)
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
var ErrPoolExhausted = errors.New("pool exhausted")

// The tap/IP pickers scan their pool twice: first skipping identities parked
// by hibernated sandboxes (soft), then — only when the pool is otherwise
// exhausted — allowing them. Hibernated taps/IPs are legitimately free;
// avoiding them just keeps same-identity wakes cheap. Ports have no such
// second pass — hibernated ports are hard-used (listener still bound).

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
