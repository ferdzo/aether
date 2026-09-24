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
	"strconv"
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
	executions     map[string]*Execution
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

	// jobs maps a process-job id to its running runner so a cancel request can
	// reach the exact runner. It is guarded by mu. Job runners are deliberately
	// kept out of instances: jobs are invisible to the scaler and never
	// registered as function instances.
	jobs map[string]*JobRunner

	// shuttingDown is set once shutdown starts; registerExecution refuses new
	// executions after it so a VM cannot be created concurrently with teardown.
	shuttingDown bool
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
		executions:     make(map[string]*Execution),
		functionConfig: make(map[string]FunctionConfig),
		lastInvoked:    make(map[string]time.Time),
		nextPort:       30000,
		usedPorts:      make(map[int]bool),
		registry:       registry,
		codeCache:      codeCache,
		redis:          redisClient,
		consumerName:   consumerName,
		jobs:           make(map[string]*JobRunner),
	}
}

func (w *Worker) Run(ctx context.Context) error {
	if w.cfg != nil {
		w.gcWorkspaces()
	}
	if w.cfg != nil && w.cfg.NoNetwork {
		// Offline mode: instances boot with no NIC, so there is no bridge (or
		// netns) to set up and no host privileges are needed.
		logger.Info("networking disabled, skipping bridge setup", "mode", "none")
	} else if w.netnsMgr != nil {
		logger.Info("network mode", "mode", "netns")
	} else if err := w.bridgeMgr.EnsureBridge(); err != nil {
		return fmt.Errorf("failed to ensure bridge: %w", err)
	}

	if err := w.ensureStreamGroup(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("failed to create consumer group: %w", err)
	}

	// Reconcile after the consumer group exists but before consuming: records
	// left by a previous worker process are marked terminal so a redelivery
	// cannot adopt a dead execution.
	if w.cfg != nil {
		w.reconcileExecutions(ctx)
	}

	go w.claimStaleJobs(ctx)

	return w.watchQueue(ctx)
}

// gcWorkspaces sweeps expired workspace images once at startup. It is
// deliberately best-effort: a GC failure must never stop the worker from
// serving jobs, and a fresh worker with no WorkspaceDir has nothing to sweep.
func (w *Worker) gcWorkspaces() {
	// Never sweep a workspace belonging to a non-terminal execution: the TTL
	// alone is not enough for a long-lived execution. The predicate is derived
	// from the durable records; if the scan fails we fall back to TTL-only GC
	// (best effort) rather than skipping GC entirely.
	var keep func(string) bool
	if w.registry != nil && w.cfg.WorkspaceTTL > 0 {
		sctx, cancel := context.WithTimeout(context.Background(), executionScanTimeout)
		defer cancel()
		if recs, err := listExecutions(w.registry, sctx); err == nil {
			live := make(map[string]bool)
			for _, rec := range recs {
				switch rec.State {
				case protocol.ExecutionStateCreating, protocol.ExecutionStateReady, protocol.ExecutionStateStopping:
				default:
					continue
				}
				if rec.WorkspacePath != "" {
					live[filepath.Base(rec.WorkspacePath)] = true
				} else if rec.ID != "" {
					live[rec.ID+workspaceImageSuffix] = true
				}
			}
			if len(live) > 0 {
				keep = func(name string) bool { return live[name] }
			}
		} else {
			logger.Warn("workspace GC could not cross-check live executions; falling back to TTL-only", "error", err)
		}
	}

	removed, err := GCWorkspacesExcept(w.cfg.WorkspaceDir, w.cfg.WorkspaceTTL, keep)
	switch {
	case err != nil:
		logger.Warn("workspace GC completed with errors", "dir", w.cfg.WorkspaceDir, "removed", removed, "error", err)
	case removed > 0:
		logger.Info("workspace GC removed expired workspaces", "dir", w.cfg.WorkspaceDir, "removed", removed)
	default:
		logger.Debug("workspace GC found nothing to remove", "dir", w.cfg.WorkspaceDir)
	}
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
	// dlqScanBatchSize bounds how many pending entries the delivery-cap pass
	// inspects per tick.
	dlqScanBatchSize = int64(100)
)

// dlqAdd writes one entry to the dead-letter stream. It is a seam so tests can
// exercise the write-failure path, which must leave the source entry pending.
var dlqAdd = func(ctx context.Context, client *redis.Client, values map[string]interface{}) (string, error) {
	return client.XAdd(ctx, &redis.XAddArgs{Stream: protocol.StreamProvisionDLQ, Values: values}).Result()
}

func (w *Worker) claimStaleJobs(ctx context.Context) {
	ticker := time.NewTicker(staleClaimCheckEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Order matters: shed exhausted entries before the reaper re-claims
		// them, then reclaim stale entries, then bound the stream.
		w.moveExhaustedToDLQ(ctx)
		w.claimOnce(ctx)
		w.trimStream(ctx)
	}
}

// maxDeliveries returns the configured delivery cap; <= 0 disables it.
func (w *Worker) maxDeliveries() int {
	if w.cfg == nil {
		return 0
	}
	return w.cfg.MaxDeliveries
}

// streamMaxLen returns the configured approximate stream bound; <= 0 disables.
func (w *Worker) streamMaxLen() int64 {
	if w.cfg == nil {
		return 0
	}
	return w.cfg.StreamMaxLen
}

// moveExhaustedToDLQ is the bounded poison-entry escape hatch. It is a separate
// pass alongside claimOnce: XPENDING exposes the per-entry delivery count but
// cannot filter by it, so the count is filtered client-side. Entries whose
// delivery count has exceeded the cap are copied to the DLQ and ACKed so they
// stop looping; a DLQ write failure leaves the entry pending so it retries.
func (w *Worker) moveExhaustedToDLQ(ctx context.Context) {
	max := w.maxDeliveries()
	if max <= 0 {
		return
	}

	pending, err := w.redis.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: protocol.StreamProvision,
		Group:  protocol.StreamGroup,
		Idle:   staleClaimAfter,
		Start:  "-",
		End:    "+",
		Count:  dlqScanBatchSize,
	}).Result()
	if err != nil {
		if ctx.Err() == nil && err != redis.Nil {
			logger.Error("failed to inspect pending entries for delivery cap", "error", err)
		}
		return
	}

	ids := make([]string, 0, len(pending))
	counts := make(map[string]int64, len(pending))
	for _, p := range pending {
		if p.RetryCount > int64(max) {
			ids = append(ids, p.ID)
			counts[p.ID] = p.RetryCount
		}
	}
	if len(ids) == 0 {
		return
	}

	// Claim before moving: this takes ownership (so a peer's reaper cannot race
	// us to the same entry) and returns the original payload in one round trip.
	msgs, err := w.redis.XClaim(ctx, &redis.XClaimArgs{
		Stream:   protocol.StreamProvision,
		Group:    protocol.StreamGroup,
		Consumer: w.consumerName,
		MinIdle:  staleClaimAfter,
		Messages: ids,
	}).Result()
	if err != nil {
		logger.Error("failed to claim exhausted entries", "error", err)
		return
	}

	for _, msg := range msgs {
		w.dlqMessage(ctx, msg, counts[msg.ID])
	}
}

// dlqMessage copies msg to the DLQ and ACKs it. On a DLQ write failure the
// source entry is deliberately left pending (and unacked) so a later pass
// retries the move.
func (w *Worker) dlqMessage(ctx context.Context, msg redis.XMessage, deliveries int64) {
	values := map[string]interface{}{
		"original_id": msg.ID,
		"deliveries":  strconv.FormatInt(deliveries, 10),
		"reason":      "max deliveries exceeded",
	}
	if raw, ok := msg.Values["job"].(string); ok {
		values["job"] = raw
	} else if encoded, err := json.Marshal(msg.Values); err == nil {
		values["job"] = string(encoded)
	}

	if _, err := dlqAdd(ctx, w.redis, values); err != nil {
		logger.Error("failed to write DLQ entry, leaving pending", "message_id", msg.ID, "error", err)
		return
	}
	logger.Warn("moved poison entry to DLQ", "message_id", msg.ID, "deliveries", deliveries)
	w.ackMessage(ctx, msg.ID)
}

// trimStream bounds the provision stream with an approximate XTRIM so acked
// entries do not accumulate forever. The DLQ is deliberately never trimmed.
func (w *Worker) trimStream(ctx context.Context) {
	max := w.streamMaxLen()
	if max <= 0 {
		return
	}

	trimmed, err := w.redis.XTrimMaxLenApprox(ctx, protocol.StreamProvision, max, 0).Result()
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("failed to trim provision stream", "error", err)
		}
		return
	}
	if trimmed > 0 {
		logger.Debug("trimmed provision stream", "removed", trimmed, "max_len", max)
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

	// Process jobs (guest supervisor, one command, exit sentinel) are dispatched
	// before any function defaults are applied. Their outcome is recorded
	// asynchronously by startJob; only the spawn itself decides the ACK.
	if jobData.Mode == jobModeProcess {
		return w.startJob(ctx, jobData)
	}

	// Persistent executions (long-lived guest exec service over vsock) are
	// dispatched the same way: one microVM serving many execs. ACK-at-readiness,
	// never registered in w.instances.
	if jobData.Mode == protocol.ExecutionMode {
		return w.startExecution(ctx, jobData)
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

// In-flight job guard. The marker is set with SetNX before provisioning and
// left to expire on success, so a redelivered request within the TTL is a
// no-op. It is deleted only when provisioning fails before the VM is spawned,
// which is what allows the redelivery to retry.
const (
	jobInflightKeyPrefix = "job:req:"
	jobInflightTTL       = 120 * time.Second
)

func jobInflightKey(requestID string) string {
	return jobInflightKeyPrefix + requestID
}

// jobExitNonce chooses the exit-code nonce the guest will stamp on its final
// console line. The guest can only know the nonce if it is delivered via MMDS,
// so an offline job (no NIC, therefore no metadata service) must use the empty
// nonce: the supervisor then emits the bare AETHER_EXIT:<code> form and the
// scanner has to expect exactly that. Minting a nonce that is never delivered
// makes the sentinel unmatchable, and every offline job is classified as a
// crash even though it ran to completion.
func jobExitNonce(job protocol.Job, offline bool) string {
	if job.ExitNonce != "" {
		return job.ExitNonce
	}
	if offline {
		return ""
	}
	return id.GenerateToken()
}

// buildJobMMDSData assembles the MMDS payload for a process job. The command
// is passed through verbatim; timeout_s is always emitted (0 means "no guest
// deadline"), as is the exit nonce the guest stamps on its final line.
func buildJobMMDSData(bootToken string, job protocol.Job, dns []string, nonce string) map[string]interface{} {
	data := map[string]interface{}{
		"token":      bootToken,
		"mode":       jobModeProcess,
		"command":    job.Command,
		"timeout_s":  job.TimeoutSeconds,
		"exit_nonce": nonce,
		"env":        job.EnvVars,
	}
	if len(dns) > 0 {
		data["dns"] = dns
	}
	return data
}

// workerID returns the id recorded on job records. It falls back to the
// configured id for bare Worker literals built without NewWorker.
func (w *Worker) workerID() string {
	if w.consumerName != "" {
		return w.consumerName
	}
	if w.cfg != nil {
		return w.cfg.WorkerID
	}
	return ""
}

// startJob provisions and launches a single process-mode job.
//
// ACK invariant: it returns nil as soon as the VM has been *spawned*, so the
// stream entry is ACKed immediately. The job's outcome (done/failed/timeout) is
// classified and recorded asynchronously by JobRunner and is never an ACK
// condition. An error is returned only for a failure before the spawn, after
// clearing the in-flight marker, so a redelivery can retry.
//
// Job VMs are deliberately invisible to the scaler: they are neither appended
// to w.instances nor registered as function instances in etcd. The durable job
// record is the only registry entry and JobRunner owns cleanup.
func (w *Worker) startJob(ctx context.Context, job protocol.Job) error {
	jobID := job.JobID
	if jobID == "" {
		jobID = job.RequestID
	}
	// A stream written directly can bypass the API's ceiling; saturate rather
	// than letting the int->Duration conversion overflow into a negative value.
	job.TimeoutSeconds = clampTimeoutSeconds(job.TimeoutSeconds)
	log := logger.With("job_id", jobID, "request_id", job.RequestID)
	log.Info("received process job", "command", job.Command, "timeout_s", job.TimeoutSeconds)

	// Jobs can be written to the stream directly, bypassing the gateway, so the
	// id is validated here as well as at the API. It becomes an etcd key suffix
	// and, when a workspace is requested, a file name. Nothing has been
	// persisted yet at this point, so there is no marker to clear.
	if err := protocol.ValidJobID(jobID); err != nil {
		log.Error("rejecting job with invalid id", "error", err)
		return fmt.Errorf("invalid job id: %w", err)
	}

	// Durable guard: a record already running or done means another consumer
	// owns this job; skip (and ACK). A lookup error (missing record or etcd
	// unavailable) is not fatal — the in-flight marker is the live guard.
	if jobID != "" {
		if rec, err := w.registry.GetJob(jobID); err == nil {
			if rec.State == protocol.JobStateRunning || rec.State == protocol.JobStateDone {
				log.Info("job already running or done, skipping", "state", rec.State)
				return nil
			}
		}
	}

	// In-flight guard: a redelivery while the first delivery is still
	// provisioning must not launch a second VM.
	inflightKey := jobInflightKey(job.RequestID)
	if job.RequestID != "" {
		set, err := w.redis.SetNX(ctx, inflightKey, protocol.JobStateProvisioning, jobInflightTTL).Result()
		if err != nil {
			return fmt.Errorf("failed to set job in-flight marker: %w", err)
		}
		if !set {
			log.Info("job request already in flight, skipping")
			return nil
		}
	}

	// fail releases the in-flight marker so a redelivery can retry. It is used
	// for every failure before the VM is spawned.
	fail := func(err error) error {
		if job.RequestID != "" {
			dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if delErr := w.redis.Del(dctx, inflightKey).Err(); delErr != nil {
				log.Warn("failed to clear job in-flight marker", "error", delErr)
			}
		}
		return err
	}

	w.mu.Lock()
	runtimeCache := w.runtimeCache
	w.mu.Unlock()

	rootfs := w.cfg.RuntimePath
	if job.Runtime != "" && runtimeCache != nil {
		p, err := runtimeCache.Ensure(job.Runtime)
		if err != nil {
			return fail(fmt.Errorf("failed to resolve job runtime %q: %w", job.Runtime, err))
		}
		rootfs = p
	}

	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("job cancelled before provisioning: %w", err))
	}

	// Workspace: create the backing image before launch. A declared drive that
	// does not exist is a hard Firecracker error, so a creation failure is a
	// provisioning failure and must not ACK (the entry stays retryable).
	var workspacePath string
	if job.WorkspaceMB > 0 {
		wsPath, err := createJobWorkspace(w.cfg.WorkspaceDir, jobID, job.WorkspaceMB)
		if err != nil {
			return fail(fmt.Errorf("failed to create job workspace: %w", err))
		}
		workspacePath = wsPath
	}

	nonce := jobExitNonce(job, w.cfg.NoNetwork)

	// MMDS is delivered over the guest's NIC, so a network-less job has no
	// metadata service. The guest aether-env fails closed when it sees a boot
	// token but cannot fetch metadata, so offline jobs emit neither a token nor
	// an MMDS payload. They currently rely on the command baked into the rootfs
	// /init; config-drive bootstrap for network-less jobs is a later phase.
	var bootToken string
	var mmdsData map[string]interface{}
	if !w.cfg.NoNetwork {
		bootToken = id.GenerateToken()
		mmdsData = buildJobMMDSData(bootToken, job, w.cfg.GuestDNS, nonce)
	}

	vcpu := int64(job.VCPU)
	if vcpu == 0 {
		vcpu = 1
	}
	memMB := int64(job.MemoryMB)
	if memMB == 0 {
		memMB = 128
	}

	instance := NewInstance(job.FunctionID, w.vmMgr, w.bridgeMgr)
	console := newJobLog(defaultJobLogBytes, nonce)

	cfg := InstanceConfig{
		KernelPath:    w.cfg.KernelPath,
		RuntimePath:   rootfs,
		SocketPath:    filepath.Join(w.cfg.SocketDir, instance.ID+".sock"),
		VCPUCount:     vcpu,
		MemSizeMB:     memMB,
		BootToken:     bootToken,
		MMDSData:      mmdsData,
		NoNetwork:     w.cfg.NoNetwork,
		ConsoleWriter: console,
	}
	if workspacePath != "" {
		// The root filesystem is /dev/vda; configured drives follow in slice
		// order, so the workspace is /dev/vdb. It is deliberately writable: the
		// whole point is to leave results behind. RootFSReadOnly stays as-is for
		// now (the job rootfs is still read-write); making it read-only is a
		// follow-up that depends on the workspace being universally available.
		cfg.Drives = []vm.DriveSpec{{Path: workspacePath, ReadOnly: false}}
	}

	if err := provisionStart(ctx, instance, cfg); err != nil {
		// Stop is safe after a partial start; no proxy port was allocated.
		_ = instance.Stop()
		return fail(fmt.Errorf("failed to start job instance: %w", err))
	}

	started := time.Now().UTC()
	running := protocol.JobRecord{
		JobID:         jobID,
		RequestID:     job.RequestID,
		Mode:          jobModeProcess,
		State:         protocol.JobStateRunning,
		WorkerID:      w.workerID(),
		StartedAt:     started,
		HeartbeatAt:   started,
		WorkspacePath: workspacePath,
	}
	if err := w.registry.PutJob(running); err != nil {
		// Recording is best-effort: the VM is up and JobRunner will write the
		// terminal record. Never fail the ACK because of a registry hiccup.
		log.Error("failed to record running job", "error", err)
	}

	runner := NewJobRunner(JobRunnerConfig{
		JobID:             jobID,
		RequestID:         job.RequestID,
		WorkerID:          w.workerID(),
		Nonce:             nonce,
		Timeout:           time.Duration(job.TimeoutSeconds) * time.Second,
		WorkspacePath:     workspacePath,
		Log:               console,
		Wait:              func() error { return <-instance.ExitCh() },
		Stop:              instance.Stop,
		Record:            func(rec protocol.JobRecord) error { return w.registry.PutJob(rec) },
		HeartbeatInterval: jobHeartbeatInterval,
		Heartbeat:         func(rec protocol.JobRecord) error { return w.registry.PutJob(rec) },
	})

	// Register immediately before launch so a cancel request that arrives while
	// the runner is starting still finds it. The runner deregisters through the
	// cleanup hook when Run returns (including on panic).
	runner.cleanup = func() { w.deregisterJob(jobID, runner) }
	w.registerJob(jobID, runner)

	startJobRunner(ctx, runner)
	log.Info("process job launched", "instance_id", instance.ID, "runtime", job.Runtime)
	return nil
}

// registerJob records a running job's runner so CancelJob can reach it. Jobs
// are deliberately kept out of w.instances: they are invisible to the scaler.
func (w *Worker) registerJob(jobID string, runner *JobRunner) {
	w.mu.Lock()
	if w.jobs == nil {
		w.jobs = make(map[string]*JobRunner)
	}
	w.jobs[jobID] = runner
	w.mu.Unlock()
}

// deregisterJob removes a finished job's runner. The runner pointer is checked
// so a late deregistration cannot delete a newer runner for the same id.
func (w *Worker) deregisterJob(jobID string, runner *JobRunner) {
	w.mu.Lock()
	if w.jobs != nil && w.jobs[jobID] == runner {
		delete(w.jobs, jobID)
	}
	w.mu.Unlock()
}

// CancelJob asks the runner owning jobID to stop. It reports whether a runner
// was found locally. Cancellation is best-effort: a job that already finished,
// or one owned by another worker, is simply not found.
func (w *Worker) CancelJob(jobID string) bool {
	w.mu.Lock()
	runner := w.jobs[jobID]
	w.mu.Unlock()
	if runner == nil {
		return false
	}
	runner.Cancel()
	return true
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
	// startJobRunner launches the asynchronous outcome recorder for a process
	// job. The goroutine is wrapped so the worker's job registry is always
	// cleaned up when Run returns, including if it panics. Tests override it to
	// observe the runner without waiting on a VM.
	startJobRunner = func(ctx context.Context, runner *JobRunner) {
		go func() {
			defer runner.runCleanup()
			runner.Run(ctx)
		}()
	}
	// createJobWorkspace builds a job's workspace image. Tests override it so
	// they exercise attachment without invoking mke2fs.
	createJobWorkspace = CreateWorkspace
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
		// NoNetwork is threaded through for completeness, but HTTP functions
		// require a networked mode: without a NIC there is no guest IP to poll
		// for readiness and no proxy target, so NET_MODE=none is for offline
		// process jobs only.
		NoNetwork: w.cfg.NoNetwork,
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

	w.destroyAllExecutions()

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

// WatchJobCancels subscribes to the cancel channel and forwards each job id to
// CancelJob. Cancellation is best-effort pub/sub: a publish that nobody
// receives (worker restarting, or the job owned by another worker) is not
// retried, and a cancel arriving after the job has finished is a no-op because
// the runner is no longer registered.
func (w *Worker) WatchJobCancels(ctx context.Context) {
	pubsub := w.redis.Subscribe(ctx, protocol.ChannelJobCancel)
	defer pubsub.Close()

	logger.Info("watching for job cancellations", "channel", protocol.ChannelJobCancel)

	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-pubsub.Channel():
			jobID := msg.Payload
			if w.CancelJob(jobID) {
				logger.Info("job cancellation requested", "job_id", jobID)
			} else {
				logger.Info("job cancellation for unknown local job", "job_id", jobID)
			}
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
