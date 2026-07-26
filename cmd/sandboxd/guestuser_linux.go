//go:build linux

package main

import (
	"errors"
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
	if current, _ := unix.SetfsgidRetGid(-1); current != int(acct.gid) {
		_ = unix.Setfsgid(oldGID)
		return fmt.Errorf("setfsgid %d did not take effect (still %d)", acct.gid, current)
	}

	oldUID, err := unix.SetfsuidRetUid(int(acct.uid))
	if err != nil {
		_ = unix.Setfsgid(oldGID)
		return fmt.Errorf("setfsuid %d: %w", acct.uid, err)
	}
	if current, _ := unix.SetfsuidRetUid(-1); current != int(acct.uid) {
		_ = unix.Setfsuid(oldUID)
		_ = unix.Setfsgid(oldGID)
		return fmt.Errorf("setfsuid %d did not take effect (still %d)", acct.uid, current)
	}

	opErr := fn()
	var restoreErr error
	if err := unix.Setfsuid(oldUID); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore fsuid %d: %w", oldUID, err))
	} else if current, _ := unix.SetfsuidRetUid(-1); current != oldUID {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore fsuid %d did not take effect (still %d)", oldUID, current))
	}
	if err := unix.Setfsgid(oldGID); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore fsgid %d: %w", oldGID, err))
	} else if current, _ := unix.SetfsgidRetGid(-1); current != oldGID {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("restore fsgid %d did not take effect (still %d)", oldGID, current))
	}
	return errors.Join(opErr, restoreErr)
}
