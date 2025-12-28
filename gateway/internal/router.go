package internal

import (
	"aether/shared/db"
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/protocol"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sync/singleflight"
)

type Handler struct {
	discovery   *Discovery
	redis       *RedisClient
	db          *db.DB
	coldStartSF singleflight.Group
}

func NewHandler(discovery *Discovery, redis *RedisClient, database *db.DB) *Handler {
	return &Handler{
		discovery: discovery,
		redis:     redis,
		db:        database,
	}
}

func (h *Handler) Handler(w http.ResponseWriter, r *http.Request) {
	funcID := chi.URLParam(r, "funcID")
	ctx := r.Context()

	instances, err := h.discovery.GetInstances(ctx, funcID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(instances) == 0 {
		fn, err := h.db.GetFunction(funcID)
		if err != nil {
			logger.Error("failed to get function", "function", funcID, "error", err)
			http.Error(w, "failed to get function", http.StatusInternalServerError)
			return
		}
		if fn == nil {
			http.Error(w, "function not found", http.StatusNotFound)
			return
		}
		if fn.CodePath == "" {
			http.Error(w, "function code not uploaded", http.StatusBadRequest)
			return
		}

		_, err = h.coldStart(ctx, fn)
		if err != nil {
			logger.Error("cold start failed", "function", funcID, "error", err)
			http.Error(w, "failed to cold start function", http.StatusInternalServerError)
			return
		}

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			instances, _ = h.discovery.GetInstances(ctx, funcID)
			if len(instances) >= 2 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		if len(instances) == 0 {
			http.Error(w, "no instances available after cold start", http.StatusServiceUnavailable)
			return
		}
	}

	instance := instances[rand.Intn(len(instances))]

	logger.Debug("Routing request to instance", "instance", instance.InstanceID, "function", funcID, "pool_size", len(instances))

	ProxyRequest(w, r, &instance)
}

const coldStartInstances = 1

func (h *Handler) coldStart(ctx context.Context, fn *protocol.FunctionMetadata) (protocol.FunctionInstance, error) {
	result, err, _ := h.coldStartSF.Do(fn.ID, func() (interface{}, error) {
		job := protocol.Job{
			RequestID:  id.GenerateRequestID(),
			FunctionID: fn.ID,
			VCPU:       fn.VCPU,
			MemoryMB:   fn.MemoryMB,
			Port:       fn.Port,
			Count:      coldStartInstances,
		}
		if err := h.redis.PushJob(&job); err != nil {
			return protocol.FunctionInstance{}, fmt.Errorf("failed to push job: %w", err)
		}

		instance, err := h.discovery.WaitForInstance(ctx, fn.ID, 30*time.Second)
		if err != nil {
			return protocol.FunctionInstance{}, fmt.Errorf("failed to wait for instance: %w", err)
		}
		return instance, nil
	})

	if err != nil {
		return protocol.FunctionInstance{}, err
	}
	return result.(protocol.FunctionInstance), nil
}