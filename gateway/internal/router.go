package internal

import (
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
	coldStartSF singleflight.Group
}

func NewHandler(discovery *Discovery, redis *RedisClient) *Handler {
	return &Handler{
		discovery: discovery,
		redis:     redis,
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
		// Trigger cold start (spawns multiple instances)
		_, err = h.coldStart(ctx, funcID)
		if err != nil {
			http.Error(w, "failed to cold start function", http.StatusInternalServerError)
			return
		}

		// Wait for more instances to register (give parallel spawns time to complete)
		// Poll until we have at least 2 instances or timeout after 2s
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

	// Pick random instance from available pool
	instance := instances[rand.Intn(len(instances))]

	logger.Debug("Routing request to instance", "instance", instance.InstanceID, "function", funcID, "pool_size", len(instances))

	ProxyRequest(w, r, &instance)
}

const coldStartInstances = 3 // Spawn multiple instances on cold start to absorb burst

func (h *Handler) coldStart(ctx context.Context, funcID string) (protocol.FunctionInstance, error) {
	result, err, _ := h.coldStartSF.Do(funcID, func() (interface{}, error) {
		job := protocol.Job{
			RequestID:  id.GenerateRequestID(),
			FunctionID: funcID,
			VCPU:       1,
			MemoryMB:   128,
			Count:      coldStartInstances,
		}
		if err := h.redis.PushJob(&job); err != nil {
			return protocol.FunctionInstance{}, fmt.Errorf("failed to push job: %w", err)
		}

		instance, err := h.discovery.WaitForInstance(ctx, funcID, 30*time.Second)
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