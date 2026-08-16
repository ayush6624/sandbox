package main

import (
	"fmt"

	"github.com/ayush6624/sandbox/internal/agentapi"
	"os/user"
	"strconv"
	"strings"
	"sync"
)

const (
	guestUsername = agentapi.GuestUser
	guestHome     = "/home/sandbox"
)

type guestAccount struct {
	uid uint32
	gid uint32
}

var (
	guestAccountOnce  sync.Once
	guestAccountValue guestAccount
	guestAccountErr   error
)

func lookupGuestAccount() (guestAccount, error) {
	guestAccountOnce.Do(func() {
		u, err := user.Lookup(guestUsername)
		if err != nil {
			guestAccountErr = fmt.Errorf("lookup %s: %w", guestUsername, err)
			return
		}
		uid, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			guestAccountErr = fmt.Errorf("parse %s uid %q: %w", guestUsername, u.Uid, err)
			return
		}
		gid, err := strconv.ParseUint(u.Gid, 10, 32)
		if err != nil {
			guestAccountErr = fmt.Errorf("parse %s gid %q: %w", guestUsername, u.Gid, err)
			return
		}
		guestAccountValue = guestAccount{uid: uint32(uid), gid: uint32(gid)}
	})
	return guestAccountValue, guestAccountErr
}

func guestEnvironment(base []string) []string {
	out := make([]string, 0, len(base)+3)
	for _, item := range base {
		name, _, _ := strings.Cut(item, "=")
		switch name {
		case "HOME", "USER", "LOGNAME":
			continue
		default:
			out = append(out, item)
		}
	}
	return append(out,
		"HOME="+guestHome,
		"USER="+guestUsername,
		"LOGNAME="+guestUsername,
	)
}
