package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

// The Firecracker MMDS link-local endpoint. A fan-out clone resumes carrying the
// snapshot source's network identity in guest memory; the host pushes the
// clone's fresh identity into MMDS (see internal/vm.StartClone), and this agent
// reads it and reconfigures eth0 so the clone stops impersonating the source.
const (
	mmdsAddr        = "169.254.169.254"
	mmdsIface       = "eth0"
	normalPollDelay = 200 * time.Millisecond
	armedPollDelay  = 5 * time.Millisecond
)

var (
	snapshotPollArmed atomic.Bool
	thawPollWake      = make(chan struct{}, 1)
	thawPollReady     = make(chan struct{}, 1)
	thawPollReadyWait = 500 * time.Millisecond
	runIPBatch        = func(batch string) ([]byte, error) {
		cmd := exec.Command("ip", "-batch", "-")
		cmd.Stdin = strings.NewReader(batch)
		return cmd.CombinedOutput()
	}
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
		id, err := fetchIdentity(client)
		if err != nil {
			// A cold boot may not have the link-local route yet. Snapshot
			// restores inherit it, so keep this process spawn off their hot path.
			ensureMMDSRoute()
			id, err = fetchIdentity(client)
		}
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
				snapshotPollArmed.Store(false)
			}
		}
		if err == nil && id.EpochMS != "" && id.EpochMS != lastEpoch {
			if err := applyClock(id.EpochMS); err != nil {
				log.Printf("thaw: set clock failed: %v", err)
			} else {
				log.Printf("thaw: stepped clock to epoch_ms=%s", id.EpochMS)
				lastEpoch = id.EpochMS
				// Same-identity restores have no new generation, but a new
				// epoch still proves the armed snapshot has resumed.
				snapshotPollArmed.Store(false)
			}
		}
		waitForThawPoll()
	}
}

func handleSnapshotPoll(w http.ResponseWriter, r *http.Request) {
	var req agentapi.SnapshotPollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Armed {
		for {
			select {
			case <-thawPollReady:
			default:
				goto readyDrained
			}
		}
	}
readyDrained:
	snapshotPollArmed.Store(req.Armed)
	select {
	case thawPollWake <- struct{}{}:
	default:
	}
	if req.Armed {
		select {
		case <-thawPollReady:
		case <-time.After(thawPollReadyWait):
			log.Printf("snapshot-poll: thaw loop did not acknowledge fast polling within %s", thawPollReadyWait)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"armed": req.Armed})
}

func waitForThawPoll() {
	delay := normalPollDelay
	if snapshotPollArmed.Load() {
		delay = armedPollDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	// Acknowledging only after the armed timer exists closes the race where the
	// host could pause immediately after the loop announced readiness but
	// before it had actually left the old 200 ms wait.
	if snapshotPollArmed.Load() {
		select {
		case thawPollReady <- struct{}{}:
		default:
		}
	}
	select {
	case <-timer.C:
	case <-thawPollWake:
	}
}

// applyClock steps CLOCK_REALTIME to the host-provided epoch (Unix ms).
func applyClock(epochMS string) error {
	ms, err := strconv.ParseInt(epochMS, 10, 64)
	if err != nil {
		return fmt.Errorf("bad epoch_ms %q: %w", epochMS, err)
	}
	return setClockRealtime(ms * int64(time.Millisecond))
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
	if net.ParseIP(id.IP).To4() == nil {
		return fmt.Errorf("bad IPv4 %q", id.IP)
	}
	if net.ParseIP(id.GW).To4() == nil {
		return fmt.Errorf("bad gateway IPv4 %q", id.GW)
	}
	if mac, err := net.ParseMAC(id.MAC); err != nil || len(mac) != 6 {
		return fmt.Errorf("bad MAC %q", id.MAC)
	}
	bits, err := strconv.Atoi(prefix)
	if err != nil || bits < 1 || bits > 32 {
		return fmt.Errorf("bad IPv4 prefix %q", prefix)
	}

	// A template built from a container image need not contain iproute2, and a
	// clone that cannot reconfigure eth0 is unreachable. Do it over netlink
	// instead (netlink_linux.go); guests that have `ip` keep the batch below.
	if !haveIPCommand() {
		return applyIdentityNetlink(mmdsIface, id.IP, id.MAC, id.GW, bits)
	}

	// One iproute2 batch replaces eight short-lived `ip` processes on every
	// clone. Besides saving process startup, the commands remain ordered and
	// the tap is still unbridged until the GARP acknowledgement.
	batch := strings.Join([]string{
		"link set dev " + mmdsIface + " down",
		"addr flush dev " + mmdsIface,
		"link set dev " + mmdsIface + " address " + id.MAC,
		"link set dev " + mmdsIface + " up",
		"addr add " + id.IP + "/" + prefix + " dev " + mmdsIface,
		"route replace default via " + id.GW,
		"route replace " + mmdsAddr + "/32 dev " + mmdsIface,
	}, "\n") + "\n"
	if out, err := runIPBatch(batch); err != nil {
		return fmt.Errorf("ip batch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureMMDSRoute makes sure the link-local MMDS address is routed via eth0
// (kernel-configured guests don't get this route automatically).
func ensureMMDSRoute() {
	if !haveIPCommand() {
		_ = mmdsRouteNetlink(nil)
		return
	}
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
