package internal

import (
	"aether/shared/db"
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/metrics"
	"aether/shared/protocol"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"
)

// MapCarrier implements propagation.TextMapCarrier for a map
type MapCarrier map[string]string

func (c MapCarrier) Get(key string) string { return c[key] }
func (c MapCarrier) Set(key, value string) { c[key] = value }
func (c MapCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

func tracer() trace.Tracer {
	return otel.Tracer("aether-gateway")
}

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
	ctx, span := tracer().Start(r.Context(), "function.invoke")
	defer span.End()

	span.SetAttributes(attribute.String("function.id", funcID))
	startTime := time.Now()
	invID := id.GenerateInvocationID()
	span.SetAttributes(attribute.String("invocation.id", invID))

	metrics.ActiveRequests.Inc()
	defer metrics.ActiveRequests.Dec()

	discoveryStart := time.Now()
	instances, err := h.discovery.GetInstances(ctx, funcID)
	metrics.InstanceDiscoveryDuration.Observe(time.Since(discoveryStart).Seconds())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		h.recordInvocation(invID, funcID, "error", startTime, err.Error())
		metrics.RecordInvocation(funcID, "error", time.Since(startTime))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if len(instances) == 0 {
		fn, err := h.db.GetFunction(funcID)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "failed to get function")
			h.recordInvocation(invID, funcID, "error", startTime, err.Error())
			metrics.RecordInvocation(funcID, "error", time.Since(startTime))
			logger.Error("failed to get function", "function", funcID, "error", err)
			http.Error(w, "failed to get function", http.StatusInternalServerError)
			return
		}
		if fn == nil {
			span.SetStatus(codes.Error, "function not found")
			h.recordInvocation(invID, funcID, "error", startTime, "function not found")
			metrics.RecordInvocation(funcID, "error", time.Since(startTime))
			http.Error(w, "function not found", http.StatusNotFound)
			return
		}
		if fn.CodePath == "" {
			span.SetStatus(codes.Error, "function code not uploaded")
			h.recordInvocation(invID, funcID, "error", startTime, "function code not uploaded")
			metrics.RecordInvocation(funcID, "error", time.Since(startTime))
			http.Error(w, "function code not uploaded", http.StatusBadRequest)
			return
		}

		_, coldSpan := tracer().Start(ctx, "function.coldstart")
		metrics.ColdStartsTotal.WithLabelValues(funcID).Inc()
		instance, err := h.coldStart(ctx, fn)
		coldSpan.End()
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "cold start failed")
			h.recordInvocation(invID, funcID, "error", startTime, err.Error())
			metrics.RecordInvocation(funcID, "error", time.Since(startTime))
			logger.Error("cold start failed", "function", funcID, "error", err)
			http.Error(w, "failed to cold start function", http.StatusInternalServerError)
			return
		}

		instances = []protocol.FunctionInstance{instance}
	}

	instance := instances[rand.Intn(len(instances))]
	span.SetAttributes(attribute.String("instance.id", instance.InstanceID))

	ProxyRequest(w, r, &instance, funcID)
	h.recordInvocation(invID, funcID, "success", startTime, "")
	metrics.RecordInvocation(funcID, "success", time.Since(startTime))
}

func (h *Handler) recordInvocation(invID, funcID, status string, startTime time.Time, errMsg string) {
	go func() {
		inv := &protocol.Invocation{
			ID:           invID,
			FunctionID:   funcID,
			Status:       status,
			DurationMS:   int(time.Since(startTime).Milliseconds()),
			StartedAt:    startTime,
			ErrorMessage: errMsg,
		}
		if err := h.db.CreateInvocation(inv); err != nil {
			logger.Warn("failed to record invocation", "error", err)
		}
	}()
}

const coldStartInstances = 1

func (h *Handler) coldStart(ctx context.Context, fn *protocol.FunctionMetadata) (protocol.FunctionInstance, error) {
	result, err, _ := h.coldStartSF.Do(fn.ID, func() (interface{}, error) {
		// Inject trace context into job for distributed tracing
		traceCtx := make(MapCarrier)
		otel.GetTextMapPropagator().Inject(ctx, traceCtx)

		job := protocol.Job{
			RequestID:    id.GenerateRequestID(),
			FunctionID:   fn.ID,
			Runtime:      fn.Runtime,
			Entrypoint:   fn.Entrypoint,
			VCPU:         fn.VCPU,
			MemoryMB:     fn.MemoryMB,
			Port:         fn.Port,
			EnvVars:      fn.EnvVars,
			Count:        coldStartInstances,
			TraceContext: traceCtx,
		}
		if err := h.redis.PushJob(&job); err != nil {
			metrics.QueuePublishErrors.Inc()
			return protocol.FunctionInstance{}, fmt.Errorf("failed to push job: %w", err)
		}

		instance, err := h.discovery.WaitForInstance(ctx, fn.ID, 10*time.Second)
		if err != nil {
			return protocol.FunctionInstance{}, fmt.Errorf("cold start failed: %w", err)
		}
		return instance, nil
	})

	if err != nil {
		return protocol.FunctionInstance{}, err
	}
	return result.(protocol.FunctionInstance), nil
}
