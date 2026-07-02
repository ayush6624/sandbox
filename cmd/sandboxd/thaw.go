package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os/exec"
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

// cloneIdentity is the document the host writes into MMDS for a clone.
type cloneIdentity struct {
	IP     string `json:"ip"`
	MAC    string `json:"mac"`
	GW     string `json:"gw"`
	Prefix string `json:"prefix"`
	Gen    string `json:"gen"`
}

// runThawAgent polls MMDS and reconfigures eth0 whenever the identity generation
// changes. On a normally cold-booted sandbox MMDS carries no identity, so this
// loops harmlessly forever doing nothing. On a fan-out clone it fires once, right
// after resume, to adopt the fresh IP/MAC. It runs for the lifetime of sandboxd.
func runThawAgent() {
	client := &http.Client{Timeout: 1 * time.Second}
	var lastGen string
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
		time.Sleep(200 * time.Millisecond)
	}
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
