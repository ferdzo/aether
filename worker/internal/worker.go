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

type FunctionConfig struct {
	VCPU  int64
	MemMB int64
}

type Worker struct {
	cfg            *Config
	vmMgr          *vm.Manager
	bridgeMgr      *network.BridgeManager
	instances      map[string][]*Instance
	functionConfig map[string]FunctionConfig
	mu             sync.Mutex
	nextPort       int
	registry       *Registry
}

func NewWorker(cfg *Config, registry *Registry) *Worker {
	return &Worker{
		cfg:            cfg,
		vmMgr:          vm.NewManager(cfg.FirecrackerBin),
		bridgeMgr:      network.NewBridgeManager(cfg.BridgeName, cfg.BridgeCIDR),
		instances:      make(map[string][]*Instance),
		functionConfig: make(map[string]FunctionConfig),
		nextPort:       30000,
		registry:       registry,
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
	if _, exists := w.functionConfig[jobData.FunctionID]; !exists {
		w.functionConfig[jobData.FunctionID] = FunctionConfig{
			VCPU:  int64(jobData.VCPU),
			MemMB: int64(jobData.MemoryMB),
		}
	}
	w.mu.Unlock()

	_, err := w.SpawnInstance(jobData.FunctionID)
	return err
}

func (w *Worker) SpawnInstance(functionID string) (*Instance, error) {
	instance := NewInstance(functionID, w.vmMgr, w.bridgeMgr)
	log := logger.With("function", functionID, "instance", instance.ID)

	w.mu.Lock()
	fnCfg := w.functionConfig[functionID]
	w.mu.Unlock()

	vcpu, memMB := fnCfg.VCPU, fnCfg.MemMB
	if vcpu == 0 {
		vcpu = 1
	}
	if memMB == 0 {
		memMB = 128
	}

	cfg := InstanceConfig{
		KernelPath:   w.cfg.KernelPath,
		RuntimePath:  w.cfg.RuntimePath,
		CodePath:     filepath.Join(w.cfg.FunctionsDir, functionID+".ext4"),
		SocketPath:   filepath.Join(w.cfg.SocketDir, instance.ID+".sock"),
		VCPUCount:    vcpu,
		MemSizeMB:    memMB,
		FunctionPort: w.cfg.FunctionPort,
	}

	if err := instance.Start(cfg); err != nil {
		return nil, fmt.Errorf("failed to start instance: %w", err)
	}

	w.mu.Lock()
	proxyPort := w.nextPort
	w.nextPort++
	w.mu.Unlock()

	if err := instance.WaitReady(w.cfg.FunctionPort, 30*time.Second); err != nil {
		instance.Stop()
		return nil, fmt.Errorf("instance not ready: %w", err)
	}

	if err := instance.StartProxy(proxyPort, w.cfg.FunctionPort); err != nil {
		instance.Stop()
		return nil, fmt.Errorf("failed to start proxy: %w", err)
	}

	w.mu.Lock()
	w.instances[functionID] = append(w.instances[functionID], instance)
	w.mu.Unlock()

	log.Info("instance started", "vm_ip", instance.GetVMIP(), "proxy_port", proxyPort)

	if err := w.registry.RegisterInstance(functionID, instance.ID, proxyPort, instance.vmIP); err != nil {
		log.Error("failed to register instance", "error", err)
	}

	return instance, nil
}

func (w *Worker) Shutdown() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for functionID, instances := range w.instances {
		for _, inst := range instances {
			logger.Info("stopping instance", "function", functionID, "instance", inst.ID)
			inst.Stop()
		}
	}

	return nil
}

func (w *Worker) GetInstances(functionID string) ([]*Instance, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	instances, ok := w.instances[functionID]
	return instances, ok && len(instances) > 0
}

func (w *Worker) InstanceCount(functionID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.instances[functionID])
}

func (w *Worker) TotalInstances() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := 0
	for _, instances := range w.instances {
		count += len(instances)
	}
	return count
}

func (w *Worker) StopInstance(functionID, instanceID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	instances, ok := w.instances[functionID]
	if !ok {
		return fmt.Errorf("function %s not found", functionID)
	}

	for i, inst := range instances {
		if inst.ID == instanceID {
			inst.Stop()
			w.instances[functionID] = append(instances[:i], instances[i+1:]...)
			if len(w.instances[functionID]) == 0 {
				delete(w.instances, functionID)
			}
			if err := w.registry.UnregisterInstance(functionID, instanceID); err != nil {
				logger.Error("failed to unregister instance", "function", functionID, "instance", instanceID, "error", err)
			}
			return nil
		}
	}

	return fmt.Errorf("instance %s not found", instanceID)
}
