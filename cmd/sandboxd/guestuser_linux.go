//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureGuestCommandProcess(cmd *exec.Cmd) error {
	acct, err := lookupGuestAccount()
	if err != nil {
		return err
	}
	switch uid := uint32(os.Geteuid()); uid {
	case 0:
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: acct.uid,
			Gid: acct.gid,
		}
	case acct.uid:
		// Already running as the intended account.
	default:
		return fmt.Errorf("sandboxd euid %d cannot run commands as uid %d", uid, acct.uid)
	}
	return nil
}

// withGuestFilesystem applies the sandbox user's filesystem UID/GID to the
// current kernel thread for exactly one open/create/list operation. File
// descriptors opened under that identity remain usable after restoring root,
// while path permission checks cannot bypass the guest user's permissions.
func withGuestFilesystem(fn func() error) error {
	acct, err := lookupGuestAccount()
	if err != nil {
		return err
	}
	if uint32(os.Geteuid()) == acct.uid {
		return fn()
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("sandboxd euid %d cannot assume filesystem uid %d", os.Geteuid(), acct.uid)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	oldGID, err := unix.SetfsgidRetGid(int(acct.gid))
	if err != nil {
		return fmt.Errorf("setfsgid %d: %w", acct.gid, err)
	}
	defer func() { _ = unix.Setfsgid(oldGID) }()
	if current, _ := unix.SetfsgidRetGid(-1); current != int(acct.gid) {
		return fmt.Errorf("setfsgid %d did not take effect (still %d)", acct.gid, current)
	}

	oldUID, err := unix.SetfsuidRetUid(int(acct.uid))
	if err != nil {
		_ = unix.Setfsgid(oldGID)
		return fmt.Errorf("setfsuid %d: %w", acct.uid, err)
	}
	defer func() { _ = unix.Setfsuid(oldUID) }()
	if current, _ := unix.SetfsuidRetUid(-1); current != int(acct.uid) {
		return fmt.Errorf("setfsuid %d did not take effect (still %d)", acct.uid, current)
	}

	return fn()
}
