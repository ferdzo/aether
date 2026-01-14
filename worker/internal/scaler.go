package internal

import (
	"aether/shared/logger"
	"context"
	"time"
)

type Scaler struct {
	worker *Worker
	cfg    *ScalingConfig
}

func NewScaler(worker *Worker, cfg *ScalingConfig) *Scaler {
	return &Scaler{worker: worker, cfg: cfg}
}

func (s *Scaler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.check()
		}
	}
}

func (s *Scaler) check() {
	s.worker.mu.Lock()
	defer s.worker.mu.Unlock()

	for functionID, instances := range s.worker.instances {
		s.checkFunction(functionID, instances)
	}
}

func (s *Scaler) checkFunction(functionID string, instances []*Instance) {
	if len(instances) == 0 {
		return
	}

	var totalActive int64
	var minIdleDuration time.Duration = -1
	for _, inst := range instances {
		totalActive += inst.GetActiveRequests()
		idle := inst.IdleDuration()
		if minIdleDuration < 0 || idle < minIdleDuration {
			minIdleDuration = idle
		}
	}
	avgConcurrency := float64(totalActive) / float64(len(instances))

	logger.Debug("scaler check", "function", functionID, "instances", len(instances), "total_active", totalActive, "avg", avgConcurrency, "min_idle", minIdleDuration)

	// Scale up
	if avgConcurrency > float64(s.cfg.ScaleUpThreshold) && len(instances) < s.cfg.MaxInstances {
		logger.Info("scaling up", "function", functionID, "instances", len(instances), "avg_concurrency", avgConcurrency)
		go func() {
			if _, err := s.worker.SpawnInstance(functionID); err != nil {
				logger.Error("failed to spawn instance", "function", functionID, "error", err)
			}
		}()
	}

	// Scale to zero: if ALL instances idle for ScaleToZeroAfter, kill everything
	if s.cfg.ScaleToZeroAfter > 0 && totalActive == 0 && minIdleDuration > s.cfg.ScaleToZeroAfter {
		logger.Info("scaling to zero", "function", functionID, "instances", len(instances), "idle_for", minIdleDuration)
		for _, inst := range instances {
			instID := inst.ID
			go func() {
				if err := s.worker.StopInstance(functionID, instID); err != nil {
					logger.Error("failed to stop instance", "function", functionID, "instance", instID, "error", err)
				}
			}()
		}
		return
	}

	// Normal scale down: keep MinInstances warm
	if len(instances) > s.cfg.MinInstances {
		for _, inst := range instances {
			if inst.GetActiveRequests() == 0 && inst.IdleDuration() > s.cfg.ScaleDownAfter {
				logger.Info("scaling down", "function", functionID, "instance", inst.ID, "idle_for", inst.IdleDuration())
				instID := inst.ID
				go func() {
					if err := s.worker.StopInstance(functionID, instID); err != nil {
						logger.Error("failed to stop instance", "function", functionID, "instance", instID, "error", err)
					}
				}()
				break
			}
		}
	}
}
