package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strconv"
	"time"
)

// The Firecracker MMDS link-local endpoint. A fan-out clone resumes carrying the
// snapshot source's network identity in guest memory; the host pushes the
// clone's fresh identity into MMDS (see internal/vm.StartClone), and this agent
// reads it and reconfigures eth0 so the clone stops impersonating the source.
const (
	mmdsAddr  = "169.254.169.254"
	mmdsIface = "eth0"
)

// cloneIdentity is the document the host writes into MMDS for a clone. A 1:1
// restore keeps its identity (Gen empty/unchanged) but still gets EpochMS so
// the stale guest clock can be stepped.
type cloneIdentity struct {
	IP     string `json:"ip"`
	MAC    string `json:"mac"`
	GW     string `json:"gw"`
	Prefix string `json:"prefix"`
	Gen    string `json:"gen"`
	// EpochMS is the host's wall clock (Unix ms) at resume. A restored guest
	// wakes with its clock frozen at snapshot time; left alone, NTP eventually
	// steps it forward minutes at once, which stalls in-flight timers (both
	// kernel sleeps and Go timers) mid-request. Stepping it here, immediately
	// at thaw, keeps that correction out of user execs.
	EpochMS string `json:"epoch_ms"`
}

// runThawAgent polls MMDS and reconfigures eth0 whenever the identity generation
// changes. On a normally cold-booted sandbox MMDS carries no identity, so this
// loops harmlessly forever doing nothing. On a fan-out clone it fires once, right
// after resume, to adopt the fresh IP/MAC. It runs for the lifetime of sandboxd.
func runThawAgent() {
	client := &http.Client{Timeout: 1 * time.Second}
	var lastGen, lastEpoch string
	for {
		ensureMMDSRoute()
		id, err := fetchIdentity(client)
		if err == nil && id.Gen != "" && id.Gen != lastGen {
			if err := applyIdentity(id); err != nil {
				log.Printf("thaw: apply identity gen=%s failed: %v", id.Gen, err)
			} else {
				log.Printf("thaw: reconfigured %s -> ip=%s mac=%s gen=%s", mmdsIface, id.IP, id.MAC, id.Gen)
				lastGen = id.Gen
				// Tell the host we shed the baked identity, so it can bridge
				// the tap now instead of sleeping a fixed margin.
				if err := announceIdentity(mmdsIface, id.IP, id.MAC); err != nil {
					log.Printf("thaw: garp announce failed: %v", err)
				}
			}
		}
		if err == nil && id.EpochMS != "" && id.EpochMS != lastEpoch {
			if err := applyClock(id.EpochMS); err != nil {
				log.Printf("thaw: set clock failed: %v", err)
			} else {
				log.Printf("thaw: stepped clock to epoch_ms=%s", id.EpochMS)
				lastEpoch = id.EpochMS
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// applyClock steps CLOCK_REALTIME to the host-provided epoch (Unix ms).
func applyClock(epochMS string) error {
	ms, err := strconv.ParseInt(epochMS, 10, 64)
	if err != nil {
		return fmt.Errorf("bad epoch_ms %q: %w", epochMS, err)
	}
	arg := fmt.Sprintf("@%d.%03d", ms/1000, ms%1000)
	if out, err := exec.Command("date", "-u", "-s", arg).CombinedOutput(); err != nil {
		return fmt.Errorf("date -s %s: %w: %s", arg, err, out)
	}
	return nil
}

// fetchIdentity reads the clone identity from MMDS (V2: token, then JSON GET).
func fetchIdentity(client *http.Client) (cloneIdentity, error) {
	var id cloneIdentity
	tokReq, _ := http.NewRequest(http.MethodPut, "http://"+mmdsAddr+"/latest/api/token", nil)
	tokReq.Header.Set("X-metadata-token-ttl-seconds", "60")
	tokResp, err := client.Do(tokReq)
	if err != nil {
		return id, err
	}
	token, _ := io.ReadAll(tokResp.Body)
	tokResp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://"+mmdsAddr+"/", nil)
	req.Header.Set("X-metadata-token", string(token))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return id, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return id, nil // no identity yet
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return id, err
	}
	_ = json.Unmarshal(body, &id) // absent fields stay empty → Gen "" → skip
	return id, nil
}

// applyIdentity rewrites eth0's MAC + IP + default route to the clone's identity.
func applyIdentity(id cloneIdentity) error {
	prefix := id.Prefix
	if prefix == "" {
		prefix = "24"
	}
	steps := [][]string{
		{"ip", "link", "set", mmdsIface, "down"},
		{"ip", "addr", "flush", "dev", mmdsIface},
		{"ip", "link", "set", mmdsIface, "address", id.MAC},
		{"ip", "link", "set", mmdsIface, "up"},
		{"ip", "addr", "add", id.IP + "/" + prefix, "dev", mmdsIface},
		{"ip", "route", "replace", "default", "via", id.GW},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return &ipError{args: args, out: string(out), err: err}
		}
	}
	ensureMMDSRoute()
	return nil
}

// ensureMMDSRoute makes sure the link-local MMDS address is routed via eth0
// (kernel-configured guests don't get this route automatically).
func ensureMMDSRoute() {
	// `ip route add` is idempotent enough for our purpose; ignore "File exists".
	_ = exec.Command("ip", "route", "add", mmdsAddr+"/32", "dev", mmdsIface).Run()
}

type ipError struct {
	args []string
	out  string
	err  error
}

func (e *ipError) Error() string {
	return e.err.Error() + ": " + e.out
}
