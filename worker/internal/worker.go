package internal

import (
	"aether/shared/logger"
	"aether/shared/network"
	"aether/shared/protocol"
	"aether/shared/vm"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

type Worker struct {
	cfg       *Config
	vmMgr     *vm.Manager
	bridgeMgr *network.BridgeManager
	instances map[string]*Instance
	mu        sync.Mutex
	nextPort  int
	registry  *Registry
}

func NewWorker(cfg *Config, registry *Registry) *Worker {
	return &Worker{
		cfg:       cfg,
		vmMgr:     vm.NewManager(cfg.FirecrackerBin),
		bridgeMgr: network.NewBridgeManager(cfg.BridgeName, cfg.BridgeCIDR),
		instances: make(map[string]*Instance),
		nextPort:  30000,
		registry:  registry,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if err := w.bridgeMgr.EnsureBridge(); err != nil {
		return fmt.Errorf("failed to ensure bridge: %w", err)
	}

	return w.watchQueue(ctx)
}

func (w *Worker) watchQueue(ctx context.Context) error {
	client, err := NewRedisClient(w.cfg.RedisAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to redis: %w", err)
	}

	logger.Info("watching queue", "queue", protocol.QueueVMProvision)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			result, err := client.BLPop(ctx, 5*time.Second, protocol.QueueVMProvision).Result()
			if err != nil {
				continue
			}
			if len(result) < 2 {
				continue
			}

			if err := w.handleJob([]byte(result[1])); err != nil {
				logger.Error("job failed", "error", err)
			}
		}
	}
}

func (w *Worker) handleJob(job []byte) error {
	var jobData protocol.Job
	if err := json.Unmarshal(job, &jobData); err != nil {
		return fmt.Errorf("failed to unmarshal job: %w", err)
	}

	log := logger.With("request_id", jobData.RequestID, "function", jobData.FunctionID)
	log.Info("received job")

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, exists := w.instances[jobData.FunctionID]; exists {
		log.Info("function already running")
		return nil
	}

	instance := NewInstance(jobData.FunctionID, w.vmMgr, w.bridgeMgr)

	cfg := InstanceConfig{
		KernelPath:   w.cfg.KernelPath,
		RuntimePath:  w.cfg.RuntimePath,
		CodePath:     filepath.Join(w.cfg.FunctionsDir, jobData.FunctionID+".ext4"),
		SocketPath:   filepath.Join(w.cfg.SocketDir, jobData.FunctionID+".sock"),
		VCPUCount:    int64(jobData.VCPU),
		MemSizeMB:    int64(jobData.MemoryMB),
		FunctionPort: w.cfg.FunctionPort,
	}

	if cfg.VCPUCount == 0 {
		cfg.VCPUCount = 1
	}
	if cfg.MemSizeMB == 0 {
		cfg.MemSizeMB = 128
	}

	if err := instance.Start(cfg); err != nil {
		return fmt.Errorf("failed to start instance: %w", err)
	}

	proxyPort := w.nextPort
	w.nextPort++

	if err := instance.WaitReady(w.cfg.FunctionPort, 30*time.Second); err != nil {
		instance.Stop()
		return fmt.Errorf("instance not ready: %w", err)
	}

	if err := instance.StartProxy(proxyPort, w.cfg.FunctionPort); err != nil {
		instance.Stop()
		return fmt.Errorf("failed to start proxy: %w", err)
	}

	w.instances[jobData.FunctionID] = instance

	log.Info("function started", "vm_ip", instance.GetVMIP(), "proxy_port", proxyPort)

	if err := w.registry.RegisterInstance(jobData.FunctionID, proxyPort, instance.vmIP); err != nil {
		return fmt.Errorf("failed to register instance: %w", err)
	}

	return nil
}

func (w *Worker) Shutdown() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for id, instance := range w.instances {
		logger.Info("stopping instance", "function", id)
		instance.Stop()
	}

	return nil
}

func (w *Worker) GetInstance(functionID string) (*Instance, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	inst, ok := w.instances[functionID]
	return inst, ok
}
