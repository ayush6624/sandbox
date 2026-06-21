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
)

// Sandbox represents a row in the sandboxes table.
type Sandbox struct {
	ID         string     `json:"id"`
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
}

// PortMapping is one exposed guest port → host port pair.
type PortMapping struct {
	GuestPort int `json:"guest_port"`
	HostPort  int `json:"host_port"`
}

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
	if minIP, err := ipToUint32(p.GuestIPMin); err == nil {
		if maxIP, err := ipToUint32(p.GuestIPMax); err == nil {
			if c := int(maxIP-minIP) + 1; c < n {
				n = c
			}
		}
	}
	if n < 0 {
		n = 0
	}
	return n
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
		expires_at  INTEGER
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_tap_running  ON sandboxes(tap_device) WHERE status = 'running';
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_ip_running   ON sandboxes(guest_ip)   WHERE status = 'running';
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_port_running ON sandboxes(host_port)  WHERE status = 'running';
	CREATE TABLE IF NOT EXISTS sandbox_ports (
		sandbox_id TEXT NOT NULL,
		guest_port INTEGER NOT NULL,
		host_port  INTEGER NOT NULL,
		PRIMARY KEY (sandbox_id, guest_port)
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_extra_host_port ON sandbox_ports(host_port);
	`
	if _, err := r.db.Exec(schema); err != nil {
		return err
	}
	// expires_at was added after v1 databases shipped. ALTER TABLE has no
	// IF NOT EXISTS, so ignore the duplicate-column error on migrated DBs.
	if _, err := r.db.Exec(`ALTER TABLE sandboxes ADD COLUMN expires_at INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

// Create allocates a tap/IP/port from the pools and inserts a 'running' row
// for the new sandbox. PID/VMID/SocketPath are filled in later via FinishStart
// once firecracker is up. A non-nil expiresAt marks the sandbox for
// auto-destroy by the server's reaper.
func (r *Registry) Create(ctx context.Context, id, rootfsPath string, expiresAt *time.Time) (Sandbox, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return Sandbox{}, err
	}
	defer tx.Rollback()

	used, err := loadUsed(ctx, tx)
	if err != nil {
		return Sandbox{}, err
	}
	tap, err := pickFreeTap(used.taps, r.pools)
	if err != nil {
		return Sandbox{}, err
	}
	ip, err := pickFreeIP(used.ips, r.pools)
	if err != nil {
		return Sandbox{}, err
	}
	port, err := pickFreePort(used.ports, r.pools)
	if err != nil {
		return Sandbox{}, err
	}

	now := time.Now()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sandboxes (id, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, expires_at)
		 VALUES (?, 0, '', '', ?, ?, ?, ?, ?, ?, ?)`,
		id, tap, ip, port, rootfsPath, StatusRunning, now.Unix(), unixOrNil(expiresAt))
	if err != nil {
		return Sandbox{}, fmt.Errorf("insert sandbox: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Sandbox{}, err
	}
	return Sandbox{
		ID:         id,
		TapDevice:  tap,
		GuestIP:    ip,
		HostPort:   port,
		RootfsPath: rootfsPath,
		Status:     StatusRunning,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
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

// Expired returns running sandboxes whose expires_at has passed.
func (r *Registry) Expired(ctx context.Context, now time.Time) ([]Sandbox, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+sandboxCols+` FROM sandboxes
		 WHERE status=? AND expires_at IS NOT NULL AND expires_at < ?`, StatusRunning, now.Unix())
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
	port, err := pickFreePort(used.ports, r.pools)
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

// sandboxCols is the column list every sandbox SELECT uses, in scanSandbox order.
const sandboxCols = `id, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, stopped_at, expires_at`

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
	err := r.Scan(&sb.ID, &sb.PID, &sb.VMID, &sb.SocketPath, &sb.TapDevice, &sb.GuestIP, &sb.HostPort, &sb.RootfsPath, &sb.Status, &createdAt, &stoppedAt, &expiresAt)
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
}

func loadUsed(ctx context.Context, tx *sql.Tx) (usedResources, error) {
	u := usedResources{
		taps:  map[string]bool{},
		ips:   map[string]bool{},
		ports: map[int]bool{},
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT tap_device, guest_ip, host_port FROM sandboxes WHERE status=?`, StatusRunning)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var tap, ip string
		var port int
		if err := rows.Scan(&tap, &ip, &port); err != nil {
			return u, err
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

func pickFreeTap(used map[string]bool, p Pools) (string, error) {
	for i := 0; i < p.TapMax; i++ {
		name := fmt.Sprintf("%s%d", p.TapPrefix, i)
		if !used[name] {
			return name, nil
		}
	}
	return "", errors.New("tap pool exhausted")
}

func pickFreeIP(used map[string]bool, p Pools) (string, error) {
	minIP, err := ipToUint32(p.GuestIPMin)
	if err != nil {
		return "", err
	}
	maxIP, err := ipToUint32(p.GuestIPMax)
	if err != nil {
		return "", err
	}
	for n := minIP; n <= maxIP; n++ {
		s := uint32ToIP(n)
		if !used[s] {
			return s, nil
		}
	}
	return "", errors.New("ip pool exhausted")
}

func pickFreePort(used map[int]bool, p Pools) (int, error) {
	for port := p.PortMin; port <= p.PortMax; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, errors.New("port pool exhausted")
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
