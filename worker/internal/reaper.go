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
		if len(instances) > r.minInstancesPerFunc {
			r.scaleToOne(functionID, instances)
		} else if len(instances) == 1 {
			r.scaleToZero(functionID, instances[0])
		}
	}
}

func (r *Reaper) scaleToOne(functionID string, instances []*Instance) {
	for _, inst := range instances {
		if inst.GetActiveRequests() > 0 {
			continue
		}

		if inst.shuttingDown.Load() {
			continue
		}

		idleTime := time.Since(inst.LastRequest())
		if idleTime > r.scaleToOneAfter {
			inst.shuttingDown.Store(true)

			remainingInstances := len(instances) - 1

			logger.Info("reaper scaling down to 1",
				"function", functionID,
				"instance", inst.ID,
				"idle_time", idleTime,
				"remaining_instances", remainingInstances)

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

	// Skip if already shutting down
	if inst.shuttingDown.Load() {
		return
	}

	idleTime := time.Since(inst.LastRequest())
	if idleTime > r.scaleToZeroAfter {
		// Mark as shutting down to prevent concurrent shutdown attempts
		inst.shuttingDown.Store(true)

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
