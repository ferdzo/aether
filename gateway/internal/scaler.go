package internal

import (
	"aether/shared/db"
	"aether/shared/logger"
	"aether/shared/protocol"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type Scaler struct {
	redis          *redis.Client
	discovery      *Discovery
	db             *db.DB
	activeRequests map[string]int64
	mu             sync.RWMutex
	checkInterval  time.Duration
	threshold      int
}

func NewScaler(redis *redis.Client, discovery *Discovery, database *db.DB) *Scaler {
	return &Scaler{
		redis:          redis,
		discovery:      discovery,
		db:             database,
		activeRequests: make(map[string]int64),
		checkInterval:  1 * time.Second,
		threshold:      3,
	}
}

func (s *Scaler) TrackRequest(functionID string, delta int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.activeRequests[functionID] += delta
	if s.activeRequests[functionID] < 0 {
		s.activeRequests[functionID] = 0
	}
}

func (s *Scaler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.check(ctx)
		}
	}
}

func (s *Scaler) check(ctx context.Context) {
	s.mu.RLock()
	snapshot := make(map[string]int64, len(s.activeRequests))
	for fnID, count := range s.activeRequests {
		snapshot[fnID] = count
	}
	s.mu.RUnlock()

	for functionID, activeCount := range snapshot {
		if activeCount == 0 {
			continue
		}

		// Get current instance count from etcd
		instances, err := s.discovery.GetInstances(ctx, functionID)
		if err != nil {
			logger.Error("failed to get instances", "function", functionID, "error", err)
			continue
		}

		currentCount := len(instances)

		// Calculate target instances: ceil(active / threshold)
		targetCount := int((activeCount + int64(s.threshold) - 1) / int64(s.threshold))

		if targetCount > currentCount {
			deficit := targetCount - currentCount
			logger.Info("gateway scaling up",
				"function", functionID,
				"active_requests", activeCount,
				"current_instances", currentCount,
				"target_instances", targetCount,
				"provisioning", deficit)

			// Push provision jobs to queue
			for i := 0; i < deficit; i++ {
				if err := s.pushProvisionJob(ctx, functionID); err != nil {
					logger.Error("failed to push provision job", "function", functionID, "error", err)
				}
			}
		}
	}
}

func (s *Scaler) pushProvisionJob(ctx context.Context, functionID string) error {
	fn, err := s.db.GetFunction(functionID)
	if err != nil {
		return err
	}

	job := protocol.Job{
		FunctionID: functionID,
		ImageID:    fn.CodePath,
		Runtime:    fn.Runtime,
		Entrypoint: fn.Entrypoint,
		VCPU:       int(fn.VCPU),
		MemoryMB:   int(fn.MemoryMB),
		Port:       fn.Port,
		EnvVars:    fn.EnvVars,
	}

	data, err := json.Marshal(job)
	if err != nil {
		return err
	}

	return s.redis.LPush(ctx, "queue:vm_provision", string(data)).Err()
}
