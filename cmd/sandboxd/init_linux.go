//go:build linux

package main

// sandboxd doubles as PID 1 for guests built from a container image
// (`sandbox template build`). Such a rootfs has no init system — the image's
// ENTRYPOINT is a container contract, not a boot contract — so the template
// build boots the kernel with `init=/usr/local/bin/sandboxd`, and this file is
// what that first process does: mount the pseudo-filesystems a normal Linux
// userland assumes, then supervise the agent.
//
// PID 1 does NOT serve the API itself. Orphaned grandchildren reparent to PID 1
// and must be reaped there, but a generic wait4(-1) reaper in the same process
// as the agent would steal exit statuses from os/exec's own wait and turn every
// exec into "wait: no child processes". Splitting them is the standard fix
// (tini and friends do the same): the supervisor reaps anything that lands on
// it, the agent only ever reaps its own children.

import (
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

// initEnv marks the supervised agent process, so it can tell it is running in
// a template guest (no systemd) rather than the systemd-managed base image.
const initEnv = "SANDBOXD_INIT"

const agentPath = agentapi.AgentPath

// defaultPath matches what a service manager would hand a system daemon, and
// includes the sbin directories because `ip` lives in /usr/sbin.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func isInit() bool { return os.Getpid() == 1 }

func initMode() bool { return os.Getenv(initEnv) == "1" }

// runInit is the PID 1 entry point. It never returns: a PID 1 that exits
// panics the kernel.
func runInit() {
	log.SetPrefix("init: ")
	log.SetFlags(0)
	mountGuestFilesystems()

	// The kernel hands init an almost empty environment — notably no PATH — and
	// that is what the agent inherits. Everything it shells out to by bare name
	// then fails to resolve, and the one that matters is `ip`: without it a
	// clone cannot adopt its new network identity, so it resumes still holding
	// the snapshot's address and is simply never reachable. (systemd sets this
	// for the base image, which is why only template guests hit it.)
	if os.Getenv("PATH") == "" {
		if err := os.Setenv("PATH", defaultPath); err != nil {
			log.Printf("set PATH: %v", err)
		}
	}

	if host, err := os.ReadFile("/etc/hostname"); err == nil {
		if name := strings.TrimSpace(string(host)); name != "" {
			if err := unix.Sethostname([]byte(name)); err != nil {
				log.Printf("sethostname %q: %v", name, err)
			}
		}
	}

	// Firecracker's ACPI shutdown (used by destroy) arrives as ctrl-alt-del,
	// which the kernel turns into an immediate restart unless init asks for the
	// signal instead. Take the signal so the rootfs is flushed before the reset.
	if err := unix.Reboot(unix.LINUX_REBOOT_CMD_CAD_OFF); err != nil {
		log.Printf("cad off: %v", err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Print("shutdown requested; syncing")
		unix.Sync()
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
	}()

	superviseAgent()
}

// guestMount is one pseudo-filesystem the agent, sshd, and ordinary guest
// workloads assume exists. A container image ships none of them mounted.
type guestMount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
	optional               bool
}

// /proc is first and mandatory: resolv.conf materialization reads
// /proc/net/pnp, sshd readiness reads /proc/net/tcp, and os.Executable reads
// /proc/self/exe. devpts is what makes the /shell pty work.
var guestMounts = []guestMount{
	{source: "proc", target: "/proc", fstype: "proc", flags: unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV},
	{source: "sysfs", target: "/sys", fstype: "sysfs", flags: unix.MS_NOSUID | unix.MS_NOEXEC | unix.MS_NODEV},
	// The kernel may already have mounted devtmpfs (CONFIG_DEVTMPFS_MOUNT);
	// then this returns EBUSY and the existing mount is what we wanted anyway.
	{source: "devtmpfs", target: "/dev", fstype: "devtmpfs", flags: unix.MS_NOSUID, data: "mode=755", optional: true},
	{source: "devpts", target: "/dev/pts", fstype: "devpts", flags: unix.MS_NOSUID | unix.MS_NOEXEC, data: "gid=5,mode=620,ptmxmode=666"},
	{source: "tmpfs", target: "/dev/shm", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=1777"},
	{source: "tmpfs", target: "/run", fstype: "tmpfs", flags: unix.MS_NOSUID | unix.MS_NODEV, data: "mode=755"},
}

func mountGuestFilesystems() {
	for _, m := range guestMounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			log.Printf("mkdir %s: %v", m.target, err)
		}
		err := unix.Mount(m.source, m.target, m.fstype, m.flags, m.data)
		switch {
		case err == nil, err == unix.EBUSY:
		case m.optional:
			log.Printf("mount %s (optional): %v", m.target, err)
		default:
			log.Printf("mount %s: %v", m.target, err)
		}
	}
}

// superviseAgent runs the agent as a child and reaps everything that dies under
// PID 1 forever. The agent is restarted if it exits, matching the
// Restart=on-failure the systemd unit gives it in the base image.
func superviseAgent() {
	exe, err := os.Executable()
	if err != nil {
		exe = agentPath
	}
	agent := startAgent(exe)
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		switch {
		case err == syscall.EINTR:
			continue
		case err != nil:
			// ECHILD: the agent failed to start. Back off and retry rather than
			// spinning — and never return, PID 1 exiting is a kernel panic.
			time.Sleep(time.Second)
			if agent <= 0 {
				agent = startAgent(exe)
			}
			continue
		}
		if pid == agent {
			log.Printf("agent exited (%v); restarting", ws)
			time.Sleep(time.Second)
			agent = startAgent(exe)
		}
	}
}

func startAgent(exe string) int {
	proc, err := os.StartProcess(exe, []string{exe}, &os.ProcAttr{
		Env:   append(os.Environ(), initEnv+"=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		log.Printf("start agent %s: %v", exe, err)
		return -1
	}
	// Deliberately no Wait: superviseAgent's wait4(-1) reaps this child along
	// with every orphan.
	return proc.Pid
}
