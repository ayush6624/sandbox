// Package gcemig provides the small subset of the GCE Managed Instance Group
// API needed by the gateway's queue-triggered fast scale-out path.
package gcemig

import (
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
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
