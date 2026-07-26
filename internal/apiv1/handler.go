// Package apiv1 adapts the established worker/gateway handlers to the stable
// public v1 resource contract. Runtime implementation details stay behind the
// adapter while legacy clients continue to use their existing routes.
package apiv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

type Handler struct {
	legacy http.Handler
	idem   *httpapi.Store

	mu         sync.RWMutex
	operations map[string]*Operation
}

func New(legacy http.Handler) *Handler {
	return &Handler{
		legacy:     legacy,
		idem:       httpapi.NewStore(24 * time.Hour),
		operations: make(map[string]*Operation),
	}
}

func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sandboxes", h.listSandboxes)
	mux.Handle("POST /v1/sandboxes", h.idem.Wrap(http.HandlerFunc(h.createSandbox)))
	mux.HandleFunc("GET /v1/sandboxes/{id}", h.getSandbox)
	mux.Handle("PATCH /v1/sandboxes/{id}", h.idem.Wrap(http.HandlerFunc(h.updateSandbox)))
	mux.Handle("DELETE /v1/sandboxes/{id}", h.idem.Wrap(http.HandlerFunc(h.deleteSandbox)))
	mux.Handle("POST /v1/sandboxes/{action}", h.idem.Wrap(http.HandlerFunc(h.sandboxAction)))
	mux.Handle("POST /v1/sandboxes/{id}/snapshots", h.idem.Wrap(http.HandlerFunc(h.createSnapshot)))
	mux.HandleFunc("GET /v1/snapshots", h.listSnapshots)
	mux.HandleFunc("GET /v1/snapshots/{id}", h.getSnapshot)
	mux.Handle("DELETE /v1/snapshots/{id}", h.idem.Wrap(http.HandlerFunc(h.deleteSnapshot)))
	mux.HandleFunc("GET /v1/templates", h.listTemplates)
	mux.HandleFunc("GET /v1/templates/{id}", h.getTemplate)
	mux.Handle("POST /v1/sandbox-batches", h.idem.Wrap(http.HandlerFunc(h.createBatch)))
	mux.HandleFunc("GET /v1/operations", h.listOperations)
	mux.HandleFunc("GET /v1/operations/{id}", h.getOperation)
	mux.Handle("POST /v1/sandboxes/{id}/port-forwards", h.idem.Wrap(http.HandlerFunc(h.createPortForward)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/port-forwards", h.listPortForwards)
}

func (h *Handler) sandboxAction(w http.ResponseWriter, r *http.Request) {
	value := r.PathValue("action")
	id, action, ok := strings.Cut(value, ":")
	if !ok || id == "" {
		httpapi.WriteProblem(w, r, 404, "not_found", "resource action not found")
		return
	}
	r.SetPathValue("id", id)
	switch action {
	case "pause":
		h.pauseSandbox(w, r)
	case "resume":
		h.resumeSandbox(w, r)
	default:
		httpapi.WriteProblem(w, r, 404, "not_found", "resource action not found")
	}
}

type Source struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type Lifecycle struct {
	TTLSeconds         int `json:"ttl_seconds,omitempty"`
	IdleTimeoutSeconds int `json:"idle_timeout_seconds,omitempty"`
}

type Resources struct {
	VCPU      int64 `json:"vcpu"`
	MemoryMIB int64 `json:"memory_mib"`
}

type Sandbox struct {
	ID        string            `json:"id"`
	Name      string            `json:"name,omitempty"`
	Status    string            `json:"status"`
	Source    Source            `json:"source"`
	Lifecycle Lifecycle         `json:"lifecycle"`
	Resources Resources         `json:"resources"`
	Metadata  map[string]string `json:"metadata"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
}

type Snapshot struct {
	ID              string     `json:"id"`
	Name            string     `json:"name,omitempty"`
	SourceSandboxID string     `json:"source_sandbox_id"`
	State           string     `json:"state"`
	CreatedAt       time.Time  `json:"created_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
}

type createRequest struct {
	Name         string            `json:"name"`
	Source       Source            `json:"source"`
	Lifecycle    Lifecycle         `json:"lifecycle"`
	Resources    *Resources        `json:"resources"`
	Metadata     map[string]string `json:"metadata"`
	SSHPublicKey string            `json:"ssh_public_key"`
}

type updateRequest struct {
	Name      *string            `json:"name"`
	Lifecycle *Lifecycle         `json:"lifecycle"`
	Metadata  *map[string]string `json:"metadata"`
}

type listResponse[T any] struct {
	Items         []T    `json:"-"`
	NextPageToken string `json:"next_page_token,omitempty"`
}

type BatchItem struct {
	Index   int              `json:"index"`
	Sandbox *Sandbox         `json:"sandbox,omitempty"`
	Error   *httpapi.Problem `json:"error,omitempty"`
}

type Operation struct {
	ID          string      `json:"id"`
	Type        string      `json:"type"`
	Status      string      `json:"status"`
	Requested   int         `json:"requested"`
	Succeeded   int         `json:"succeeded"`
	Failed      int         `json:"failed"`
	Results     []BatchItem `json:"results,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
}

func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	var body createRequest
	if !decodeBody(w, r, &body, true) {
		return
	}
	if err := validateCreate(body); err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return
	}
	sb, status, detail := h.create(r, body)
	if status != http.StatusCreated {
		httpapi.WriteProblem(w, r, status, "", detail)
		return
	}
	w.Header().Set("Location", "/v1/sandboxes/"+url.PathEscape(sb.ID))
	writeJSON(w, http.StatusCreated, sb)
}

func (h *Handler) create(r *http.Request, body createRequest) (Sandbox, int, string) {
	source := body.Source
	if source.Type == "" {
		source.Type = "default"
	}
	legacyBody := map[string]any{
		"name":                body.Name,
		"timeout_sec":         body.Lifecycle.TTLSeconds,
		"hibernate_after_sec": body.Lifecycle.IdleTimeoutSeconds,
		"ssh_pubkey":          body.SSHPublicKey,
	}
	if body.Resources != nil {
		legacyBody["vcpus"] = body.Resources.VCPU
		legacyBody["mem_mib"] = body.Resources.MemoryMIB
	}
	path := "/sandboxes"
	if source.Type == "snapshot" {
		path = "/snapshots/" + url.PathEscape(source.ID) + "/fanout"
		legacyBody["count"] = 1
		delete(legacyBody, "ssh_pubkey") // legacy fanout does not provision keys
	}
	rec := h.call(r, http.MethodPost, path, legacyBody)
	if rec.Code < 200 || rec.Code >= 300 {
		return Sandbox{}, rec.Code, legacyDetail(rec)
	}
	var raw registry.Sandbox
	if source.Type == "snapshot" {
		var list []registry.Sandbox
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
			return Sandbox{}, 502, "invalid batch-create response from worker"
		}
		raw = list[0]
	} else if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		return Sandbox{}, 502, "invalid create response from worker"
	}
	fields := map[string]any{"source_type": source.Type, "source_id": source.ID, "metadata": nonNilMetadata(body.Metadata)}
	annotated := h.call(r, http.MethodPatch, "/sandboxes/"+url.PathEscape(raw.ID)+"/public-fields", fields)
	if annotated.Code >= 200 && annotated.Code < 300 {
		_ = json.Unmarshal(annotated.Body.Bytes(), &raw)
	} else {
		return Sandbox{}, annotated.Code, legacyDetail(annotated)
	}
	return publicSandbox(raw), http.StatusCreated, ""
}

func (h *Handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodGet, "/sandboxes", nil)
	if !translateError(w, r, rec) {
		return
	}
	var raw []registry.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	items := make([]Sandbox, 0, len(raw))
	for _, sb := range raw {
		pub := publicSandbox(sb)
		if !matchesSandboxFilters(pub, r.URL.Query()) {
			continue
		}
		items = append(items, pub)
	}
	page, next, ok := paginate(w, r, items)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"sandboxes": page, "next_page_token": next})
}

func (h *Handler) getSandbox(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodGet, "/sandboxes/"+url.PathEscape(r.PathValue("id")), nil)
	if !translateError(w, r, rec) {
		return
	}
	var raw registry.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	writeJSON(w, 200, publicSandbox(raw))
}

func (h *Handler) updateSandbox(w http.ResponseWriter, r *http.Request) {
	var body updateRequest
	if !decodeBody(w, r, &body, true) {
		return
	}
	currentRec := h.call(r, http.MethodGet, "/sandboxes/"+url.PathEscape(r.PathValue("id")), nil)
	if !translateError(w, r, currentRec) {
		return
	}
	var current registry.Sandbox
	if err := json.Unmarshal(currentRec.Body.Bytes(), &current); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	name, metadata := current.Name, current.Metadata
	ttl, idle := remainingTTL(current.ExpiresAt), current.HibernateAfterSec
	if body.Name != nil {
		name = *body.Name
	}
	if body.Metadata != nil {
		metadata = *body.Metadata
	}
	if body.Lifecycle != nil {
		ttl, idle = body.Lifecycle.TTLSeconds, body.Lifecycle.IdleTimeoutSeconds
	}
	rec := h.call(r, http.MethodPatch, "/sandboxes/"+url.PathEscape(current.ID)+"/public-fields",
		map[string]any{"name": name, "metadata": nonNilMetadata(metadata), "ttl_seconds": ttl, "idle_timeout_seconds": idle})
	if !translateError(w, r, rec) {
		return
	}
	var updated registry.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	writeJSON(w, 200, publicSandbox(updated))
}

func (h *Handler) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodDelete, "/sandboxes/"+url.PathEscape(r.PathValue("id")), nil)
	if !translateError(w, r, rec) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) pauseSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, "hibernate")
}

func (h *Handler) resumeSandbox(w http.ResponseWriter, r *http.Request) {
	h.lifecycleAction(w, r, "resume")
}

func (h *Handler) lifecycleAction(w http.ResponseWriter, r *http.Request, action string) {
	rec := h.call(r, http.MethodPost, "/sandboxes/"+url.PathEscape(r.PathValue("id"))+"/"+action, nil)
	if !translateError(w, r, rec) {
		return
	}
	var raw registry.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	writeJSON(w, 200, publicSandbox(raw))
}

func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             string `json:"name"`
		RetentionSeconds int    `json:"retention_seconds"`
	}
	if !decodeBody(w, r, &body, false) {
		return
	}
	if body.RetentionSeconds < 0 {
		httpapi.WriteProblem(w, r, 400, "invalid_request", "retention_seconds must be non-negative")
		return
	}
	rec := h.call(r, http.MethodPost, "/sandboxes/"+url.PathEscape(r.PathValue("id"))+"/snapshot", map[string]any{"name": body.Name})
	if !translateError(w, r, rec) {
		return
	}
	var raw registry.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	annotated := h.call(r, http.MethodPatch, "/snapshots/"+url.PathEscape(raw.ID)+"/public-fields",
		map[string]any{"name": body.Name, "retention_seconds": body.RetentionSeconds})
	if !translateError(w, r, annotated) {
		return
	}
	if err := json.Unmarshal(annotated.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	writeJSON(w, 201, publicSnapshot(raw))
}

func (h *Handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodGet, "/snapshots", nil)
	if !translateError(w, r, rec) {
		return
	}
	var raw []registry.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	for _, snap := range raw {
		if !snap.Golden && snap.ID == r.PathValue("id") {
			writeJSON(w, 200, publicSnapshot(snap))
			return
		}
	}
	httpapi.WriteProblem(w, r, 404, "snapshot_not_found", "snapshot not found")
}

func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodGet, "/snapshots", nil)
	if !translateError(w, r, rec) {
		return
	}
	var raw []registry.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	items := make([]Snapshot, 0, len(raw))
	for _, snap := range raw {
		if !snap.Golden {
			items = append(items, publicSnapshot(snap))
		}
	}
	page, next, ok := paginate(w, r, items)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"snapshots": page, "next_page_token": next})
}

func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodDelete, "/snapshots/"+url.PathEscape(r.PathValue("id")), nil)
	if !translateError(w, r, rec) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodGet, "/info", nil)
	if !translateError(w, r, rec) {
		return
	}
	var info struct {
		DefaultVcpus  int64 `json:"default_vcpus"`
		DefaultMemMIB int64 `json:"default_mem_mib"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	templates := []map[string]any{template(info.DefaultVcpus, info.DefaultMemMIB)}
	page, next, ok := paginate(w, r, templates)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"templates": page, "next_page_token": next})
}

func (h *Handler) getTemplate(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != "default" {
		httpapi.WriteProblem(w, r, 404, "template_not_found", "template not found")
		return
	}
	rec := h.call(r, http.MethodGet, "/info", nil)
	if !translateError(w, r, rec) {
		return
	}
	var info struct {
		DefaultVcpus  int64 `json:"default_vcpus"`
		DefaultMemMIB int64 `json:"default_mem_mib"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	writeJSON(w, 200, template(info.DefaultVcpus, info.DefaultMemMIB))
}

func (h *Handler) createBatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Count          int           `json:"count"`
		Sandbox        createRequest `json:"sandbox"`
		MaxParallelism int           `json:"max_parallelism"`
	}
	if !decodeBody(w, r, &body, true) {
		return
	}
	if body.Count < 1 || body.Count > 100 {
		httpapi.WriteProblem(w, r, 400, "invalid_request", "count must be between 1 and 100")
		return
	}
	if err := validateCreate(body.Sandbox); err != nil {
		httpapi.WriteProblem(w, r, 400, "invalid_request", err.Error())
		return
	}
	if body.MaxParallelism == 0 {
		body.MaxParallelism = 8
	}
	if body.MaxParallelism < 1 || body.MaxParallelism > 32 {
		httpapi.WriteProblem(w, r, 400, "invalid_request", "max_parallelism must be between 1 and 32")
		return
	}
	op := &Operation{
		ID: uuid.NewString(), Type: "sandbox_batch_create", Status: "pending",
		Requested: body.Count, CreatedAt: time.Now(), Results: make([]BatchItem, body.Count),
	}
	h.mu.Lock()
	h.pruneOperationsLocked(time.Now())
	h.operations[op.ID] = op
	h.mu.Unlock()
	background := r.Clone(context.WithoutCancel(r.Context()))
	go h.runBatch(background, op.ID, body.Count, body.MaxParallelism, body.Sandbox)
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	writeJSON(w, http.StatusAccepted, op)
}

func (h *Handler) runBatch(parent *http.Request, id string, count, parallel int, create createRequest) {
	h.mu.Lock()
	h.operations[id].Status = "running"
	h.mu.Unlock()
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			req := parent.Clone(parent.Context())
			req.Header.Set(httpapi.RequestIDHeader, parent.Header.Get(httpapi.RequestIDHeader))
			sb, status, detail := h.create(req, create)
			h.mu.Lock()
			defer h.mu.Unlock()
			op := h.operations[id]
			if status == http.StatusCreated {
				op.Succeeded++
				op.Results[index] = BatchItem{Index: index, Sandbox: &sb}
			} else {
				op.Failed++
				op.Results[index] = BatchItem{Index: index, Error: &httpapi.Problem{
					Type: "https://sandbox.dev/problems/batch_item_failed", Title: http.StatusText(status),
					Status: status, Detail: detail, Code: "batch_item_failed", RequestID: httpapi.RequestID(parent),
				}}
			}
		}(i)
	}
	wg.Wait()
	now := time.Now()
	h.mu.Lock()
	op := h.operations[id]
	op.CompletedAt = &now
	switch {
	case op.Failed == 0:
		op.Status = "succeeded"
	case op.Succeeded == 0:
		op.Status = "failed"
	default:
		op.Status = "partially_succeeded"
	}
	h.mu.Unlock()
}

func (h *Handler) getOperation(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	op := cloneOperation(h.operations[r.PathValue("id")])
	h.mu.RUnlock()
	if op == nil {
		httpapi.WriteProblem(w, r, 404, "operation_not_found", "operation not found")
		return
	}
	writeJSON(w, 200, op)
}

func (h *Handler) listOperations(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.pruneOperationsLocked(time.Now())
	ops := make([]Operation, 0, len(h.operations))
	for _, op := range h.operations {
		ops = append(ops, *cloneOperation(op))
	}
	h.mu.Unlock()
	sort.Slice(ops, func(i, j int) bool { return ops[i].CreatedAt.After(ops[j].CreatedAt) })
	page, next, ok := paginate(w, r, ops)
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"operations": page, "next_page_token": next})
}

func (h *Handler) createPortForward(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GuestPort int `json:"guest_port"`
	}
	if !decodeBody(w, r, &body, true) {
		return
	}
	rec := h.call(r, http.MethodPost, "/sandboxes/"+url.PathEscape(r.PathValue("id"))+"/ports", body)
	if !translateError(w, r, rec) {
		return
	}
	var port registry.PortMapping
	if err := json.Unmarshal(rec.Body.Bytes(), &port); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	writeJSON(w, 201, publicPort(r.PathValue("id"), port))
}

func (h *Handler) listPortForwards(w http.ResponseWriter, r *http.Request) {
	rec := h.call(r, http.MethodGet, "/sandboxes/"+url.PathEscape(r.PathValue("id"))+"/ports", nil)
	if !translateError(w, r, rec) {
		return
	}
	var raw []registry.PortMapping
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		httpapi.WriteProblem(w, r, 502, "invalid_upstream_response", err.Error())
		return
	}
	out := make([]map[string]any, len(raw))
	for i, port := range raw {
		out[i] = publicPort(r.PathValue("id"), port)
	}
	writeJSON(w, 200, map[string]any{"port_forwards": out})
}

func (h *Handler) call(parent *http.Request, method, path string, body any) *responseRecorder {
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, _ := http.NewRequestWithContext(parent.Context(), method, path, reader)
	req.Header = parent.Header.Clone()
	req.Header.Del("Idempotency-Key")
	rec := newRecorder()
	h.legacy.ServeHTTP(rec, req)
	return rec
}

func publicSandbox(sb registry.Sandbox) Sandbox {
	status := sb.Status
	switch status {
	case registry.StatusHibernated:
		status = "paused"
	case registry.StatusStopping:
		status = "deleting"
	}
	source := Source{Type: sb.SourceType, ID: sb.SourceID}
	if source.Type == "" {
		source.Type = "default"
	}
	return Sandbox{
		ID: sb.ID, Name: sb.Name, Status: status, Source: source,
		Lifecycle: Lifecycle{TTLSeconds: remainingTTL(sb.ExpiresAt), IdleTimeoutSeconds: sb.HibernateAfterSec},
		Resources: Resources{VCPU: sb.Vcpus, MemoryMIB: sb.MemMIB},
		Metadata:  nonNilMetadata(sb.Metadata), CreatedAt: sb.CreatedAt, ExpiresAt: sb.ExpiresAt,
	}
}

func (h *Handler) pruneOperationsLocked(now time.Time) {
	for id, op := range h.operations {
		if now.Sub(op.CreatedAt) > 24*time.Hour {
			delete(h.operations, id)
		}
	}
}

func publicSnapshot(s registry.Snapshot) Snapshot {
	state := s.Durability
	if state == "" {
		state = "local"
	}
	return Snapshot{
		ID: s.ID, Name: s.Name, SourceSandboxID: s.SourceID, State: state,
		CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt,
	}
}

func publicPort(sandboxID string, p registry.PortMapping) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("%s:%d", sandboxID, p.GuestPort), "sandbox_id": sandboxID,
		"guest_port": p.GuestPort, "host_port": p.HostPort, "status": "active",
	}
}

func template(vcpu, memoryMIB int64) map[string]any {
	return map[string]any{
		"id": "default", "revision": "host-default",
		"resources": Resources{VCPU: vcpu, MemoryMIB: memoryMIB},
	}
}

func validateCreate(body createRequest) error {
	source := body.Source
	if source.Type == "" {
		source.Type = "default"
	}
	switch source.Type {
	case "default":
		if source.ID != "" {
			return errors.New("default source must not include id")
		}
	case "template":
		if source.ID != "default" {
			return errors.New("only template id \"default\" is currently available")
		}
	case "snapshot":
		if source.ID == "" {
			return errors.New("snapshot source requires id")
		}
	default:
		return errors.New("source.type must be default, template, or snapshot")
	}
	if body.Lifecycle.TTLSeconds < 0 || body.Lifecycle.IdleTimeoutSeconds < 0 {
		return errors.New("lifecycle durations must be non-negative")
	}
	if body.Resources != nil && (body.Resources.VCPU < 1 || body.Resources.MemoryMIB < 128) {
		return errors.New("resources.vcpu must be positive and resources.memory_mib must be at least 128")
	}
	if len(body.Metadata) > 64 {
		return errors.New("metadata must contain at most 64 entries")
	}
	for key, value := range body.Metadata {
		if key == "" || len(key) > 64 || len(value) > 1024 {
			return errors.New("metadata keys must be 1-64 bytes and values at most 1024 bytes")
		}
	}
	return nil
}

func matchesSandboxFilters(sb Sandbox, query url.Values) bool {
	if want := query.Get("status"); want != "" && want != sb.Status {
		return false
	}
	if want := query.Get("source_type"); want != "" && want != sb.Source.Type {
		return false
	}
	if value := query.Get("created_after"); value != "" {
		t, err := time.Parse(time.RFC3339, value)
		if err != nil || !sb.CreatedAt.After(t) {
			return false
		}
	}
	if value := query.Get("created_before"); value != "" {
		t, err := time.Parse(time.RFC3339, value)
		if err != nil || !sb.CreatedAt.Before(t) {
			return false
		}
	}
	for key, values := range query {
		if strings.HasPrefix(key, "metadata.") && (len(values) == 0 || sb.Metadata[strings.TrimPrefix(key, "metadata.")] != values[0]) {
			return false
		}
	}
	return true
}

func paginate[T any](w http.ResponseWriter, r *http.Request, items []T) ([]T, string, bool) {
	size := 50
	if value := r.URL.Query().Get("page_size"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			httpapi.WriteProblem(w, r, 400, "invalid_page_size", "page_size must be between 1 and 100")
			return nil, "", false
		}
		size = n
	}
	offset, err := httpapi.ParseCursor(r.URL.Query().Get("page_token"))
	if err != nil || offset > len(items) {
		httpapi.WriteProblem(w, r, 400, "invalid_page_token", "page_token is invalid")
		return nil, "", false
	}
	end := offset + size
	if end > len(items) {
		end = len(items)
	}
	next := ""
	if end < len(items) {
		next = httpapi.Cursor(end)
	}
	return items[offset:end], next, true
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any, required bool) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) && !required {
			return true
		}
		httpapi.WriteProblem(w, r, 400, "invalid_request", "invalid JSON body: "+err.Error())
		return false
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		httpapi.WriteProblem(w, r, 400, "invalid_request", "request body must contain exactly one JSON value")
		return false
	}
	return true
}

func translateError(w http.ResponseWriter, r *http.Request, rec *responseRecorder) bool {
	if rec.Code >= 200 && rec.Code < 300 {
		return true
	}
	httpapi.WriteProblem(w, r, rec.Code, "", legacyDetail(rec))
	return false
}

func legacyDetail(rec *responseRecorder) string {
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &body) == nil && body.Error != "" {
		return body.Error
	}
	if text := strings.TrimSpace(rec.Body.String()); text != "" {
		return text
	}
	return http.StatusText(rec.Code)
}

func remainingTTL(expiry *time.Time) int {
	if expiry == nil {
		return 0
	}
	seconds := int(time.Until(*expiry).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

func nonNilMetadata(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}

func cloneOperation(in *Operation) *Operation {
	if in == nil {
		return nil
	}
	out := *in
	out.Results = append([]BatchItem(nil), in.Results...)
	return &out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type responseRecorder struct {
	HeaderMap http.Header
	Body      bytes.Buffer
	Code      int
}

func newRecorder() *responseRecorder {
	return &responseRecorder{HeaderMap: make(http.Header), Code: 200}
}
func (r *responseRecorder) Header() http.Header    { return r.HeaderMap }
func (r *responseRecorder) WriteHeader(status int) { r.Code = status }
func (r *responseRecorder) Write(p []byte) (int, error) {
	return r.Body.Write(p)
}
