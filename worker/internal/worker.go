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

	if err := w.handleJob(ctx, []byte(raw)); err != nil {
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

func (w *Worker) handleJob(ctx context.Context, job []byte) error {
	var jobData protocol.Job
	if err := json.Unmarshal(job, &jobData); err != nil {
		return fmt.Errorf("failed to unmarshal job: %w", err)
	}

	// Provisioning is scoped to the caller's context (watchQueue/claimOnce), so
	// worker shutdown cancels in-flight spawns. Trace context from the job is
	// extracted into that same ctx. The VM lifetime is owned by vm.Manager and
	// is deliberately not tied to it.
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

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("job cancelled before provisioning: %w", err)
	}

	w.mu.Lock()
	port := jobData.Port
	if port == 0 {
		port = defaultFunctionPort
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
			if _, err := w.SpawnInstanceContext(ctx, jobData.FunctionID); err != nil {
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

const defaultFunctionPort = 3000

// resolveFunctionPort returns the single port used for MMDS, readiness and the
// proxy target. 0 (unset) resolves to the historical default; the function API
// and the default are unchanged.
func resolveFunctionPort(port int) int {
	if port > 0 {
		return port
	}
	return defaultFunctionPort
}

// buildMMDSData assembles the guest MMDS payload. The port is always the
// resolved port, never the raw (possibly 0) configured value.
func buildMMDSData(bootToken string, fnCfg FunctionConfig, dns []string) map[string]interface{} {
	data := map[string]interface{}{
		"token":      bootToken,
		"env":        fnCfg.EnvVars,
		"entrypoint": fnCfg.Entrypoint,
		"port":       resolveFunctionPort(fnCfg.Port),
	}
	if len(dns) > 0 {
		data["dns"] = dns
	}
	return data
}

// Test seams: unit tests override these to exercise the post-launch stages of
// provisioning (readiness, proxy, registration, cleanup) without firecracker
// or root. Production always uses the real implementation.
var (
	provisionStart = func(ctx context.Context, inst *Instance, cfg InstanceConfig) error {
		return inst.Start(cfg)
	}
	provisionWaitReady = func(ctx context.Context, inst *Instance, port int, timeout time.Duration) error {
		return inst.WaitReady(ctx, port, timeout)
	}
	provisionStartProxy = func(inst *Instance, listenPort, targetPort int) error {
		return inst.StartProxy(listenPort, targetPort)
	}
)

// SpawnInstance provisions a function instance on a background context. It is
// kept for callers (the scaler) that have no request-scoped context.
func (w *Worker) SpawnInstance(functionID string) (*Instance, error) {
	return w.SpawnInstanceContext(context.Background(), functionID)
}

// SpawnInstanceContext provisions and registers a VM for functionID.
//
// Context ownership: ctx governs provisioning only — code/runtime fetch,
// network setup, readiness wait and registration. Cancelling it aborts an
// in-flight spawn and cleans up the half-provisioned instance. It never owns
// the VM: vm.Manager.Launch derives its own context.Background()-rooted
// lifetime (shared/vm/vm.go), the *Instance keeps that VM alive after this
// function returns, and worker shutdown stops tracked instances explicitly
// rather than through this ctx.
func (w *Worker) SpawnInstanceContext(ctx context.Context, functionID string) (*Instance, error) {
	ctx, span := tracer().Start(ctx, "instance.spawn")
	defer span.End()

	spawnStart := time.Now()
	instance := NewInstance(functionID, w.vmMgr, w.bridgeMgr)
	span.SetAttributes(
		attribute.String("function.id", functionID),
		attribute.String("instance.id", instance.ID),
	)
	log := logger.With("function", functionID, "instance", instance.ID)

	// Callbacks must be installed before Start: a VM can exit immediately, and
	// monitorVM would otherwise find no death callback and leak its resources.
	instance.SetOnRequest(w.MarkInvoked)
	instance.SetNetnsManager(w.netnsMgr)
	// The closure captures the instance rather than its ID, so death cleanup
	// does not depend on w.instances membership (the instance is tracked only
	// after readiness + proxy succeed).
	instance.SetVMDeathCallback(func(_, _ string) {
		w.handleVMDeath(instance)
	})

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	codePath, err := w.codeCache.EnsureCode(functionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get code: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	vcpu, memMB := fnCfg.VCPU, fnCfg.MemMB
	if vcpu == 0 {
		vcpu = 1
	}
	if memMB == 0 {
		memMB = 128
	}
	functionPort := resolveFunctionPort(fnCfg.Port)

	bootToken := id.GenerateToken()
	mmdsData := buildMMDSData(bootToken, fnCfg, w.cfg.GuestDNS)

	cfg := InstanceConfig{
		KernelPath:   w.cfg.KernelPath,
		RuntimePath:  rootfs,
		Drives:       []vm.DriveSpec{{Path: codePath, ReadOnly: true}},
		SocketPath:   filepath.Join(w.cfg.SocketDir, instance.ID+".sock"),
		VCPUCount:    vcpu,
		MemSizeMB:    memMB,
		FunctionPort: functionPort,
		BootToken:    bootToken,
		MMDSData:     mmdsData,
	}

	if err := provisionStart(ctx, instance, cfg); err != nil {
		// A partial Start may already have provisioned network; Stop is safe
		// after a partial start and no proxy port has been allocated yet.
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("failed to start instance: %w", err)
	}
	if instance.isStopped() {
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("instance %s stopped during provisioning", instance.ID)
	}

	if err := provisionWaitReady(ctx, instance, functionPort, 30*time.Second); err != nil {
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("instance not ready: %w", err)
	}
	if instance.isStopped() {
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("instance %s stopped during provisioning", instance.ID)
	}

	w.mu.Lock()
	proxyPort := w.allocatePort()
	metrics.PortsAllocated.Set(float64(len(w.usedPorts)))
	w.mu.Unlock()
	// Record ownership before StartProxy so every failure path can release it.
	instance.setProxyPort(proxyPort)

	if err := provisionStartProxy(instance, proxyPort, functionPort); err != nil {
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("failed to start proxy: %w", err)
	}
	if instance.isStopped() {
		// The VM died while the proxy was starting; the second Stop inside
		// cleanup tears down the proxy server created above.
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("instance %s stopped during provisioning", instance.ID)
	}

	w.mu.Lock()
	w.instances[functionID] = append(w.instances[functionID], instance)
	metrics.InstancesActive.WithLabelValues(functionID, w.cfg.WorkerID).Set(float64(len(w.instances[functionID])))
	w.mu.Unlock()

	log.Info("instance started", "vm_ip", instance.GetVMIP(), "proxy_port", proxyPort)

	if err := ctx.Err(); err != nil {
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("provisioning cancelled before registration: %w", err)
	}
	if err := w.registry.RegisterInstance(functionID, instance.ID, proxyPort, instance.GetVMIP()); err != nil {
		// Registration is part of provisioning: a VM that is up but not
		// registered must not be left running. Tear it down so the stream
		// entry stays pending and is reclaimed.
		log.Error("failed to register instance", "error", err)
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("failed to register instance: %w", err)
	}
	instance.markRegistered()
	if instance.isStopped() {
		// The VM died while registering; ensure the etcd key is removed before
		// reporting failure so the entry is reclaimed cleanly.
		w.cleanupInstance(functionID, instance)
		return nil, fmt.Errorf("instance %s stopped during provisioning", instance.ID)
	}

	metrics.VMSpawnsTotal.WithLabelValues(functionID, "success").Inc()
	metrics.VMSpawnDuration.WithLabelValues(functionID).Observe(time.Since(spawnStart).Seconds())

	return instance, nil
}

// releaseInstancePort releases the proxy port allocated for an instance exactly
// once, whether or not the instance is tracked in w.instances.
func (w *Worker) releaseInstancePort(inst *Instance) {
	if inst == nil || !inst.takePortOwnership() {
		return
	}
	port := inst.GetProxyPort()

	w.mu.Lock()
	w.releasePort(port)
	metrics.PortsAllocated.Set(float64(len(w.usedPorts)))
	w.mu.Unlock()
}

// detachInstance removes inst from the worker's bookkeeping. Returns true when
// the instance was tracked. Safe to call when the instance was never tracked.
func (w *Worker) detachInstance(functionID string, inst *Instance) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	instances, ok := w.instances[functionID]
	if !ok {
		return false
	}
	for idx, cur := range instances {
		if cur != inst {
			continue
		}
		w.instances[functionID] = append(instances[:idx], instances[idx+1:]...)
		if len(w.instances[functionID]) == 0 {
			delete(w.instances, functionID)
		}
		metrics.InstancesActive.WithLabelValues(functionID, w.cfg.WorkerID).Set(float64(len(w.instances[functionID])))
		return true
	}
	return false
}

// cleanupInstance is the single cleanup entry point for stopped, dead and
// failed instances. It unregisters, stops (idempotently), releases the proxy
// port and drops bookkeeping. It is safe to call more than once and for
// instances that were never tracked.
func (w *Worker) cleanupInstance(functionID string, inst *Instance) {
	if inst == nil {
		return
	}

	if inst.takeRegistered() {
		if err := w.registry.UnregisterInstance(functionID, inst.ID); err != nil {
			logger.Error("failed to unregister instance", "function", functionID, "instance", inst.ID, "error", err)
		}
	}

	if err := inst.Stop(); err != nil {
		logger.Error("failed to stop instance", "function", functionID, "instance", inst.ID, "error", err)
	}

	w.releaseInstancePort(inst)
	w.detachInstance(functionID, inst)
}

func (w *Worker) Shutdown() error {
	w.mu.Lock()
	all := make([]*Instance, 0)
	for _, instances := range w.instances {
		all = append(all, instances...)
	}
	w.mu.Unlock()

	for _, inst := range all {
		logger.Info("stopping instance", "function", inst.FunctionID, "instance", inst.ID)
		w.cleanupInstance(inst.FunctionID, inst)
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
	instances, ok := w.instances[functionID]
	if !ok {
		w.mu.Unlock()
		return fmt.Errorf("function %s not found", functionID)
	}
	var inst *Instance
	for _, cur := range instances {
		if cur.ID == instanceID {
			inst = cur
			break
		}
	}
	w.mu.Unlock()

	if inst == nil {
		return fmt.Errorf("instance %s not found", instanceID)
	}

	// Unregister happens inside cleanupInstance before the proxy is drained, so
	// gateway traffic stops (~2s cache TTL) while in-flight requests finish.
	w.cleanupInstance(functionID, inst)
	return nil
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

// handleVMDeath is the exactly-once cleanup owner for a VM that exited without
// a Stop request. It works whether or not the instance is tracked in
// w.instances, so a VM that dies during provisioning is still fully cleaned up.
func (w *Worker) handleVMDeath(inst *Instance) {
	log := logger.With("function", inst.FunctionID, "instance", inst.ID)
	log.Warn("handling VM death - cleaning up instance")

	metrics.VMDeathsTotal.WithLabelValues(inst.FunctionID, "unexpected").Inc()
	w.cleanupInstance(inst.FunctionID, inst)
	log.Info("dead instance cleaned up")
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
