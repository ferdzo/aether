package functions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"aether/shared/builder"
	"aether/shared/db"
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/protocol"
	"aether/shared/storage"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
)

const codeBucket = "function-code"

type FunctionsAPI struct {
	db    *db.DB
	minio *storage.Minio
	redis *redis.Client
}

func NewFunctionsAPI(database *db.DB, minio *storage.Minio, redisClient *redis.Client) *FunctionsAPI {
	return &FunctionsAPI{
		db:    database,
		minio: minio,
		redis: redisClient,
	}
}

func (api *FunctionsAPI) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", api.List)
	r.Post("/", api.Create)
	r.Get("/{id}", api.Get)
	r.Put("/{id}", api.Update)
	r.Delete("/{id}", api.Delete)
	r.Post("/{id}/code", api.UploadCode)
	r.Get("/{id}/invocations", api.GetInvocations)
	return r
}

// POST /api/functions
func (api *FunctionsAPI) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string            `json:"name"`
		Runtime  string            `json:"runtime"`
		VCPU     int               `json:"vcpu"`
		MemoryMB int               `json:"memory_mb"`
		EnvVars  map[string]string `json:"env_vars"`
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
		ID:       id.GenerateFunctionID(),
		Name:     req.Name,
		Runtime:  req.Runtime,
		VCPU:     req.VCPU,
		MemoryMB: req.MemoryMB,
		EnvVars:  req.EnvVars,
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
		Name     string            `json:"name"`
		Runtime  string            `json:"runtime"`
		VCPU     int               `json:"vcpu"`
		MemoryMB int               `json:"memory_mb"`
		EnvVars  map[string]string `json:"env_vars"`
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
	if req.VCPU > 0 {
		existing.VCPU = req.VCPU
	}
	if req.MemoryMB > 0 {
		existing.MemoryMB = req.MemoryMB
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

    // 1. Read uploaded archive (zip or tar.gz)
    archiveData, err := io.ReadAll(r.Body)
    if err != nil {
        logger.Error("failed to read archive", "error", err)
        http.Error(w, "failed to read archive", http.StatusInternalServerError)
        return
    }
    defer r.Body.Close()

    // 2. Get filename from Content-Disposition or default to .zip
    filename := r.Header.Get("X-Filename")
    if filename == "" {
        filename = "code.zip"
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

