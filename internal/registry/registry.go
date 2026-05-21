package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
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
		stopped_at  INTEGER
	);
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_tap_running  ON sandboxes(tap_device) WHERE status = 'running';
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_ip_running   ON sandboxes(guest_ip)   WHERE status = 'running';
	CREATE UNIQUE INDEX IF NOT EXISTS uniq_port_running ON sandboxes(host_port)  WHERE status = 'running';
	`
	_, err := r.db.Exec(schema)
	return err
}

// Create allocates a tap/IP/port from the pools and inserts a 'running' row
// for the new sandbox. PID/VMID/SocketPath are filled in later via FinishStart
// once firecracker is up.
func (r *Registry) Create(ctx context.Context, id, rootfsPath string) (Sandbox, error) {
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
		`INSERT INTO sandboxes (id, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at)
		 VALUES (?, 0, '', '', ?, ?, ?, ?, ?, ?)`,
		id, tap, ip, port, rootfsPath, StatusRunning, now.Unix())
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

// Destroy removes a sandbox row outright.
func (r *Registry) Destroy(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM sandboxes WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("sandbox %s not found", id)
	}
	return nil
}

// Get returns the sandbox row for the given ID.
func (r *Registry) Get(ctx context.Context, id string) (Sandbox, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT id, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, stopped_at
		 FROM sandboxes WHERE id=?`, id)
	return scanSandbox(row)
}

// List returns all running sandboxes (most recent first).
func (r *Registry) List(ctx context.Context) ([]Sandbox, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, pid, vm_id, socket_path, tap_device, guest_ip, host_port, rootfs_path, status, created_at, stopped_at
		 FROM sandboxes WHERE status=? ORDER BY created_at DESC`, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSandbox(r rowScanner) (Sandbox, error) {
	var sb Sandbox
	var createdAt int64
	var stoppedAt sql.NullInt64
	err := r.Scan(&sb.ID, &sb.PID, &sb.VMID, &sb.SocketPath, &sb.TapDevice, &sb.GuestIP, &sb.HostPort, &sb.RootfsPath, &sb.Status, &createdAt, &stoppedAt)
	if err != nil {
		return sb, err
	}
	sb.CreatedAt = time.Unix(createdAt, 0)
	if stoppedAt.Valid {
		t := time.Unix(stoppedAt.Int64, 0)
		sb.StoppedAt = &t
	}
	return sb, nil
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
	return u, rows.Err()
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
