package protocol

import "time"

// ExecutionMode is the protocol.Job.Mode value that asks the worker for a
// persistent execution instead of a one-shot process job. The job is published
// on the existing provision stream (StreamProvision) with this mode, so
// execution creation reuses the durable stream, ACK-at-spawn, idempotency and
// DLQ for free; there is deliberately no separate scheduler.
//
// A process job runs one command and resets its VM. An execution keeps one
// microVM alive and serves many execs over virtio-vsock (see
// shared/protocol/exec.go), with the workspace persisting between them for the
// execution's lifetime. Job.TimeoutSeconds is reused as that lifetime.
const ExecutionMode = "execution"

// EtcdExecutionPrefix is the key space holding durable execution records. Like
// job records and unlike worker/instance registrations, they are written
// without a lease so they outlive the worker and the API can still answer for
// stopped executions.
const EtcdExecutionPrefix = "/executions/"

// ExecutionRecord is the durable etcd record of a single persistent execution.
// It is the only registry entry for an execution: executions are deliberately
// invisible to the scaler (never added to Worker.instances), exactly like
// process jobs.
type ExecutionRecord struct {
	ID string `json:"id"`
	// RequestID identifies the creation attempt that wrote this record.
	// Re-creating an id overwrites the previous run's terminal record, and a
	// client waiting for readiness can observe that stale record before the
	// worker has written the new one, so the poll matches on this field.
	RequestID  string `json:"request_id,omitempty"`
	State      string `json:"state"`
	WorkerID   string `json:"worker_id,omitempty"`
	WorkerAddr string `json:"worker_addr,omitempty"`
	// WorkspacePath is the host path to the execution's workspace image. The
	// guest sees it mounted at /workspace; the image outlives the VM and is
	// subject to the existing worker-side retention/GC policy.
	WorkspacePath string    `json:"workspace_path,omitempty"`
	Error         string    `json:"error,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
}

// Execution lifecycle states. An execution is created ("creating" while the VM
// boots), becomes "ready" once the guest service answers the vsock handshake,
// moves through "stopping" and ends in exactly one of "stopped" (destroyed by
// request or lifetime expiry) or "failed".
const (
	ExecutionStateCreating = "creating"
	ExecutionStateReady    = "ready"
	ExecutionStateStopping = "stopping"
	ExecutionStateStopped  = "stopped"
	ExecutionStateFailed   = "failed"
)

// ExecutionKey returns the etcd key holding the record for execution id. The id
// is validated with ValidJobID (the same rules as a job id: no '/', bounded
// length, a safe path element) so a record can always be looked up again.
func ExecutionKey(id string) string {
	return EtcdExecutionPrefix + id
}
