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
	for functionID, instances := range s.worker.instances {
		s.checkFunction(functionID, instances)
	}

	// Drop invocation records for functions that are gone and cold.
	for functionID, last := range s.worker.lastInvoked {
		if _, live := s.worker.instances[functionID]; live {
			continue
		}
		if time.Since(last) > s.cfg.WarmWindow+s.cfg.ScaleToZeroAfter {
			delete(s.worker.lastInvoked, functionID)
		}
	}
	s.worker.mu.Unlock()
}

// shouldScaleToZero: everything idle past ScaleToZeroAfter AND the function
// outside its warm window — recently invoked functions hold MinInstances.
func (s *Scaler) shouldScaleToZero(functionID string, totalActive int64, minIdleDuration time.Duration) bool {
	if s.cfg.ScaleToZeroAfter <= 0 || totalActive != 0 || minIdleDuration <= s.cfg.ScaleToZeroAfter {
		return false
	}
	if s.cfg.WarmWindow > 0 {
		if last, ok := s.worker.LastInvoked(functionID); ok && time.Since(last) < s.cfg.WarmWindow {
			return false
		}
	}
	return true
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
	if s.shouldScaleToZero(functionID, totalActive, minIdleDuration) {
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
