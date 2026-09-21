package executions

import (
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
	r.Delete("/{id}", requireAuth(api.Delete))
	return r
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
			http.Error(w, "execution did not become ready in time", http.StatusGatewayTimeout)
			return
		}
		if ctx.Err() != nil {
			// Client went away; nothing useful to write.
			return
		}
		logger.Error("execution failed to become ready", "execution_id", execID, "error", err)
		http.Error(w, "execution failed", http.StatusBadGateway)
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
	if execID == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
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
	if execID == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if len(req.Argv) == 0 {
		http.Error(w, "argv is required and must not be empty", http.StatusBadRequest)
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

	api.proxy(w, r, http.MethodPost, rec.WorkerAddr, "/executions/"+execID+"/exec", body)
}

// DELETE /api/executions/{id}
func (api *ExecutionsAPI) Delete(w http.ResponseWriter, r *http.Request) {
	execID := chi.URLParam(r, "id")
	if execID == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
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

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
