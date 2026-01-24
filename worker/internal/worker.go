package internal

import (
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/metrics"
	"aether/shared/network"
	"aether/shared/protocol"
	"aether/shared/system"
	"aether/shared/vm"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

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
	return otel.Tracer("aether-worker")
}

type FunctionConfig struct {
	Runtime    string
	Entrypoint string
	VCPU       int64
	MemMB      int64
	Port       int
	EnvVars    map[string]string
}

type Worker struct {
	cfg            *Config
	vmMgr          *vm.Manager
	bridgeMgr      *network.BridgeManager
	instances      map[string][]*Instance
	functionConfig map[string]FunctionConfig
	mu             sync.Mutex
	nextPort       int
	usedPorts      map[int]bool
	registry       *Registry
	codeCache      *CodeCache
	redis          *redis.Client
}

func NewWorker(cfg *Config, registry *Registry, codeCache *CodeCache, redisClient *redis.Client) *Worker {
	return &Worker{
		cfg:            cfg,
		vmMgr:          vm.NewManager(cfg.FirecrackerBin),
		bridgeMgr:      network.NewBridgeManager(cfg.BridgeName, cfg.BridgeCIDR),
		instances:      make(map[string][]*Instance),
		functionConfig: make(map[string]FunctionConfig),
		nextPort:       30000,
		usedPorts:      make(map[int]bool),
		registry:       registry,
		codeCache:      codeCache,
		redis:          redisClient,
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

	maxInstances := 10

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			w.mu.Lock()
			totalInstances := 0
			for _, insts := range w.instances {
				totalInstances += len(insts)
			}
			w.mu.Unlock()

			if totalInstances >= maxInstances {
				logger.Info("at capacity, waiting", "current", totalInstances, "max", maxInstances)
				time.Sleep(500 * time.Millisecond)
				continue
			}

			freeRAM, err := system.GetFreeRAM()
			if err == nil && freeRAM < 500 {
				logger.Info("low memory, waiting", "free_mb", freeRAM)
				time.Sleep(500 * time.Millisecond)
				continue
			}

			result, err := client.BLPop(ctx, 1*time.Second, protocol.QueueVMProvision).Result()
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

	ctx := context.Background()
	if jobData.TraceContext != nil {
		carrier := MapCarrier(jobData.TraceContext)
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}

	_, span := tracer().Start(ctx, "job.process")
	defer span.End()

	span.SetAttributes(
		attribute.String("request.id", jobData.RequestID),
		attribute.String("function.id", jobData.FunctionID),
		attribute.Int("instance.count", jobData.Count),
	)

	log := logger.With("request_id", jobData.RequestID, "function", jobData.FunctionID)
	log.Info("received job", "count", jobData.Count)

	w.mu.Lock()
	port := jobData.Port
	if port == 0 {
		port = 3000
	}
	entrypoint := jobData.Entrypoint
	if entrypoint == "" {
		entrypoint = "handler.js"
	}
	w.functionConfig[jobData.FunctionID] = FunctionConfig{
		Runtime:    jobData.Runtime,
		Entrypoint: entrypoint,
		VCPU:       int64(jobData.VCPU),
		MemMB:      int64(jobData.MemoryMB),
		Port:       port,
		EnvVars:    jobData.EnvVars,
	}
	w.mu.Unlock()

	count := jobData.Count
	if count <= 0 {
		count = 1
	}

	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.SpawnInstance(jobData.FunctionID); err != nil {
				log.Error("failed to spawn instance", "error", err)
			}
		}()
	}
	wg.Wait()

	return nil
}

func (w *Worker) SpawnInstance(functionID string) (*Instance, error) {
	_, span := tracer().Start(context.Background(), "instance.spawn")
	defer span.End()

	spawnStart := time.Now()
	instance := NewInstance(functionID, w.vmMgr, w.bridgeMgr)
	span.SetAttributes(
		attribute.String("function.id", functionID),
		attribute.String("instance.id", instance.ID),
	)
	log := logger.With("function", functionID, "instance", instance.ID)

	codePath, err := w.codeCache.EnsureCode(functionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get code: %w", err)
	}

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
	functionPort := fnCfg.Port
	if functionPort == 0 {
		functionPort = 3000
	}

	bootToken := id.GenerateToken()
	mmdsData := map[string]interface{}{
		"token":      bootToken,
		"env":        fnCfg.EnvVars,
		"entrypoint": fnCfg.Entrypoint,
		"port":       fnCfg.Port,
	}

	cfg := InstanceConfig{
		KernelPath:   w.cfg.KernelPath,
		RuntimePath:  w.cfg.RuntimePath,
		CodePath:     codePath,
		SocketPath:   filepath.Join(w.cfg.SocketDir, instance.ID+".sock"),
		VCPUCount:    vcpu,
		MemSizeMB:    memMB,
		FunctionPort: functionPort,
		BootToken:    bootToken,
		MMDSData:     mmdsData,
	}

	if err := instance.Start(cfg); err != nil {
		return nil, fmt.Errorf("failed to start instance: %w", err)
	}

	instance.SetVMDeathCallback(w.handleVMDeath)

	w.mu.Lock()
	proxyPort := w.allocatePort()
	metrics.PortsAllocated.Set(float64(len(w.usedPorts)))
	w.mu.Unlock()

	if err := instance.WaitReady(functionPort, 30*time.Second); err != nil {
		instance.Stop()
		return nil, fmt.Errorf("instance not ready: %w", err)
	}

	if err := instance.StartProxy(proxyPort, functionPort); err != nil {
		w.mu.Lock()
		w.releasePort(proxyPort)
		w.mu.Unlock()
		instance.Stop()
		return nil, fmt.Errorf("failed to start proxy: %w", err)
	}

	w.mu.Lock()
	w.instances[functionID] = append(w.instances[functionID], instance)
	metrics.InstancesActive.WithLabelValues(functionID, w.cfg.WorkerID).Set(float64(len(w.instances[functionID])))
	w.mu.Unlock()

	log.Info("instance started", "vm_ip", instance.GetVMIP(), "proxy_port", proxyPort)

	if err := w.registry.RegisterInstance(functionID, instance.ID, proxyPort, instance.vmIP); err != nil {
		log.Error("failed to register instance", "error", err)
	}

	metrics.VMSpawnsTotal.WithLabelValues(functionID, "success").Inc()
	metrics.VMSpawnDuration.WithLabelValues(functionID).Observe(time.Since(spawnStart).Seconds())

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
			w.releasePort(inst.GetProxyPort())
			metrics.PortsAllocated.Set(float64(len(w.usedPorts)))

			inst.Stop()
			w.instances[functionID] = append(instances[:i], instances[i+1:]...)
			metrics.InstancesActive.WithLabelValues(functionID, w.cfg.WorkerID).Set(float64(len(w.instances[functionID])))
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

func (w *Worker) WatchCodeUpdates(ctx context.Context) {
	pubsub := w.redis.Subscribe(ctx, protocol.ChannelCodeUpdate)
	defer pubsub.Close()

	logger.Info("watching for code updates", "channel", protocol.ChannelCodeUpdate)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-pubsub.Channel():
			functionID := msg.Payload
			logger.Info("code update received", "function", functionID)
			w.handleCodeUpdate(functionID)
		}
	}
}

func (w *Worker) handleCodeUpdate(functionID string) {
	if err := w.codeCache.Invalidate(functionID); err != nil {
		logger.Error("failed to invalidate cache", "function", functionID, "error", err)
	}

	w.mu.Lock()
	instances := w.instances[functionID]
	w.mu.Unlock()

	for _, inst := range instances {
		logger.Info("stopping instance for code update", "function", functionID, "instance", inst.ID)
		go func(id string) {
			if err := w.StopInstance(functionID, id); err != nil {
				logger.Error("failed to stop instance", "function", functionID, "instance", id, "error", err)
			}
		}(inst.ID)
	}

	w.mu.Lock()
	delete(w.functionConfig, functionID)
	w.mu.Unlock()
}

func (w *Worker) handleVMDeath(functionID, instanceID string) {
	log := logger.With("function", functionID, "instance", instanceID)
	log.Warn("handling VM death - cleaning up instance")

	metrics.VMDeathsTotal.WithLabelValues(functionID, "unexpected").Inc()

	if err := w.StopInstance(functionID, instanceID); err != nil {
		log.Error("failed to cleanup dead instance", "error", err)
	} else {
		log.Info("dead instance cleaned up successfully")
	}
}

func (w *Worker) allocatePort() int {
	for port := 30000; port < 65535; port++ {
		if !w.usedPorts[port] {
			w.usedPorts[port] = true
			logger.Debug("allocated port", "port", port)
			return port
		}
	}
	logger.Error("no available ports")
	return 30000
}
func (w *Worker) releasePort(port int) {
	if port > 0 {
		delete(w.usedPorts, port)
		logger.Debug("released port", "port", port)
	}
}
