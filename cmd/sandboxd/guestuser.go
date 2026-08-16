package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

const (
	defaultGuestUser = agentapi.GuestUser
	defaultGuestHome = "/home/sandbox"
	defaultGuestCwd  = "/home/sandbox/app"
)

// A template built from a container image carries the identity the image
// declares (see agentapi.GuestProfilePath). Everything else — the base rootfs,
// every snapshot derived from it — has no such file and keeps the defaults.
var (
	guestProfileOnce  sync.Once
	guestProfileValue agentapi.GuestProfile
)

func guestProfile() agentapi.GuestProfile {
	guestProfileOnce.Do(func() {
		p := agentapi.GuestProfile{User: defaultGuestUser, Home: defaultGuestHome, Cwd: defaultGuestCwd}
		if raw, err := os.ReadFile(agentapi.GuestProfilePath); err == nil {
			var fromImage agentapi.GuestProfile
			if err := json.Unmarshal(raw, &fromImage); err != nil {
				log.Printf("guest profile: %v (using defaults)", err)
			} else {
				if fromImage.User != "" {
					p.User = fromImage.User
					// The image's user owns its own home; only fall back to the
					// default home when the profile does not name one.
					p.Home = "/"
				}
				if fromImage.Home != "" {
					p.Home = fromImage.Home
				}
				if fromImage.Cwd != "" {
					p.Cwd = fromImage.Cwd
				}
			}
		}
		guestProfileValue = p
	})
	return guestProfileValue
}

func guestUsername() string { return guestProfile().User }

func guestHome() string { return guestProfile().Home }

// guestCwd is where an exec with no explicit cwd runs.
func guestCwd() string { return guestProfile().Cwd }

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
		u, err := user.Lookup(guestUsername())
		if err != nil {
			guestAccountErr = fmt.Errorf("lookup %s: %w", guestUsername(), err)
			return
		}
		uid, err := strconv.ParseUint(u.Uid, 10, 32)
		if err != nil {
			guestAccountErr = fmt.Errorf("parse %s uid %q: %w", guestUsername(), u.Uid, err)
			return
		}
		gid, err := strconv.ParseUint(u.Gid, 10, 32)
		if err != nil {
			guestAccountErr = fmt.Errorf("parse %s gid %q: %w", guestUsername(), u.Gid, err)
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
		"HOME="+guestHome(),
		"USER="+guestUsername(),
		"LOGNAME="+guestUsername(),
	)
}
