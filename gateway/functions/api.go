package functions

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"aether/shared/builder"
	"aether/shared/db"
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/protocol"
	"aether/shared/storage"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

const (
	codeBucket    = "function-code"
	maxUploadSize = 50 << 20
)

type FunctionsAPI struct {
	db        *db.DB
	minio     *storage.Minio
	redis     *redis.Client
	loki      *LokiClient
	authToken string
}

func NewFunctionsAPI(database *db.DB, minio *storage.Minio, redisClient *redis.Client, loki *LokiClient, authToken string) *FunctionsAPI {
	return &FunctionsAPI{
		db:        database,
		minio:     minio,
		redis:     redisClient,
		loki:      loki,
		authToken: authToken,
	}
}

func (api *FunctionsAPI) Routes() chi.Router {
	r := chi.NewRouter()

	// When AUTH_TOKEN is set, every management call must present it as a
	// bearer token; unset keeps the homelab-open behavior.
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

	r.Get("/", requireAuth(api.List))
	r.Post("/", requireAuth(api.Create))
	r.Get("/{id}", requireAuth(api.Get))
	r.Put("/{id}", requireAuth(api.Update))
	r.Delete("/{id}", requireAuth(api.Delete))
	r.Post("/{id}/code", requireAuth(api.UploadCode))
	r.Get("/{id}/invocations", requireAuth(api.GetInvocations))
	r.Get("/{id}/logs", requireAuth(api.Logs))
	return r
}

// POST /api/functions
func (api *FunctionsAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string            `json:"name"`
		Runtime    string            `json:"runtime"`
		Entrypoint string            `json:"entrypoint"`
		VCPU       int               `json:"vcpu"`
		MemoryMB   int               `json:"memory_mb"`
		Port       int               `json:"port"`
		EnvVars    map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.Name == "" || req.Runtime == "" {
		http.Error(w, "name and runtime are required", http.StatusBadRequest)
		return
	}

	fn := &protocol.FunctionMetadata{
		ID:         id.GenerateFunctionID(),
		Name:       req.Name,
		Runtime:    req.Runtime,
		Entrypoint: req.Entrypoint,
		VCPU:       req.VCPU,
		MemoryMB:   req.MemoryMB,
		Port:       req.Port,
		EnvVars:    req.EnvVars,
	}

	if fn.VCPU == 0 {
		fn.VCPU = 1
	}
	if fn.MemoryMB == 0 {
		fn.MemoryMB = 128
	}

	if err := api.db.CreateFunction(fn); err != nil {
		logger.Error("failed to create function", "error", err)
		http.Error(w, "failed to create function", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(fn)
}

// GET /api/functions
func (api *FunctionsAPI) List(w http.ResponseWriter, r *http.Request) {
	functions, err := api.db.GetFunctions()
	if err != nil {
		logger.Error("failed to list functions", "error", err)
		http.Error(w, "failed to list functions", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(functions)
}

// GET /api/functions/{id}
func (api *FunctionsAPI) Get(w http.ResponseWriter, r *http.Request) {
	fnID := chi.URLParam(r, "id")

	fn, err := api.db.GetFunction(fnID)
	if err != nil {
		logger.Error("failed to get function", "error", err)
		http.Error(w, "failed to get function", http.StatusInternalServerError)
		return
	}
	if fn == nil {
		http.Error(w, "function not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(fn)
}

// PUT /api/functions/{id}
func (api *FunctionsAPI) Update(w http.ResponseWriter, r *http.Request) {
	fnID := chi.URLParam(r, "id")

	existing, err := api.db.GetFunction(fnID)
	if err != nil {
		logger.Error("failed to get function", "error", err)
		http.Error(w, "failed to get function", http.StatusInternalServerError)
		return
	}
	if existing == nil {
		http.Error(w, "function not found", http.StatusNotFound)
		return
	}

	var req struct {
		Name       string            `json:"name"`
		Runtime    string            `json:"runtime"`
		Entrypoint string            `json:"entrypoint"`
		VCPU       int               `json:"vcpu"`
		MemoryMB   int               `json:"memory_mb"`
		Port       int               `json:"port"`
		EnvVars    map[string]string `json:"env_vars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if req.Name != "" {
		existing.Name = req.Name
	}
	if req.Runtime != "" {
		existing.Runtime = req.Runtime
	}
	if req.Entrypoint != "" {
		existing.Entrypoint = req.Entrypoint
	}
	if req.VCPU > 0 {
		existing.VCPU = req.VCPU
	}
	if req.MemoryMB > 0 {
		existing.MemoryMB = req.MemoryMB
	}
	if req.Port > 0 {
		existing.Port = req.Port
	}
	if req.EnvVars != nil {
		existing.EnvVars = req.EnvVars
	}

	if err := api.db.UpdateFunction(existing); err != nil {
		logger.Error("failed to update function", "error", err)
		http.Error(w, "failed to update function", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(existing)
}

// DELETE /api/functions/{id}
func (api *FunctionsAPI) Delete(w http.ResponseWriter, r *http.Request) {
	fnID := chi.URLParam(r, "id")

	fn, err := api.db.GetFunction(fnID)
	if err != nil {
		logger.Error("failed to get function", "error", err)
		http.Error(w, "failed to get function", http.StatusInternalServerError)
		return
	}
	if fn == nil {
		http.Error(w, "function not found", http.StatusNotFound)
		return
	}

	if err := api.db.DeleteFunction(fnID); err != nil {
		logger.Error("failed to delete function", "error", err)
		http.Error(w, "failed to delete function", http.StatusInternalServerError)
		return
	}

	// Remove the stored code image and tell workers to stop live instances
	// and drop their cached copies. Best-effort: DB row is already gone.
	if fn.CodePath != "" {
		if err := api.minio.DeleteObject(codeBucket, fn.CodePath); err != nil {
			logger.Warn("failed to delete function artifact", "function", fnID, "path", fn.CodePath, "error", err)
		}
	}
	if err := api.redis.Publish(context.Background(), protocol.ChannelCodeUpdate, fnID).Err(); err != nil {
		logger.Warn("failed to publish function removal event", "function", fnID, "error", err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// POST /api/functions/{id}/code
func (api *FunctionsAPI) UploadCode(w http.ResponseWriter, r *http.Request) {
    fnID := chi.URLParam(r, "id")

    fn, err := api.db.GetFunction(fnID)
    if err != nil {
        logger.Error("failed to get function", "error", err)
        http.Error(w, "failed to get function", http.StatusInternalServerError)
        return
    }
    if fn == nil {
        http.Error(w, "function not found", http.StatusNotFound)
        return
    }

	// 1. Read uploaded archive (zip or tar.gz), capped to maxUploadSize.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	archiveData, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "upload exceeds size limit", http.StatusRequestEntityTooLarge)
			return
		}
		logger.Error("failed to read archive", "error", err)
		http.Error(w, "failed to read archive", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// 2. Get filename from X-Filename header, rejecting path tricks before
	// the archive type ever reaches the ext4 builder.
	filename := r.Header.Get("X-Filename")
	if filename == "" {
		filename = "code.zip"
	}
	if filepath.Base(filename) != filename || filename == "." || filename == string(filepath.Separator) {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}

    // 3. Build ext4 image from archive
    ext4Data, err := builder.BuildFromArchive(archiveData, filename)
    if err != nil {
        logger.Error("failed to build code image", "error", err)
        http.Error(w, "failed to build code image", http.StatusInternalServerError)
        return
    }

    // 4. Upload ext4 to MinIO (not the zip!)
    objectPath := fnID + "/code.ext4"
    if err := api.minio.PutObject(codeBucket, objectPath, ext4Data); err != nil {
        logger.Error("failed to upload code to minio", "error", err)
        http.Error(w, "failed to upload code", http.StatusInternalServerError)
        return
    }

    fn.CodePath = objectPath
    if err := api.db.UpdateFunction(fn); err != nil {
        logger.Error("failed to update function", "error", err)
        http.Error(w, "failed to update function", http.StatusInternalServerError)
        return
    }

    if err := api.redis.Publish(context.Background(), protocol.ChannelCodeUpdate, fnID).Err(); err != nil {
        logger.Warn("failed to publish code update event", "function", fnID, "error", err)
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]string{
        "message":   "code uploaded",
        "code_path": objectPath,
    })
}


// GET /api/functions/{id}/invocations
func (api *FunctionsAPI) GetInvocations(w http.ResponseWriter, r *http.Request) {
	fnID := chi.URLParam(r, "id")

	fn, err := api.db.GetFunction(fnID)
	if err != nil {
		logger.Error("failed to get function", "error", err)
		http.Error(w, "failed to get function", http.StatusInternalServerError)
		return
	}
	if fn == nil {
		http.Error(w, "function not found", http.StatusNotFound)
		return
	}

	invocations, err := api.db.GetInvocations(fnID, 100)
	if err != nil {
		logger.Error("failed to get invocations", "error", err)
		http.Error(w, "failed to get invocations", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(invocations)
}


// GET /api/functions/{id}/logs?since=1h&limit=200
func (api *FunctionsAPI) Logs(w http.ResponseWriter, r *http.Request) {
	fnID := chi.URLParam(r, "id")
	if api.loki == nil {
		http.Error(w, "log storage not configured", http.StatusNotImplemented)
		return
	}

	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	lines, err := api.loki.FunctionLogs(r.Context(), fnID, r.URL.Query().Get("since"), limit)
	if err != nil {
		logger.Error("failed to fetch function logs", "function", fnID, "error", err)
		http.Error(w, "failed to fetch logs", http.StatusBadGateway)
		return
	}
	if lines == nil {
		lines = []LogLine{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(lines)
}
