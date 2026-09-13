package apiv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/google/uuid"
)

const fanoutChunk = 8

func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	if !h.durableCreatesReady(w, r) {
		return
	}
	var body createRequest
	raw, ok := decodeRawBody(w, r, &body)
	if !ok {
		return
	}
	if err := validateCreate(body); err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return
	}
	h.acceptSingle(w, r, raw, body)
}

func (h *Handler) createSandboxAsync(w http.ResponseWriter, r *http.Request) {
	if !h.durableCreatesReady(w, r) {
		return
	}
	var body createRequest
	raw, ok := decodeRawBody(w, r, &body)
	if !ok {
		return
	}
	if err := validateCreate(body); err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return
	}
	h.acceptCreate(w, r, raw, "sandbox_create", 1, []createops.Member{{ID: uuid.NewString(), Index: 0, Spec: durableSpec(body)}})
}

func (h *Handler) acceptSingle(w http.ResponseWriter, r *http.Request, raw []byte, body createRequest) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		httpapi.WriteProblem(w, r, 400, "idempotency_key_required", "Idempotency-Key is required for durable creates")
		return
	}
	id := uuid.NewString()
	member := createops.Member{ID: uuid.NewString(), Index: 0, Spec: durableSpec(body)}
	pending, _ := json.Marshal(Operation{ID: id, Type: "sandbox_create", Status: "pending", Requested: 1, CreatedAt: time.Now().UTC()})
	sum := sha256.Sum256(raw)
	got, err := h.createOps.Accept(r.Context(), createops.AcceptRequest{ID: id, RequestID: httpapi.RequestID(r), Scope: r.Method + " " + r.URL.Path + " " + key, BodyHash: sum[:], Type: "sandbox_create", MaxParallelism: 1, Members: []createops.Member{member}, ResponseStatus: 202, ResponseBody: pending})
	if errors.Is(err, createops.ErrIdempotencyConflict) {
		httpapi.WriteProblem(w, r, 409, "idempotency_key_reused", "Idempotency-Key was used for a different request")
		return
	}
	if err != nil {
		httpapi.WriteProblem(w, r, 500, "create_operation_store_failed", err.Error())
		return
	}
	if got.Created {
		log.Printf("create operation %s: accepted request_id=%s", got.Operation.ID, got.Operation.RequestID)
	}
	h.signalDispatcher()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		status, final, ready, finalErr := h.createOps.FinalResponse(r.Context(), got.Operation.ID)
		if finalErr != nil {
			httpapi.WriteProblem(w, r, 500, "create_operation_store_failed", finalErr.Error())
			return
		}
		if ready {
			if got.Operation.RequestID != "" {
				w.Header().Set(httpapi.RequestIDHeader, got.Operation.RequestID)
			}
			if !got.Created {
				w.Header().Set("Idempotency-Replayed", "true")
			}
			if status == http.StatusCreated {
				var sb Sandbox
				if json.Unmarshal(final, &sb) == nil {
					w.Header().Set("Location", "/v1/sandboxes/"+url.PathEscape(sb.ID))
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if status >= 400 {
				w.Header().Set("Content-Type", "application/problem+json")
			}
			w.WriteHeader(status)
			_, _ = w.Write(final)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// finishSingle stores the exact public response after Record committed the
// terminal worker outcome. The dispatcher calls it for normal completion and
// for recovery of a crash between Record and this write.
func (h *Handler) finishSingle(ctx context.Context, op createops.Operation) {
	if op.Type != "sandbox_create" || op.CompletedAt == nil || len(op.Members) != 1 || op.Members[0].Outcome == nil {
		return
	}
	out := op.Members[0].Outcome
	status := http.StatusCreated
	var body []byte
	if out.Failure != nil {
		status = out.Failure.Status
		body, _ = json.Marshal(httpapi.Problem{Type: "https://sandbox.dev/problems/" + out.Failure.Code, Title: http.StatusText(status), Status: status, Detail: out.Failure.Detail, Code: out.Failure.Code, RequestID: op.RequestID})
	} else if out.Sandbox != nil {
		body, _ = json.Marshal(publicSandbox(*out.Sandbox))
	} else {
		status = http.StatusServiceUnavailable
		body, _ = json.Marshal(httpapi.Problem{Type: "https://sandbox.dev/problems/batch_item_failed", Title: http.StatusText(status), Status: status, Code: "batch_item_failed", RequestID: op.RequestID})
	}
	if err := h.createOps.SetFinalResponse(ctx, op.ID, status, body); err != nil {
		log.Printf("create operation %s: persist final response: %v", op.ID, err)
	}
}

func (h *Handler) durableCreatesReady(w http.ResponseWriter, r *http.Request) bool {
	if h.createOps != nil && h.executor != nil {
		return true
	}
	httpapi.WriteProblem(w, r, http.StatusServiceUnavailable, "create_operations_unconfigured", "durable create operations are not configured")
	return false
}

func decodeRawBody(w http.ResponseWriter, r *http.Request, target any) ([]byte, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return nil, false
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		httpapi.WriteProblem(w, r, 400, "invalid_request", "request body must contain one JSON value")
		return nil, false
	}
	return raw, true
}

func durableSpec(in createRequest) createops.Spec {
	return createops.Spec{Source: createops.Source{Type: in.Source.Type, ID: in.Source.ID}, Name: in.Name, Resources: func() *createops.Resources {
		if in.Resources == nil || (in.Resources.VCPU == 0 && in.Resources.MemoryMIB == 0) {
			return nil
		}
		return &createops.Resources{VCPU: in.Resources.VCPU, MemoryMIB: in.Resources.MemoryMIB}
	}(), Lifecycle: createops.Lifecycle{TTLSeconds: in.Lifecycle.TTLSeconds, IdleTimeoutSeconds: in.Lifecycle.IdleTimeoutSeconds}, Metadata: in.Metadata}
}

func (h *Handler) acceptCreate(w http.ResponseWriter, r *http.Request, raw []byte, typ string, parallel int, members []createops.Member) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 255 {
		httpapi.WriteProblem(w, r, 400, "idempotency_key_required", "Idempotency-Key is required for durable creates")
		return
	}
	id := uuid.NewString()
	op := Operation{ID: id, Type: typ, Status: "pending", Requested: len(members), CreatedAt: time.Now().UTC()}
	accepted, _ := json.Marshal(op)
	sum := sha256.Sum256(raw)
	got, err := h.createOps.Accept(r.Context(), createops.AcceptRequest{ID: id, RequestID: httpapi.RequestID(r), Scope: r.Method + " " + r.URL.Path + " " + key, BodyHash: sum[:], Type: typ, MaxParallelism: parallel, Members: members, ResponseStatus: http.StatusAccepted, ResponseBody: accepted})
	if errors.Is(err, createops.ErrIdempotencyConflict) {
		httpapi.WriteProblem(w, r, 409, "idempotency_key_reused", "Idempotency-Key was used for a different request")
		return
	}
	if err != nil {
		httpapi.WriteProblem(w, r, 500, "create_operation_store_failed", err.Error())
		return
	}
	if got.Operation.RequestID != "" {
		w.Header().Set(httpapi.RequestIDHeader, got.Operation.RequestID)
	}
	w.Header().Set("Location", "/v1/operations/"+got.Operation.ID)
	if !got.Created {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(got.ResponseStatus)
	_, _ = w.Write(got.ResponseBody)
	if got.Created {
		log.Printf("create operation %s: accepted request_id=%s", got.Operation.ID, got.Operation.RequestID)
		h.signalDispatcher()
	}
}

func (h *Handler) getOperation(w http.ResponseWriter, r *http.Request) {
	if h.createOps == nil {
		httpapi.WriteProblem(w, r, 404, "operation_not_found", "operation not found")
		return
	}
	op, err := h.createOps.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, createops.ErrNotFound) {
		httpapi.WriteProblem(w, r, 404, "operation_not_found", "operation not found")
		return
	}
	if err != nil {
		httpapi.WriteProblem(w, r, 500, "create_operation_store_failed", err.Error())
		return
	}
	if op.Type != "sandbox_batch_create" && op.Type != "sandbox_create" {
		httpapi.WriteProblem(w, r, 404, "operation_not_found", "operation not found")
		return
	}
	writeJSON(w, 200, publicOperation(op))
}

func (h *Handler) listOperations(w http.ResponseWriter, r *http.Request) {
	query := createops.ListQuery{Limit: 50}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			httpapi.WriteProblem(w, r, 400, "invalid_page_size", "page_size must be between 1 and 100")
			return
		}
		query.Limit = limit
	}
	var err error
	query.BeforeID, query.Offset, err = parseOperationPageToken(r.URL.Query().Get("page_token"))
	if err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_page_token", "page_token is invalid")
		return
	}
	if h.createOps == nil {
		if query.BeforeID != "" || query.Offset != 0 {
			httpapi.WriteProblem(w, r, 400, "invalid_page_token", "page_token is invalid")
			return
		}
		writeJSON(w, 200, map[string]any{"operations": []Operation{}, "next_page_token": ""})
		return
	}
	stored, err := h.createOps.List(r.Context(), query)
	if errors.Is(err, createops.ErrInvalidPage) {
		httpapi.WriteProblem(w, r, 400, "invalid_page_token", "page_token is invalid")
		return
	}
	if err != nil {
		httpapi.WriteProblem(w, r, 500, "create_operation_store_failed", err.Error())
		return
	}
	ops := make([]Operation, 0, len(stored.Operations))
	for _, op := range stored.Operations {
		ops = append(ops, publicOperation(op))
	}
	next := ""
	if stored.NextID != "" {
		next = "op1." + base64.RawURLEncoding.EncodeToString([]byte(stored.NextID))
	}
	writeJSON(w, 200, map[string]any{"operations": ops, "next_page_token": next})
}

func parseOperationPageToken(token string) (string, int, error) {
	if strings.HasPrefix(token, "op1.") {
		encoded := strings.TrimPrefix(token, "op1.")
		if len(encoded) == 0 || len(encoded) > base64.RawURLEncoding.EncodedLen(255) {
			return "", 0, createops.ErrInvalidPage
		}
		id, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || len(id) == 0 || len(id) > 255 || base64.RawURLEncoding.EncodeToString(id) != encoded {
			return "", 0, createops.ErrInvalidPage
		}
		return string(id), 0, nil
	}
	offset, err := httpapi.ParseCursor(token)
	return "", offset, err
}

func publicOperation(in createops.Operation) Operation {
	out := Operation{ID: in.ID, RequestID: in.RequestID, Type: in.Type, Status: in.Status, Requested: in.Requested, CreatedAt: in.CreatedAt, CompletedAt: in.CompletedAt, Results: make([]BatchItem, len(in.Members))}
	for i, m := range in.Members {
		out.Results[i].Index = m.Index
		out.Results[i].Progress = publicCreateProgress(m)
		if m.Outcome == nil {
			continue
		}
		if m.Outcome.Failure == nil && m.Outcome.Sandbox != nil {
			out.Succeeded++
			sb := publicSandbox(*m.Outcome.Sandbox)
			out.Results[i].Sandbox = &sb
			continue
		}
		out.Failed++
		f := m.Outcome.Failure
		if f == nil {
			f = &createops.Failure{Status: 503, Code: "batch_item_failed", Detail: "worker returned no result"}
		}
		out.Results[i].Error = &httpapi.Problem{Type: "https://sandbox.dev/problems/" + f.Code, Title: http.StatusText(f.Status), Status: f.Status, Detail: f.Detail, Code: f.Code, RequestID: in.RequestID}
	}
	return out
}

// RunCreateOperations owns recovery. It is intentionally started by the
// process lifecycle, never by request handling, so accepted work survives an
// HTTP handler restart and each operation has one in-process dispatcher.
func (h *Handler) RunCreateOperations(ctx context.Context) {
	if h.createOps == nil || h.executor == nil {
		return
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	defer h.dispatchWG.Wait()
	for {
		h.dispatchDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-tick.C:
		}
	}
}
func (h *Handler) signalDispatcher() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}
func (h *Handler) dispatchDue(ctx context.Context) {
	ops, err := h.createOps.Pending(ctx)
	if err != nil {
		return
	}
	for _, op := range ops {
		if op.CompletedAt != nil {
			h.finishSingle(ctx, op)
			continue
		}
		h.dispatchMu.Lock()
		if h.dispatching[op.ID] {
			h.dispatchMu.Unlock()
			continue
		}
		h.dispatching[op.ID] = true
		h.dispatchMu.Unlock()
		h.dispatchWG.Add(1)
		go func(op createops.Operation) {
			defer h.dispatchWG.Done()
			defer func() { h.dispatchMu.Lock(); delete(h.dispatching, op.ID); h.dispatchMu.Unlock() }()
			h.executeOperation(ctx, op)
		}(op)
	}
}
