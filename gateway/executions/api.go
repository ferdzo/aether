package executions

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aether/gateway/internal"
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/protocol"

	"github.com/go-chi/chi/v5"
	etcd "go.etcd.io/etcd/client/v3"
)

// executionRecordTimeout bounds a single etcd read so an unreachable etcd fails
// the request with 502 instead of hanging it.
const executionRecordTimeout = 2 * time.Second

// maxExecutionBodyBytes bounds a create/exec request body so a large upload
// cannot make the gateway buffer without bound.
const maxExecutionBodyBytes = 1 << 20 // 1 MiB

// ErrExecutionNotFound is returned by an ExecutionRecordStore when no record
// exists for an id, so handlers can map it to 404 separately from a transport
// error.
var ErrExecutionNotFound = errors.New("execution record not found")

// ExecutionRecordStore reads the durable execution records workers write to
// etcd. The etcd-backed implementation is the production default; tests
// substitute a stub.
type ExecutionRecordStore interface {
	GetExecution(ctx context.Context, id string) (*protocol.ExecutionRecord, error)
}

type etcdExecutionStore struct {
	client *etcd.Client
}

func (s *etcdExecutionStore) GetExecution(ctx context.Context, id string) (*protocol.ExecutionRecord, error) {
	resp, err := s.client.Get(ctx, protocol.ExecutionKey(id))
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, ErrExecutionNotFound
	}

	var rec protocol.ExecutionRecord
	if err := json.Unmarshal(resp.Kvs[0].Value, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

type ExecutionsAPI struct {
	redis     *internal.RedisClient
	records   ExecutionRecordStore
	authToken string
	// controlToken is presented to the owning worker's control API as a bearer
	// token. It is independent of authToken (the gateway's own API auth).
	controlToken string

	httpClient *http.Client
	// readyTimeout bounds how long Create polls the record for state=ready
	// before giving up with 504. pollInterval is the poll cadence.
	readyTimeout time.Duration
	pollInterval time.Duration
}

func NewExecutionsAPI(redisClient *internal.RedisClient, etcdClient *etcd.Client, authToken, controlToken string) *ExecutionsAPI {
	api := &ExecutionsAPI{
		redis:        redisClient,
		authToken:    authToken,
		controlToken: controlToken,
		httpClient:   &http.Client{},
		readyTimeout: 120 * time.Second,
		pollInterval: 250 * time.Millisecond,
	}
	if etcdClient != nil {
		api.records = &etcdExecutionStore{client: etcdClient}
	}
	return api
}

func (api *ExecutionsAPI) Routes() chi.Router {
	r := chi.NewRouter()

	// When AUTH_TOKEN is set, every executions call must present it as a bearer
	// token; unset keeps the homelab-open behavior (matches the jobs API).
	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if api.authToken != "" {
				token, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
				if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(api.authToken)) != 1 {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
			next(w, req)
		}
	}

	r.Post("/", requireAuth(api.Create))
	r.Get("/{id}", requireAuth(api.Get))
	r.Post("/{id}/exec", requireAuth(api.Exec))
	r.Get("/{id}/exec/{exec_id}", requireAuth(api.GetExecRecord))
	r.Get("/{id}/exec/{exec_id}/events", requireAuth(api.ExecEvents))
	r.Post("/{id}/exec/{exec_id}/signal", requireAuth(api.Signal))
	r.Delete("/{id}", requireAuth(api.Delete))
	return r
}

// validExecID applies the same id rules as Create to the path-based handlers,
// so a crafted id can never reach an etcd key or worker path.
func validExecID(w http.ResponseWriter, id string) bool {
	if id == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
		return false
	}
	if err := protocol.ValidJobID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// writeExecutionError writes a small JSON error that includes the execution id,
// so a caller that supplied none can still find or destroy the execution an
// error refers to.
func writeExecutionError(w http.ResponseWriter, status int, id, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "id": id})
}

// POST /api/executions
//
// Creation reuses the existing provision queue: a Mode:"execution" job is
// published and the handler then polls the durable record until the worker
// reports it ready. There is no scheduler.
func (api *ExecutionsAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string            `json:"id"`
		Runtime        string            `json:"runtime"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		VCPU           int               `json:"vcpu"`
		MemoryMB       int               `json:"memory_mb"`
		WorkspaceMB    int               `json:"workspace_mb"`
		EnvVars        map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxExecutionBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.Runtime == "" {
		http.Error(w, "runtime is required", http.StatusBadRequest)
		return
	}
	// vcpu and memory default to 1 and 128 when zero; negative values would
	// otherwise reach the Firecracker machine configuration unchanged.
	if req.VCPU < 0 {
		http.Error(w, "vcpu must not be negative", http.StatusBadRequest)
		return
	}
	if req.MemoryMB < 0 {
		http.Error(w, "memory_mb must not be negative", http.StatusBadRequest)
		return
	}
	// An execution with no lifetime cannot be stopped: require an explicit
	// positive timeout rather than silently allowing an unbounded VM.
	if req.TimeoutSeconds <= 0 {
		http.Error(w, "timeout_seconds must be positive", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds > protocol.MaxTimeoutSeconds {
		http.Error(w, fmt.Sprintf("timeout_seconds must be at most %d", protocol.MaxTimeoutSeconds), http.StatusBadRequest)
		return
	}
	// The workspace is where results persist between execs, so it is required.
	if req.WorkspaceMB <= 0 {
		http.Error(w, "workspace_mb must be positive", http.StatusBadRequest)
		return
	}
	if req.WorkspaceMB > protocol.MaxWorkspaceMB {
		http.Error(w, fmt.Sprintf("workspace_mb must be at most %d", protocol.MaxWorkspaceMB), http.StatusBadRequest)
		return
	}
	if req.ID != "" {
		if err := protocol.ValidJobID(req.ID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if api.records == nil {
		http.Error(w, "execution records unavailable", http.StatusBadGateway)
		return
	}

	execID := req.ID
	if execID == "" {
		execID = id.GenerateJobID()
	}

	job := &protocol.Job{
		RequestID:      id.GenerateRequestID(),
		JobID:          execID,
		Mode:           protocol.ExecutionMode,
		Runtime:        req.Runtime,
		TimeoutSeconds: req.TimeoutSeconds,
		VCPU:           req.VCPU,
		MemoryMB:       req.MemoryMB,
		WorkspaceMB:    req.WorkspaceMB,
		EnvVars:        req.EnvVars,
	}

	if err := api.redis.PushJob(job); err != nil {
		logger.Error("failed to publish execution", "execution_id", execID, "error", err)
		http.Error(w, "failed to publish execution", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), api.readyTimeout)
	defer cancel()

	rec, err := api.waitReady(ctx, execID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeExecutionError(w, http.StatusGatewayTimeout, execID, "execution did not become ready in time")
			return
		}
		if ctx.Err() != nil {
			// Client went away; nothing useful to write.
			return
		}
		logger.Error("execution failed to become ready", "execution_id", execID, "error", err)
		writeExecutionError(w, http.StatusBadGateway, execID, "execution failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(rec)
}

// waitReady polls the durable record until the worker marks the execution
// ready. A missing record (worker still provisioning) is not an error; a
// terminal failed state or a poll timeout is.
func (api *ExecutionsAPI) waitReady(ctx context.Context, execID string) (*protocol.ExecutionRecord, error) {
	for {
		rec, err := api.records.GetExecution(ctx, execID)
		switch {
		case err == nil && rec.State == protocol.ExecutionStateReady:
			return rec, nil
		case err == nil && rec.State == protocol.ExecutionStateFailed:
			return nil, fmt.Errorf("execution %s failed: %s", execID, rec.Error)
		case err == nil && rec.State == protocol.ExecutionStateStopped:
			return nil, fmt.Errorf("execution %s stopped during creation", execID)
		case err != nil && !errors.Is(err, ErrExecutionNotFound):
			return nil, err
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, ctx.Err()
		case <-time.After(api.pollInterval):
		}
	}
}

// GET /api/executions/{id}
func (api *ExecutionsAPI) Get(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	if !validExecID(w, execID) {
		return
	}
	if api.records == nil {
		http.Error(w, "execution records unavailable", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), executionRecordTimeout)
	defer cancel()

	rec, err := api.records.GetExecution(ctx, execID)
	if err != nil {
		if errors.Is(err, ErrExecutionNotFound) {
			http.Error(w, "execution not found", http.StatusNotFound)
			return
		}
		logger.Error("failed to read execution record", "execution_id", execID, "error", err)
		http.Error(w, "failed to read execution record", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rec)
}

// POST /api/executions/{id}/exec
//
// The command is proxied to the owning worker's control API; argv is never
// shell-wrapped.
func (api *ExecutionsAPI) Exec(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	if !validExecID(w, execID) {
		return
	}

	rec, ok := api.requireReady(w, r, execID)
	if !ok {
		return
	}

	var req struct {
		Argv           []string          `json:"argv"`
		Cwd            string            `json:"cwd"`
		Env            map[string]string `json:"env"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		Stream         bool              `json:"stream"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxExecutionBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(req.Argv) == 0 {
		http.Error(w, "argv is required and must not be empty", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds < 0 {
		http.Error(w, "timeout_seconds must not be negative", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds > protocol.MaxTimeoutSeconds {
		http.Error(w, fmt.Sprintf("timeout_seconds must be at most %d", protocol.MaxTimeoutSeconds), http.StatusBadRequest)
		return
	}

	execPath := "/executions/" + execID + "/exec"
	if req.Stream {
		// Stream the worker's SSE response through unchanged, flushing per
		// chunk so the client sees events as they arrive. The worker holds the
		// bounded buffer, so a slow gateway read cannot stall the exec.
		body, err := json.Marshal(struct {
			Argv           []string          `json:"argv"`
			Cwd            string            `json:"cwd,omitempty"`
			Env            map[string]string `json:"env,omitempty"`
			TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
			Stream         bool              `json:"stream"`
		}{req.Argv, req.Cwd, req.Env, req.TimeoutSeconds, true})
		if err != nil {
			http.Error(w, "failed to encode exec request", http.StatusInternalServerError)
			return
		}
		api.proxySSE(w, r, http.MethodPost, rec.WorkerAddr, execPath, body)
		return
	}

	body, err := json.Marshal(protocol.ExecRequest{
		Argv:           req.Argv,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
	})
	if err != nil {
		http.Error(w, "failed to encode exec request", http.StatusInternalServerError)
		return
	}

	api.proxy(w, r, http.MethodPost, rec.WorkerAddr, execPath, body)
}

// GET /api/executions/{id}/exec/{exec_id}
//
// Proxies the owning worker's per-exec record, propagating 404/409/502.
func (api *ExecutionsAPI) GetExecRecord(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	scopedExecID := chi.URLParam(r, "exec_id")
	if !validExecID(w, execID) || !validExecID(w, scopedExecID) {
		return
	}
	rec, ok := api.lookup(w, r, execID)
	if !ok {
		return
	}
	if rec.WorkerAddr == "" {
		http.Error(w, "execution has no worker address", http.StatusBadGateway)
		return
	}
	api.proxy(w, r, http.MethodGet, rec.WorkerAddr, "/executions/"+execID+"/exec/"+scopedExecID, nil)
}

// GET /api/executions/{id}/exec/{exec_id}/events
//
// Proxies the owning worker's SSE event stream, forwarding Last-Event-ID so a
// reconnect resumes where it left off. The stream is flushed per chunk.
func (api *ExecutionsAPI) ExecEvents(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	scopedExecID := chi.URLParam(r, "exec_id")
	if !validExecID(w, execID) || !validExecID(w, scopedExecID) {
		return
	}
	rec, ok := api.lookup(w, r, execID)
	if !ok {
		return
	}
	if rec.WorkerAddr == "" {
		http.Error(w, "execution has no worker address", http.StatusBadGateway)
		return
	}
	api.proxySSE(w, r, http.MethodGet, rec.WorkerAddr, "/executions/"+execID+"/exec/"+scopedExecID+"/events", nil)
}

// POST /api/executions/{id}/exec/{exec_id}/signal
//
// Proxies a signal delivery to the owning worker's control API and relays its
// status (404 unknown, 409 not running, 400 invalid, 502 transport).
func (api *ExecutionsAPI) Signal(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	scopedExecID := chi.URLParam(r, "exec_id")
	if !validExecID(w, execID) || !validExecID(w, scopedExecID) {
		return
	}
	rec, ok := api.lookup(w, r, execID)
	if !ok {
		return
	}
	if rec.WorkerAddr == "" {
		http.Error(w, "execution has no worker address", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxExecutionBodyBytes))
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	api.proxy(w, r, http.MethodPost, rec.WorkerAddr, "/executions/"+execID+"/exec/"+scopedExecID+"/signal", body)
}

// DELETE /api/executions/{id}
func (api *ExecutionsAPI) Delete(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	if !validExecID(w, execID) {
		return
	}

	rec, ok := api.lookup(w, r, execID)
	if !ok {
		return
	}
	if rec.WorkerAddr == "" {
		http.Error(w, "execution has no worker address", http.StatusBadGateway)
		return
	}

	api.proxy(w, r, http.MethodDelete, rec.WorkerAddr, "/executions/"+execID, nil)
}

// requireReady reads the record and rejects anything not currently ready. It
// writes the error response itself and reports whether the caller may proceed.
func (api *ExecutionsAPI) requireReady(w http.ResponseWriter, r *http.Request, execID string) (*protocol.ExecutionRecord, bool) {
	rec, ok := api.lookup(w, r, execID)
	if !ok {
		return nil, false
	}
	if rec.State != protocol.ExecutionStateReady {
		http.Error(w, fmt.Sprintf("execution is %s, not ready", rec.State), http.StatusConflict)
		return nil, false
	}
	if rec.WorkerAddr == "" {
		http.Error(w, "execution has no worker address", http.StatusBadGateway)
		return nil, false
	}
	return rec, true
}

func (api *ExecutionsAPI) lookup(w http.ResponseWriter, r *http.Request, execID string) (*protocol.ExecutionRecord, bool) {
	if api.records == nil {
		http.Error(w, "execution records unavailable", http.StatusBadGateway)
		return nil, false
	}

	ctx, cancel := context.WithTimeout(r.Context(), executionRecordTimeout)
	defer cancel()

	rec, err := api.records.GetExecution(ctx, execID)
	if err != nil {
		if errors.Is(err, ErrExecutionNotFound) {
			http.Error(w, "execution not found", http.StatusNotFound)
			return nil, false
		}
		logger.Error("failed to read execution record", "execution_id", execID, "error", err)
		http.Error(w, "failed to read execution record", http.StatusBadGateway)
		return nil, false
	}
	return rec, true
}

// proxy forwards a request to the owning worker's control API, adding the
// control bearer token and relaying the worker's status and body unchanged so
// 404/409/502 propagate.
func (api *ExecutionsAPI) proxy(w http.ResponseWriter, r *http.Request, method, workerAddr, path string, body []byte) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}

	req, err := http.NewRequestWithContext(r.Context(), method, "http://"+workerAddr+path, reader)
	if err != nil {
		http.Error(w, "failed to build worker request", http.StatusBadGateway)
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if api.controlToken != "" {
		req.Header.Set("Authorization", "Bearer "+api.controlToken)
	}

	resp, err := api.httpClient.Do(req)
	if err != nil {
		logger.Error("worker control request failed", "worker_addr", workerAddr, "error", err)
		http.Error(w, "worker unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// A 401 from the worker is the gateway's own misconfiguration (wrong or
	// missing control token), not the caller's auth failure: the caller already
	// authenticated at the gateway. Surface it as 502.
	if resp.StatusCode == http.StatusUnauthorized {
		logger.Error("worker rejected the gateway control token", "worker_addr", workerAddr)
		http.Error(w, "worker rejected control token", http.StatusBadGateway)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// proxySSE forwards a streaming (text/event-stream) request to the owning
// worker and relays its response verbatim, flushing after every chunk so events
// are genuinely live. Non-2xx (404/409/502) bodies are relayed too, so the
// worker's status propagates. A worker 401 is the gateway's own control-token
// misconfiguration and is surfaced as 502, as the non-streaming proxy does.
//
// The Last-Event-ID request header is forwarded so a reconnect resumes from the
// worker's buffer.
func (api *ExecutionsAPI) proxySSE(w http.ResponseWriter, r *http.Request, method, workerAddr, path string, body []byte) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(r.Context(), method, "http://"+workerAddr+path, reader)
	if err != nil {
		http.Error(w, "failed to build worker request", http.StatusBadGateway)
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if api.controlToken != "" {
		req.Header.Set("Authorization", "Bearer "+api.controlToken)
	}
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		req.Header.Set("Last-Event-ID", last)
	}

	resp, err := api.httpClient.Do(req)
	if err != nil {
		logger.Error("worker control stream failed", "worker_addr", workerAddr, "error", err)
		http.Error(w, "worker unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		logger.Error("worker rejected the gateway control token", "worker_addr", workerAddr)
		http.Error(w, "worker rejected control token", http.StatusBadGateway)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if execID := resp.Header.Get("X-Exec-ID"); execID != "" {
		w.Header().Set("X-Exec-ID", execID)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			// Bound a stuck downstream client; the worker's buffer absorbs a
			// slow reader, so this only unwinds this proxy handler.
			_ = rc.SetWriteDeadline(time.Now().Add(reverseSSEWriteTimeout))
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// reverseSSEWriteTimeout bounds one proxied SSE write to a slow client.
const reverseSSEWriteTimeout = 30 * time.Second
