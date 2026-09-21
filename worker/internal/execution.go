package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"aether/shared/logger"
	"aether/shared/protocol"
	"aether/shared/vm"
)

// executionReadyTimeout bounds how long the worker waits for the guest exec
// service handshake after the VM process has launched. There is no HTTP probe:
// a successful CONNECT + Hello/Ready over vsock is the readiness gate.
const executionReadyTimeout = 60 * time.Second

// defaultControlPort is the worker control API port when WORKER_CONTROL_PORT is
// unset. The gateway dials the owning worker's WorkerAddr (worker IP + this
// port) to exec and destroy executions.
const defaultControlPort = 9091

// Errors returned by the execution exec path so the control API can map them to
// distinct status codes (404 unknown, 409 busy, 502 guest failure).
var (
	errExecutionNotFound = errors.New("execution not found")
	errExecutionBusy     = errors.New("execution busy")
)

// vsockCIDCounter hands out guest context ids for execution VMs. It starts at 2
// so the first allocated CID is 3 (2 is reserved for the host; Firecracker
// requires >= 3). Each execution's vsock device has its own Unix socket, so
// CIDs are effectively namespaced per VM; a monotonic counter still guarantees
// no two concurrently running VMs on this host share a CID, which keeps the
// scheme simple and unambiguous if a future host-side vsock path ever addresses
// guests by CID. Wraparound after 2^32 VMs is irrelevant in practice.
var vsockCIDCounter uint32 = 2

func nextVsockCID() uint32 { return atomic.AddUint32(&vsockCIDCounter, 1) }

// Execution is a thin persistent-execution handle: one microVM, its workspace
// image and the vsock path the host uses to drive the guest exec service. It
// deliberately introduces no new abstraction layer — no Machine/MachineSpec,
// Controller, Session or Scheduler.
//
// An Execution lives only on the worker that created it and is deliberately not
// added to Worker.instances (the scaler view), exactly like process jobs. The
// durable ExecutionRecord is its only registry entry.
type Execution struct {
	ID            string
	instance      *Instance
	workspacePath string
	vsockPath     string
	workerAddr    string
	startedAt     time.Time

	mu       sync.Mutex
	busy     bool
	stopped  bool
	lifetime *time.Timer
}

// tryAcquire is the per-execution busy gate. It admits at most one exec at a
// time and refuses once the execution is stopping/stopped.
func (e *Execution) tryAcquire() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped || e.busy {
		return false
	}
	e.busy = true
	return true
}

func (e *Execution) release() {
	e.mu.Lock()
	e.busy = false
	e.mu.Unlock()
}

// markStopped closes the gate and stops the lifetime timer. It is idempotent.
func (e *Execution) markStopped() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	if e.lifetime != nil {
		e.lifetime.Stop()
		e.lifetime = nil
	}
}

// --- execution record persistence (etcd) ------------------------------------

// PutExecution stores an execution record durably. Like job records (and unlike
// worker/instance registrations) it is not attached to a lease: the record must
// outlive the worker so the API can still answer for stopped executions.
func (r *Registry) PutExecution(rec protocol.ExecutionRecord) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal execution record: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := r.client.Put(ctx, protocol.ExecutionKey(rec.ID), string(val)); err != nil {
		return fmt.Errorf("failed to put execution record: %w", err)
	}
	logger.Info("execution record stored", "execution_id", rec.ID, "state", rec.State)
	return nil
}

// GetExecution reads an execution record by id. A missing record is an error.
func (r *Registry) GetExecution(id string) (protocol.ExecutionRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := r.client.Get(ctx, protocol.ExecutionKey(id))
	if err != nil {
		return protocol.ExecutionRecord{}, fmt.Errorf("failed to get execution record: %w", err)
	}
	if len(resp.Kvs) == 0 {
		return protocol.ExecutionRecord{}, fmt.Errorf("execution %s not found", id)
	}

	var rec protocol.ExecutionRecord
	if err := json.Unmarshal(resp.Kvs[0].Value, &rec); err != nil {
		return protocol.ExecutionRecord{}, fmt.Errorf("failed to unmarshal execution record: %w", err)
	}
	return rec, nil
}

// Seams so unit tests can exercise the lifecycle without etcd.
var (
	putExecution = func(r *Registry, rec protocol.ExecutionRecord) error { return r.PutExecution(rec) }
	getExecution = func(r *Registry, id string) (protocol.ExecutionRecord, error) { return r.GetExecution(id) }
	// execOnGuest runs one command against the guest service. Tests override it
	// to avoid a real VM.
	execOnGuest = runExecOnGuest
	// executionWaitReady is the readiness gate (vsock handshake). Tests override
	// it so provisioning can complete without a real guest.
	executionWaitReady = waitExecHandshake
	// shutdownGuestFn asks the guest to reset before teardown. Best-effort.
	shutdownGuestFn = shutdownExecGuest
)

func (w *Worker) putExecutionRecord(rec protocol.ExecutionRecord) error {
	if w == nil || w.registry == nil {
		return errors.New("no registry configured")
	}
	return putExecution(w.registry, rec)
}

func (w *Worker) getExecutionRecord(id string) (protocol.ExecutionRecord, error) {
	if w == nil || w.registry == nil {
		return protocol.ExecutionRecord{}, errors.New("no registry configured")
	}
	return getExecution(w.registry, id)
}

// --- worker execution registry ----------------------------------------------

func (w *Worker) registerExecution(e *Execution) {
	w.mu.Lock()
	if w.executions == nil {
		w.executions = make(map[string]*Execution)
	}
	w.executions[e.ID] = e
	w.mu.Unlock()
}

func (w *Worker) lookupExecution(id string) *Execution {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.executions[id]
}

// removeExecution atomically removes and returns an execution, so exactly one
// caller (a DELETE, a lifetime expiry or shutdown) owns its teardown. A second
// concurrent destroy gets nil.
func (w *Worker) removeExecution(id string) *Execution {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.executions[id]
	if !ok {
		return nil
	}
	delete(w.executions, id)
	return e
}

// controlAddr is the address recorded on an execution so the gateway can dial
// the owning worker's control API.
func (w *Worker) controlAddr() string {
	host := "127.0.0.1"
	port := defaultControlPort
	if w != nil && w.cfg != nil {
		if w.cfg.WorkerIP != "" {
			host = w.cfg.WorkerIP
		}
		if w.cfg.ControlPort > 0 {
			port = w.cfg.ControlPort
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// startExecution provisions one persistent execution and returns as soon as the
// guest service answers the handshake, so the provision-stream entry is ACKed
// at readiness. A failure before that point returns an error (no ACK) after
// clearing the in-flight marker, so a redelivery can retry.
func (w *Worker) startExecution(ctx context.Context, job protocol.Job) error {
	execID := job.JobID
	if execID == "" {
		execID = job.RequestID
	}
	log := logger.With("execution_id", execID, "request_id", job.RequestID)
	log.Info("received execution job", "runtime", job.Runtime, "lifetime_s", job.TimeoutSeconds)

	// Executions can be written to the stream directly, bypassing the gateway,
	// so the id is validated here as well as at the API. It becomes an etcd key
	// suffix and a workspace file name.
	if err := protocol.ValidJobID(execID); err != nil {
		log.Error("rejecting execution with invalid id", "error", err)
		return fmt.Errorf("invalid execution id: %w", err)
	}

	// Durable guard: a record already creating/ready means another consumer owns
	// this execution; skip (and ACK). A lookup error is not fatal — the
	// in-flight marker is the live guard.
	if rec, err := w.getExecutionRecord(execID); err == nil {
		if rec.State == protocol.ExecutionStateReady || rec.State == protocol.ExecutionStateCreating {
			log.Info("execution already in progress, skipping", "state", rec.State)
			return nil
		}
	}

	// In-flight guard: a redelivery while the first delivery is still
	// provisioning must not launch a second VM. Reuses the process-job marker.
	inflightKey := jobInflightKey(job.RequestID)
	if job.RequestID != "" {
		set, err := w.redis.SetNX(ctx, inflightKey, protocol.ExecutionStateCreating, jobInflightTTL).Result()
		if err != nil {
			return fmt.Errorf("failed to set execution in-flight marker: %w", err)
		}
		if !set {
			log.Info("execution request already in flight, skipping")
			return nil
		}
	}

	fail := func(err error) error {
		if job.RequestID != "" {
			dctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if delErr := w.redis.Del(dctx, inflightKey).Err(); delErr != nil {
				log.Warn("failed to clear execution in-flight marker", "error", delErr)
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
			return fail(fmt.Errorf("failed to resolve execution runtime %q: %w", job.Runtime, err))
		}
		rootfs = p
	}

	if err := ctx.Err(); err != nil {
		return fail(fmt.Errorf("execution cancelled before provisioning: %w", err))
	}

	// Workspace: create the backing image before launch. A declared drive that
	// does not exist is a hard Firecracker error, so a creation failure is a
	// provisioning failure and must not ACK.
	var workspacePath string
	if job.WorkspaceMB > 0 {
		wsPath, err := createJobWorkspace(w.cfg.WorkspaceDir, execID, job.WorkspaceMB)
		if err != nil {
			return fail(fmt.Errorf("failed to create execution workspace: %w", err))
		}
		workspacePath = wsPath
	}

	vsockPath := filepath.Join(w.cfg.SocketDir, execID+".vsock")
	// Firecracker refuses to reuse an existing bridge socket.
	if err := os.Remove(vsockPath); err != nil && !os.IsNotExist(err) {
		log.Warn("failed to remove stale vsock socket", "path", vsockPath, "error", err)
	}
	cid := nextVsockCID()

	vcpu := int64(job.VCPU)
	if vcpu == 0 {
		vcpu = 1
	}
	memMB := int64(job.MemoryMB)
	if memMB == 0 {
		memMB = 128
	}

	instance := NewInstance(job.FunctionID, w.vmMgr, w.bridgeMgr)
	// Bounded, non-blocking console sink: execution boot output is small, and a
	// blocking sink would stall VM shutdown (see jobLog).
	console := newJobLog(defaultJobLogBytes, "")

	cfg := InstanceConfig{
		KernelPath:    w.cfg.KernelPath,
		RuntimePath:   rootfs,
		SocketPath:    filepath.Join(w.cfg.SocketDir, instance.ID+".sock"),
		VCPUCount:     vcpu,
		MemSizeMB:     memMB,
		ConsoleWriter: console,
		// Executions are vsock-only; the guest has no NIC requirement. NoNetwork
		// is honoured so an offline worker can serve them without privileges.
		NoNetwork: w.cfg.NoNetwork,
		Vsock:     &vm.VsockSpec{Path: vsockPath, CID: cid},
	}
	if workspacePath != "" {
		cfg.Drives = []vm.DriveSpec{{Path: workspacePath, ReadOnly: false}}
	}

	if err := provisionStart(ctx, instance, cfg); err != nil {
		_ = instance.Stop()
		removeExecutionWorkspace(workspacePath)
		return fail(fmt.Errorf("failed to start execution instance: %w", err))
	}

	// Readiness gate: wait for the guest exec service handshake.
	if err := executionWaitReady(ctx, vsockPath, executionReadyTimeout); err != nil {
		_ = instance.Stop()
		removeExecutionWorkspace(workspacePath)
		return fail(fmt.Errorf("execution not ready: %w", err))
	}

	started := time.Now().UTC()
	exec := &Execution{
		ID:            execID,
		instance:      instance,
		workspacePath: workspacePath,
		vsockPath:     vsockPath,
		workerAddr:    w.controlAddr(),
		startedAt:     started,
	}
	w.registerExecution(exec)

	rec := protocol.ExecutionRecord{
		ID:            execID,
		State:         protocol.ExecutionStateReady,
		WorkerID:      w.workerID(),
		WorkerAddr:    w.controlAddr(),
		WorkspacePath: workspacePath,
		StartedAt:     started,
	}
	if err := w.putExecutionRecord(rec); err != nil {
		// Recording is best-effort: the VM is up and serving; a registry hiccup
		// must not fail the ACK (the execution simply cannot be polled ready).
		log.Error("failed to record ready execution", "error", err)
	}

	// Optional lifetime: destroy the execution when it expires. The timer is
	// owned by the Worker, not by the provisioning ctx, so the execution is not
	// tied to the request that created it.
	if job.TimeoutSeconds > 0 {
		exec.mu.Lock()
		exec.lifetime = time.AfterFunc(time.Duration(job.TimeoutSeconds)*time.Second, func() {
			log.Info("execution lifetime expired, destroying")
			if err := w.destroyExecution(execID); err != nil {
				log.Warn("failed to destroy expired execution", "error", err)
			}
		})
		exec.mu.Unlock()
	}

	log.Info("execution ready", "instance_id", instance.ID, "cid", cid, "worker_addr", exec.workerAddr)
	return nil
}

// ExecExecution runs one command on a live execution. A missing execution
// yields errExecutionNotFound; a concurrent exec yields errExecutionBusy; a
// guest-side busy reply is surfaced as errGuestBusy. These are distinguishable
// so the control API can answer 404/409/502.
func (w *Worker) ExecExecution(ctx context.Context, id string, req protocol.ExecRequest) (protocol.ExecResult, error) {
	exec := w.lookupExecution(id)
	if exec == nil {
		return protocol.ExecResult{}, errExecutionNotFound
	}
	if !exec.tryAcquire() {
		return protocol.ExecResult{}, errExecutionBusy
	}
	defer exec.release()

	log := logger.With("execution_id", id)
	log.Debug("running exec", "argv", req.Argv)

	res, err := execOnGuest(ctx, exec.vsockPath, id, req)
	if err != nil {
		return res, err
	}
	return res, nil
}

// destroyExecution tears an execution down. It is idempotent: the first caller
// claims the execution (shutdown, Stop, record "stopped"), later callers are
// no-ops. The workspace image is deliberately retained per the existing
// retention policy — the caller retrieves results from it.
func (w *Worker) destroyExecution(id string) error {
	exec := w.removeExecution(id)
	if exec == nil {
		return nil
	}
	log := logger.With("execution_id", id)
	log.Info("destroying execution")

	// Close the gate first so a racing exec is refused, then best-effort ask
	// the guest to flush/reset before the VMM is stopped.
	exec.markStopped()
	if err := shutdownGuestFn(exec.vsockPath); err != nil {
		log.Debug("guest shutdown handshake failed", "error", err)
	}
	if err := exec.instance.Stop(); err != nil {
		log.Warn("failed to stop execution instance", "error", err)
	}

	rec := protocol.ExecutionRecord{
		ID:            id,
		State:         protocol.ExecutionStateStopped,
		WorkerID:      w.workerID(),
		WorkerAddr:    exec.workerAddr,
		WorkspacePath: exec.workspacePath,
		StartedAt:     exec.startedAt,
		FinishedAt:    time.Now().UTC(),
	}
	if err := w.putExecutionRecord(rec); err != nil {
		log.Error("failed to record stopped execution", "error", err)
	}
	log.Info("execution stopped", "workspace", exec.workspacePath)
	return nil
}

// DestroyExecution is the control API entry point for DELETE. It is idempotent
// and always succeeds for a well-formed id.
func (w *Worker) DestroyExecution(id string) error {
	return w.destroyExecution(id)
}

// destroyAllExecutions stops every live execution; called on worker shutdown.
func (w *Worker) destroyAllExecutions() {
	w.mu.Lock()
	ids := make([]string, 0, len(w.executions))
	for id := range w.executions {
		ids = append(ids, id)
	}
	w.mu.Unlock()

	for _, id := range ids {
		if err := w.destroyExecution(id); err != nil {
			logger.Warn("failed to destroy execution on shutdown", "execution_id", id, "error", err)
		}
	}
}

// removeExecutionWorkspace removes a workspace image created for a provisioning
// attempt that failed before the execution was registered and ACKed. Without
// this, the redelivery would hit CreateWorkspace's "refusing to reuse it" guard
// and wedge. A no-op when no workspace was created.
func removeExecutionWorkspace(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Warn("failed to remove failed execution workspace", "path", path, "error", err)
	}
}
