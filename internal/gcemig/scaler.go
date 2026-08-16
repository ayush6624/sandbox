// Package gcemig provides the small subset of the GCE Managed Instance Group
// API needed by the gateway's queue-triggered fast scale-out path.
package gcemig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	computeAPIBase   = "https://compute.googleapis.com/compute/v1"
	metadataTokenURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
)

// Scaler grows one zonal MIG using the control VM's attached service account.
// ScaleOut never shrinks the group: it reads targetSize first and only submits a
// resize when desired is larger.
type Scaler struct {
	project string
	zone    string
	mig     string
	max     int

	client   *http.Client
	apiBase  string
	tokenURL string

	tokenMu     sync.Mutex
	accessToken string
	tokenExpiry time.Time
}

// New returns a scaler for a zonal managed instance group.
func New(project, zone, mig string, max int) (*Scaler, error) {
	if project == "" || zone == "" || mig == "" {
		return nil, fmt.Errorf("GCE direct scale-out requires project, zone, and MIG name")
	}
	if max <= 0 {
		return nil, fmt.Errorf("GCE direct scale-out max must be positive")
	}
	return &Scaler{
		project:  project,
		zone:     zone,
		mig:      mig,
		max:      max,
		client:   &http.Client{Timeout: 5 * time.Second},
		apiBase:  computeAPIBase,
		tokenURL: metadataTokenURL,
	}, nil
}

func (s *Scaler) endpoint() string {
	return fmt.Sprintf("%s/projects/%s/zones/%s/instanceGroupManagers/%s",
		strings.TrimRight(s.apiBase, "/"),
		url.PathEscape(s.project), url.PathEscape(s.zone), url.PathEscape(s.mig))
}

// TargetSize reads the MIG's current target size. This is the authority on how
// many workers the group has been told to run — unlike a heartbeat-derived
// count, which also sees resumed standby instances that are not part of the
// target. The autoscaler's scale-in ceiling is built from this.
func (s *Scaler) TargetSize(ctx context.Context) (int, error) {
	token, err := s.token(ctx)
	if err != nil {
		return 0, err
	}
	var state struct {
		TargetSize int `json:"targetSize"`
	}
	if err := s.doJSON(ctx, http.MethodGet, s.endpoint(), token, &state); err != nil {
		return 0, fmt.Errorf("read MIG target size: %w", err)
	}
	return state.TargetSize, nil
}

// ScaleOut grows the MIG to desired, capped at the configured maximum. It is a
// no-op when the MIG target is already at or above desired.
func (s *Scaler) ScaleOut(ctx context.Context, desired int) error {
	if desired > s.max {
		desired = s.max
	}
	if desired <= 0 {
		return nil
	}

	current, err := s.TargetSize(ctx)
	if err != nil {
		return err
	}
	if current >= desired {
		return nil
	}

	token, err := s.token(ctx)
	if err != nil {
		return err
	}
	resizeURL := s.endpoint() + "/resize?size=" + strconv.Itoa(desired)
	if err := s.doJSON(ctx, http.MethodPost, resizeURL, token, nil); err != nil {
		return fmt.Errorf("resize MIG from %d to %d: %w", current, desired, err)
	}
	return nil
}

// DeleteInstance removes ONE named instance from the group and decrements its
// target size in the same operation.
//
// This is deliberately not a resize-down. `resize` to N-1 lets GCE choose the
// victim by its own ordering, which has no relationship to which host the
// gateway drained — so a resize would delete a host full of live sandboxes and
// leave the empty one running. deleteInstances is the only call that removes a
// SPECIFIC member, and it is what makes cordon-then-drain meaningful.
//
// The caller owns the floor: this refuses to act only on an empty name. Going
// below a minimum is a policy decision the gateway makes with knowledge of
// demand, and duplicating it here would let the two disagree.
func (s *Scaler) DeleteInstance(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("delete MIG instance: empty instance name")
	}
	token, err := s.token(ctx)
	if err != nil {
		return err
	}
	// A fully qualified instance URL is accepted, but the zonal short form is
	// what the MIG reports and is unambiguous within the group's zone.
	body := struct {
		Instances []string `json:"instances"`
	}{Instances: []string{fmt.Sprintf("zones/%s/instances/%s", s.zone, name)}}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := s.doJSONBody(ctx, http.MethodPost, s.endpoint()+"/deleteInstances", token, raw, nil); err != nil {
		return fmt.Errorf("delete MIG instance %s: %w", name, err)
	}
	return nil
}

func (s *Scaler) token(ctx context.Context) (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.accessToken != "" && time.Until(s.tokenExpiry) > time.Minute {
		return s.accessToken, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.tokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("metadata token: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", responseError(resp)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode metadata token: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("metadata token response omitted access_token")
	}
	s.accessToken = body.AccessToken
	s.tokenExpiry = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return s.accessToken, nil
}

func (s *Scaler) doJSON(ctx context.Context, method, endpoint, token string, dst any) error {
	return s.doJSONBody(ctx, method, endpoint, token, nil, dst)
}

func (s *Scaler) doJSONBody(ctx context.Context, method, endpoint, token string, body []byte, dst any) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return responseError(resp)
	}
	if dst != nil {
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
