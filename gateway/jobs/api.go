package jobs

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
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

const jobModeProcess = "process"

// jobCancelRequestedState is the state echoed to the caller when a cancel
// request has been published but the worker has not yet recorded the terminal
// outcome. It is intentionally not a JobRecord state.
const jobCancelRequestedState = "cancel_requested"

// jobRecordTimeout bounds a single etcd read so an unreachable etcd fails the
// request with 502 instead of hanging it.
const jobRecordTimeout = 2 * time.Second

// maxJobBodyBytes bounds a submit request body so a large upload cannot make
// the gateway buffer without bound.
const maxJobBodyBytes = 1 << 20 // 1 MiB

// ErrJobNotFound is returned by a JobRecordStore when no record exists for an
// id, so handlers can map it to 404 separately from a transport error.
var ErrJobNotFound = errors.New("job record not found")

// JobRecordStore reads the durable job records workers write to etcd. The
// etcd-backed implementation is the production default; tests substitute a
// stub.
type JobRecordStore interface {
	GetJob(ctx context.Context, jobID string) (*protocol.JobRecord, error)
}

type etcdJobStore struct {
	client *etcd.Client
}

func (s *etcdJobStore) GetJob(ctx context.Context, jobID string) (*protocol.JobRecord, error) {
	resp, err := s.client.Get(ctx, protocol.JobKey(jobID))
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, ErrJobNotFound
	}

	var rec protocol.JobRecord
	if err := json.Unmarshal(resp.Kvs[0].Value, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

type JobsAPI struct {
	redis     *internal.RedisClient
	records   JobRecordStore
	authToken string
}

func NewJobsAPI(redisClient *internal.RedisClient, etcdClient *etcd.Client, authToken string) *JobsAPI {
	api := &JobsAPI{
		redis:     redisClient,
		authToken: authToken,
	}
	if etcdClient != nil {
		api.records = &etcdJobStore{client: etcdClient}
	}
	return api
}

func (api *JobsAPI) Routes() chi.Router {
	r := chi.NewRouter()

	// When AUTH_TOKEN is set, every jobs call must present it as a bearer
	// token; unset keeps the homelab-open behavior.
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

	r.Post("/", requireAuth(api.Submit))
	r.Get("/{id}", requireAuth(api.Get))
	r.Delete("/{id}", requireAuth(api.Cancel))
	return r
}

// POST /api/jobs
func (api *JobsAPI) Submit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID             string            `json:"id"`
		Mode           string            `json:"mode"`
		Runtime        string            `json:"runtime"`
		Command        []string          `json:"command"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		VCPU           int               `json:"vcpu"`
		MemoryMB       int               `json:"memory_mb"`
		WorkspaceMB    int               `json:"workspace_mb"`
		EnvVars        map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJobBodyBytes)).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.Runtime == "" {
		http.Error(w, "runtime is required", http.StatusBadRequest)
		return
	}
	if len(req.Command) == 0 {
		http.Error(w, "command is required and must not be empty", http.StatusBadRequest)
		return
	}
	if req.Mode != "" && req.Mode != jobModeProcess {
		http.Error(w, `unsupported mode: only "process" is supported`, http.StatusBadRequest)
		return
	}
	if req.WorkspaceMB < 0 {
		http.Error(w, "workspace_mb must not be negative", http.StatusBadRequest)
		return
	}
	if req.WorkspaceMB > protocol.MaxWorkspaceMB {
		http.Error(w, fmt.Sprintf("workspace_mb must be at most %d", protocol.MaxWorkspaceMB), http.StatusBadRequest)
		return
	}
	// vcpu and memory default to 1 and 128 when zero. A negative value would
	// otherwise reach the Firecracker machine configuration unchanged.
	if req.VCPU < 0 {
		http.Error(w, "vcpu must not be negative", http.StatusBadRequest)
		return
	}
	if req.MemoryMB < 0 {
		http.Error(w, "memory_mb must not be negative", http.StatusBadRequest)
		return
	}
	// Cancellation is best-effort pub/sub, so a job with no deadline could
	// still run unbounded if the cancel message is missed. Require an explicit
	// positive timeout rather than silently allowing an unbounded job.
	if req.TimeoutSeconds <= 0 {
		http.Error(w, "timeout_seconds must be positive", http.StatusBadRequest)
		return
	}
	if req.TimeoutSeconds > protocol.MaxTimeoutSeconds {
		http.Error(w, fmt.Sprintf("timeout_seconds must be at most %d", protocol.MaxTimeoutSeconds), http.StatusBadRequest)
		return
	}
	// Reject a caller-supplied id that could not be fetched back: the router
	// treats '/' as a path separator, so such a job could never be read, and
	// whitespace produces awkward etcd keys.
	if req.ID != "" {
		if err := protocol.ValidJobID(req.ID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	jobID := req.ID
	if jobID == "" {
		jobID = id.GenerateJobID()
	}

	job := &protocol.Job{
		RequestID:      id.GenerateRequestID(),
		JobID:          jobID,
		Mode:           jobModeProcess,
		Runtime:        req.Runtime,
		Command:        req.Command,
		TimeoutSeconds: req.TimeoutSeconds,
		VCPU:           req.VCPU,
		MemoryMB:       req.MemoryMB,
		WorkspaceMB:    req.WorkspaceMB,
		EnvVars:        req.EnvVars,
	}

	if err := api.redis.PushJob(job); err != nil {
		logger.Error("failed to publish job", "job_id", job.JobID, "error", err)
		http.Error(w, "failed to publish job", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"job_id":     job.JobID,
		"request_id": job.RequestID,
		"state":      protocol.JobStateProvisioning,
	})
}

// GET /api/jobs/{id}
func (api *JobsAPI) Get(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	if jobID == "" {
		http.Error(w, "job id is required", http.StatusBadRequest)
		return
	}
	if api.records == nil {
		http.Error(w, "job records unavailable", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), jobRecordTimeout)
	defer cancel()

	rec, err := api.records.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		logger.Error("failed to read job record", "job_id", jobID, "error", err)
		http.Error(w, "failed to read job record", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rec)
}

// isTerminalJobState reports whether a job has already reached a final state
// and therefore cannot be cancelled.
func isTerminalJobState(state string) bool {
	switch state {
	case protocol.JobStateDone, protocol.JobStateFailed, protocol.JobStateTimeout, protocol.JobStateCancelled:
		return true
	default:
		return false
	}
}

// DELETE /api/jobs/{id}
//
// Looks up the durable record, refuses to "cancel" a job that is already
// terminal, and otherwise publishes the id to the cancel channel. The worker
// owning the job stops the VM and writes the terminal `cancelled` record; this
// handler only acknowledges the request.
func (api *JobsAPI) Cancel(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "id")
	if jobID == "" {
		http.Error(w, "job id is required", http.StatusBadRequest)
		return
	}
	if api.records == nil {
		http.Error(w, "job records unavailable", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), jobRecordTimeout)
	defer cancel()

	rec, err := api.records.GetJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, ErrJobNotFound) {
			http.Error(w, "job not found", http.StatusNotFound)
			return
		}
		logger.Error("failed to read job record", "job_id", jobID, "error", err)
		http.Error(w, "failed to read job record", http.StatusBadGateway)
		return
	}

	// Returning 202 for a job that can no longer be stopped would mislead the
	// caller into thinking a cancel was delivered.
	if isTerminalJobState(rec.State) {
		http.Error(w, fmt.Sprintf("job is already %s and cannot be cancelled", rec.State), http.StatusConflict)
		return
	}

	if err := api.redis.PublishJobCancel(ctx, jobID); err != nil {
		logger.Error("failed to publish job cancel", "job_id", jobID, "error", err)
		http.Error(w, "failed to publish job cancellation", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"job_id": jobID,
		"state":  jobCancelRequestedState,
	})
}
