package internal

import (
	"aether/shared/id"
	"aether/shared/logger"
	"aether/shared/metrics"
	"aether/shared/network"
	"aether/shared/protocol"
	"aether/shared/vm"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	redis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
	lastInvoked    map[string]time.Time
	mu             sync.Mutex
	nextPort       int
	usedPorts      map[int]bool
	registry       *Registry
	codeCache      *CodeCache
	runtimeCache   *RuntimeCache
	netnsMgr       *network.NetnsManager
	redis          *redis.Client
	consumerName   string
}

func NewWorker(cfg *Config, registry *Registry, codeCache *CodeCache, redisClient *redis.Client) *Worker {
	consumerName := cfg.WorkerID
	if consumerName == "" {
		consumerName = id.GetWorkerID()
	}
	return &Worker{
		cfg:            cfg,
		vmMgr:          vm.NewManager(cfg.FirecrackerBin),
		bridgeMgr:      network.NewBridgeManager(cfg.BridgeName, cfg.BridgeCIDR),
		instances:      make(map[string][]*Instance),
		functionConfig: make(map[string]FunctionConfig),
		lastInvoked:    make(map[string]time.Time),
		nextPort:       30000,
		usedPorts:      make(map[int]bool),
		registry:       registry,
		codeCache:      codeCache,
		redis:          redisClient,
		consumerName:   consumerName,
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if w.netnsMgr != nil {
		logger.Info("network mode", "mode", "netns")
	} else if err := w.bridgeMgr.EnsureBridge(); err != nil {
		return fmt.Errorf("failed to ensure bridge: %w", err)
	}

	if err := w.ensureStreamGroup(ctx); err != nil {
		return fmt.Errorf("failed to create consumer group: %w", err)
	}

	go w.claimStaleJobs(ctx)

	return w.watchQueue(ctx)
}

func (w *Worker) ensureStreamGroup(ctx context.Context) error {
	err := w.redis.XGroupCreateMkStream(ctx, protocol.StreamProvision, protocol.StreamGroup, "0").Err()
	if err != nil && strings.Contains(err.Error(), "BUSYGROUP") {
		return nil
	}
	return err
}

func (w *Worker) watchQueue(ctx context.Context) error {
	logger.Info("watching provision stream", "stream", protocol.StreamProvision, "group", protocol.StreamGroup)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		result, err := w.redis.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    protocol.StreamGroup,
			Consumer: w.consumerName,
			Streams:  []string{protocol.StreamProvision, ">"},
			Count:    1,
			Block:    5 * time.Second,
		}).Result()
		if err != nil {
			if ctx.Err() != nil || err == redis.Nil {
				continue
			}
			logger.Error("failed to read from provision stream", "error", err)
			time.Sleep(time.Second)
			continue
		}

		for _, stream := range result {
			for _, msg := range stream.Messages {
				w.processMessage(ctx, msg)
			}
		}
	}
}

var (
	staleClaimCheckEvery = 30 * time.Second
	staleClaimAfter      = 70 * time.Second // MUST exceed max spawn time (~30s WaitReady) or booting VMs get double-spawned
	staleClaimBatchSize  = int64(10)
)

func (w *Worker) claimStaleJobs(ctx context.Context) {
	ticker := time.NewTicker(staleClaimCheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		w.claimOnce(ctx)
	}
}

func (w *Worker) claimOnce(ctx context.Context) {
	pending, err := w.redis.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: protocol.StreamProvision,
		Group:  protocol.StreamGroup,
		Idle:   staleClaimAfter,
		Start:  "-",
		End:    "+",
		Count:  staleClaimBatchSize,
	}).Result()
	if err != nil {
		if ctx.Err() == nil && err != redis.Nil {
			logger.Error("failed to inspect pending entries", "error", err)
		}
		return
	}
	if len(pending) == 0 {
		return
	}

	ids := make([]string, 0, len(pending))
	for _, p := range pending {
		ids = append(ids, p.ID)
	}

	msgs, err := w.redis.XClaim(ctx, &redis.XClaimArgs{
		Stream:   protocol.StreamProvision,
		Group:    protocol.StreamGroup,
		Consumer: w.consumerName,
		MinIdle:  staleClaimAfter,
		Messages: ids,
	}).Result()
	if err != nil {
		logger.Error("failed to claim stale entries", "error", err)
		return
	}

	logger.Warn("claimed stale provision entries for reprocessing", "count", len(msgs), "idle_threshold", staleClaimAfter)
	metrics.StaleClaimsTotal.Add(float64(len(msgs)))

	for _, msg := range msgs {
		w.processMessage(ctx, msg)
	}
}

// processMessage acks only on success — a failed job stays pending for redelivery.
func (w *Worker) processMessage(ctx context.Context, msg redis.XMessage) {
	raw, ok := msg.Values["job"].(string)
	if !ok {
		// Unparseable jobs can never succeed; retrying would wedge the pending list.
		logger.Error("malformed stream message, dropping", "message_id", msg.ID, "values", msg.Values)
		metrics.PoisonJobsTotal.Inc()
		w.ackMessage(ctx, msg.ID)
		return
	}

	if err := w.handleJob([]byte(raw)); err != nil {
		logger.Error("job failed, leaving pending for redelivery", "message_id", msg.ID, "error", err)
		metrics.JobRetriesTotal.Inc()
		return
	}

	w.ackMessage(ctx, msg.ID)
}

func (w *Worker) ackMessage(ctx context.Context, id string) {
	if err := w.redis.XAck(ctx, protocol.StreamProvision, protocol.StreamGroup, id).Err(); err != nil {
		logger.Error("failed to ack message", "message_id", id, "error", err)
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

	// Idempotency guard: if another consumer already fulfilled this job
	// (e.g. it died mid-spawn and the entry was redelivered), skip spawning.
	if ready, err := w.registry.HasReadyInstance(jobData.FunctionID); err == nil && ready {
		log.Info("ready instance already registered, skipping spawn")
		span.SetAttributes(attribute.Bool("job.idempotent_skip", true))
		return nil
	}

	var wg sync.WaitGroup
	errCh := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.SpawnInstance(jobData.FunctionID); err != nil {
				log.Error("failed to spawn instance", "error", err)
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)

	var spawnErrs []error
	for err := range errCh {
		spawnErrs = append(spawnErrs, err)
	}
	if err := errors.Join(spawnErrs...); err != nil {
		return fmt.Errorf("instance spawn failed (%d/%d): %w", len(spawnErrs), count, err)
	}

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
	runtimeCache := w.runtimeCache
	w.mu.Unlock()

	rootfs := w.cfg.RuntimePath
	if runtimeCache != nil && fnCfg.Runtime != "" {
		if p, err := runtimeCache.Ensure(fnCfg.Runtime); err != nil {
			logger.Warn("runtime image unavailable, using configured rootfs", "runtime", fnCfg.Runtime, "error", err)
		} else {
			rootfs = p
		}
	}

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
		RuntimePath:  rootfs,
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
	instance.SetOnRequest(w.MarkInvoked)
	if w.netnsMgr != nil {
		instance.SetNetnsManager(w.netnsMgr)
	}

	w.mu.Lock()
	proxyPort := w.allocatePort()
	metrics.PortsAllocated.Set(float64(len(w.usedPorts)))
	w.mu.Unlock()

	if err := instance.WaitReady(functionPort, 30*time.Second); err != nil {
		w.mu.Lock()
		w.releasePort(proxyPort)
		metrics.PortsAllocated.Set(float64(len(w.usedPorts)))
		w.mu.Unlock()
		instance.Stop()
		return nil, fmt.Errorf("instance not ready: %w", err)
	}

	if err := instance.StartProxy(proxyPort, functionPort); err != nil {
		w.mu.Lock()
		w.releasePort(proxyPort)
		metrics.PortsAllocated.Set(float64(len(w.usedPorts)))
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

// MarkInvoked feeds the scaler's warm window: recently invoked functions keep MinInstances.
func (w *Worker) MarkInvoked(functionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastInvoked[functionID] = time.Now()
}

func (w *Worker) LastInvoked(functionID string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.lastInvoked[functionID]
	return t, ok
}

func (w *Worker) SetRuntimeCache(rc *RuntimeCache) { w.runtimeCache = rc }

func (w *Worker) SetNetnsManager(m *network.NetnsManager) { w.netnsMgr = m }

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
			// Unregister BEFORE draining so gateway traffic stops (~2s cache TTL) while in-flight requests finish.
			if err := w.registry.UnregisterInstance(functionID, instanceID); err != nil {
				logger.Error("failed to unregister instance", "function", functionID, "instance", instanceID, "error", err)
			}

			w.releasePort(inst.GetProxyPort())
			metrics.PortsAllocated.Set(float64(len(w.usedPorts)))

			inst.Stop()
			w.instances[functionID] = append(instances[:i], instances[i+1:]...)
			metrics.InstancesActive.WithLabelValues(functionID, w.cfg.WorkerID).Set(float64(len(w.instances[functionID])))
			if len(w.instances[functionID]) == 0 {
				delete(w.instances, functionID)
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

// allocatePort finds the next available port starting from 30000
// Must be called with w.mu held
func (w *Worker) allocatePort() int {
	for port := 30000; port < 65535; port++ {
		if !w.usedPorts[port] {
			w.usedPorts[port] = true
			logger.Debug("allocated port", "port", port)
			return port
		}
	}
	logger.Error("no available ports")
	return 30000 // fallback, will likely fail
}

// releasePort returns a port to the available pool
// Must be called with w.mu held
func (w *Worker) releasePort(port int) {
	if port > 0 {
		delete(w.usedPorts, port)
		logger.Debug("released port", "port", port)
	}
}
