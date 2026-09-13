package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type CreateRequest struct {
	Name      string            `json:"name,omitempty"`
	Source    CreateSource      `json:"source"`
	Lifecycle CreateLifecycle   `json:"lifecycle"`
	Resources *CreateResources  `json:"resources,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}
type CreateSource struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}
type CreateLifecycle struct {
	TTLSeconds         int `json:"ttl_seconds,omitempty"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds,omitempty"`
}
type CreateResources struct {
	VCPU      int64 `json:"vcpu"`
	MemoryMIB int64 `json:"memory_mib"`
}
type Operation struct {
	ID          string            `json:"id"`
	RequestID   string            `json:"request_id,omitempty"`
	Type        string            `json:"type"`
	Status      string            `json:"status"`
	Requested   int               `json:"requested"`
	Succeeded   int               `json:"succeeded"`
	Failed      int               `json:"failed"`
	Results     []OperationResult `json:"results,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
}
type OperationResult struct {
	Index    int             `json:"index"`
	Progress *CreateProgress `json:"progress,omitempty"`
	Sandbox  json.RawMessage `json:"sandbox,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}
type CreateProgress struct {
	Coordination CreateCoordination    `json:"coordination"`
	Worker       *CreateWorkerProgress `json:"worker,omitempty"`
}
type CreateCoordination struct {
	Phase     string     `json:"phase"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}
type CreateWorkerProgress struct {
	Attempt       int64        `json:"attempt"`
	Sequence      int64        `json:"sequence"`
	Condition     string       `json:"condition"`
	Current       CreateStage  `json:"current"`
	LastCompleted *CreateStage `json:"last_completed,omitempty"`
	ObservedAt    time.Time    `json:"observed_at"`
}
type CreateStage struct {
	Stage       string     `json:"stage"`
	Attempt     int64      `json:"attempt"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (o Operation) Terminal() bool {
	return o.CompletedAt != nil
}

// CreateAcceptanceError retains the key needed to replay an uncertain acceptance.
type CreateAcceptanceError struct {
	IdempotencyKey string
	Cause          error
}

func (e *CreateAcceptanceError) Error() string {
	return fmt.Sprintf("create acceptance unknown; replay the same options with --idempotency-key %s: %v", e.IdempotencyKey, e.Cause)
}
func (e *CreateAcceptanceError) Unwrap() error { return e.Cause }

func (c *Client) CreateAsync(ctx context.Context, key string, body CreateRequest) (Operation, error) {
	if strings.TrimSpace(key) == "" {
		return Operation{}, fmt.Errorf("idempotency key must be nonempty")
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Operation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/sandbox-creations", bytes.NewReader(b))
	if err != nil {
		return Operation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	uncertain := func(err error) (Operation, error) { return Operation{}, &CreateAcceptanceError{key, err} }
	// Redirects can change the method or replay a mutation at a different route.
	httpClient := *c.http
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := httpClient.Do(req)
	if err != nil {
		return uncertain(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return uncertain(apiError(resp))
	}
	if resp.StatusCode >= 400 {
		return Operation{}, apiError(resp)
	}
	if resp.StatusCode != http.StatusAccepted {
		return uncertain(fmt.Errorf("expected HTTP 202 operation receipt, got HTTP %d", resp.StatusCode))
	}
	var op Operation
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return uncertain(err)
	}
	if err := json.Unmarshal(data, &op); err != nil {
		return uncertain(err)
	}
	if strings.TrimSpace(op.ID) == "" || op.Type != "sandbox_create" || op.Status != "pending" || op.Requested != 1 || op.Succeeded != 0 || op.Failed != 0 || len(op.Results) != 0 || op.CreatedAt.IsZero() || op.CompletedAt != nil {
		return uncertain(fmt.Errorf("invalid single-create operation receipt"))
	}
	return op, nil
}
func (c *Client) GetOperation(ctx context.Context, id string) (Operation, error) {
	var op Operation
	err := c.do(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(id), nil, &op)
	if err == nil && (op.ID != id || (op.Type != "sandbox_create" && op.Type != "sandbox_batch_create") || op.Requested < 1) {
		err = fmt.Errorf("invalid operation response for %s", id)
	}
	return op, err
}
func (c *Client) WaitOperation(ctx context.Context, id string, interval time.Duration, observe func(Operation)) (Operation, error) {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	for {
		op, err := c.GetOperation(ctx, id)
		if err != nil {
			return op, err
		}
		if observe != nil {
			observe(op)
		}
		if op.Terminal() {
			return op, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return op, ctx.Err()
		case <-timer.C:
		}
	}
}
