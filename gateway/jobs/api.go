package jobs

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
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

// jobRecordTimeout bounds a single etcd read so an unreachable etcd fails the
// request with 502 instead of hanging it.
const jobRecordTimeout = 2 * time.Second

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
	if len(req.Command) == 0 {
		http.Error(w, "command is required and must not be empty", http.StatusBadRequest)
		return
	}
	if req.Mode != "" && req.Mode != jobModeProcess {
		http.Error(w, `unsupported mode: only "process" is supported`, http.StatusBadRequest)
		return
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
