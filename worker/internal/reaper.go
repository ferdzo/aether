package internal

import (
	"aether/shared/logger"
	"context"
	"time"
)

type Reaper struct {
	worker              *Worker
	checkInterval       time.Duration
	scaleToOneAfter     time.Duration
	scaleToZeroAfter    time.Duration
	minInstancesPerFunc int
}

func NewReaper(worker *Worker) *Reaper {
	return &Reaper{
		worker:              worker,
		checkInterval:       10 * time.Second,
		scaleToOneAfter:     30 * time.Second,
		scaleToZeroAfter:    5 * time.Minute,
		minInstancesPerFunc: 1,
	}
}

func (r *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.check()
		}
	}
}

func (r *Reaper) check() {
	r.worker.mu.Lock()
	defer r.worker.mu.Unlock()

	for functionID, instances := range r.worker.instances {
		if len(instances) == 0 {
			continue
		}

		// Two-tier scale-down strategy:
		// 1. If multiple instances: scale idle ones to keep 1 hot (30s idle)
		// 2. If single instance: scale to zero only after 5-10 minutes

		if len(instances) > r.minInstancesPerFunc {
			// Multiple instances: aggressive scale-down to 1 hot
			r.scaleToOne(functionID, instances)
		} else if len(instances) == 1 {
			// Single instance: conservative scale to zero
			r.scaleToZero(functionID, instances[0])
		}
	}
}

func (r *Reaper) scaleToOne(functionID string, instances []*Instance) {
	// Keep 1 hot instance, stop others that have been idle
	for _, inst := range instances {
		if inst.GetActiveRequests() > 0 {
			continue // Skip if actively serving
		}

		idleTime := time.Since(inst.LastRequest())
		if idleTime > r.scaleToOneAfter {
			logger.Info("reaper scaling down to 1",
				"function", functionID,
				"instance", inst.ID,
				"idle_time", idleTime,
				"remaining_instances", len(instances)-1)

			go func(fid, iid string) {
				if err := r.worker.StopInstance(fid, iid); err != nil {
					logger.Error("failed to stop instance", "function", fid, "instance", iid, "error", err)
				}
			}(functionID, inst.ID)

			return
		}
	}
}

func (r *Reaper) scaleToZero(functionID string, inst *Instance) {
	if inst.GetActiveRequests() > 0 {
		return
	}

	idleTime := time.Since(inst.LastRequest())
	if idleTime > r.scaleToZeroAfter {
		logger.Info("reaper scaling to zero",
			"function", functionID,
			"instance", inst.ID,
			"idle_time", idleTime)

		go func(fid, iid string) {
			if err := r.worker.StopInstance(fid, iid); err != nil {
				logger.Error("failed to stop instance", "function", fid, "instance", iid, "error", err)
			}
		}(functionID, inst.ID)
	}
}
