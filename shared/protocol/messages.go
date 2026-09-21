package protocol

import (
	"errors"
	"fmt"
	"time"
)

// maxJobIDLen bounds a job id. It keeps etcd keys and workspace file names
// reasonable; nothing depends on the exact number.
const maxJobIDLen = 128

// ValidJobID reports whether id can be used as an etcd key suffix, a URL path
// segment and a workspace file name.
//
// The gateway rejects invalid caller-supplied ids so a job cannot be created
// that is impossible to look up: a '/' in the id would be treated as a path
// separator by the router, making GET /api/jobs/{id} unable to reach it. The
// worker re-checks because jobs can also be written to the stream directly,
// bypassing the API.
func ValidJobID(id string) error {
	if id == "" {
		return errors.New("job id is empty")
	}
	if len(id) > maxJobIDLen {
		return fmt.Errorf("job id is longer than %d characters", maxJobIDLen)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("job id %q is not allowed", id)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("job id %q contains %q; allowed: letters, digits, '-', '_' and '.'", id, r)
		}
	}
	return nil
}

type Job struct {
	RequestID    string            `json:"request_id"`
	FunctionID   string            `json:"function_id"`
	ImageID      string            `json:"image_id"`
	Runtime      string            `json:"runtime"`
	Entrypoint   string            `json:"entrypoint"`
	VCPU         int               `json:"vcpu"`
	MemoryMB     int               `json:"memory_mb"`
	Port         int               `json:"port"`
	Count        int               `json:"count"`
	EnvVars      map[string]string `json:"env_vars,omitempty"`
	TraceContext map[string]string `json:"trace_context,omitempty"`

	// Process-mode job fields. All optional and additive: existing function
	// provision jobs leave them zero and behave exactly as before.
	JobID          string   `json:"job_id,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	Command        []string `json:"command,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	ExitNonce      string   `json:"exit_nonce,omitempty"`
}

// JobRecord is the durable etcd record of a single process job. It outlives the
// worker that ran it, so it is never attached to a lease.
type JobRecord struct {
	JobID      string    `json:"job_id"`
	RequestID  string    `json:"request_id,omitempty"`
	Mode       string    `json:"mode,omitempty"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exit_code"`
	WorkerID   string    `json:"worker_id,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Job lifecycle states. A job starts out provisioning/running and ends in
// exactly one of done, failed or timeout.
const (
	JobStateProvisioning = "provisioning"
	JobStateRunning      = "running"
	JobStateDone         = "done"
	JobStateFailed       = "failed"
	JobStateTimeout      = "timeout"
)

type WorkerNode struct {
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	PublicIP      string    `json:"public_ip"`
	TotalCPU      int       `json:"total_cpu"`
	TotalMemoryMB int       `json:"total_mem"`
	Version       string    `json:"version"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

type FunctionInstance struct {
	InstanceID string    `json:"instance_id"`
	FunctionID string    `json:"function_id"`
	WorkerID   string    `json:"worker_id"`
	HostIP     string    `json:"host_ip"`
	ProxyPort  int       `json:"proxy_port"`
	InternalIP string    `json:"internal_ip"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
}

const (
	StreamProvision   = "stream:vm_provision"
	StreamGroup       = "aether-workers"
	ChannelCodeUpdate = "channel:code_update"
	EtcdFuncPrefix    = "/functions/"
	EtcdWorkerPrefix  = "/workers/"
	EtcdJobPrefix     = "/jobs/"
)

func InstanceKey(functionID, instanceID string) string {
	return EtcdFuncPrefix + functionID + "/instances/" + instanceID
}

// JobKey returns the etcd key holding the record for jobID.
func JobKey(jobID string) string {
	return EtcdJobPrefix + jobID
}

func WorkerKey(workerID string) string {
	return EtcdWorkerPrefix + workerID
}

type FunctionMetadata struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Runtime    string            `json:"runtime"`
	Entrypoint string            `json:"entrypoint"` // e.g., "handler.js", "main.py", "app.go"
	CodePath   string            `json:"code_path"`
	VCPU       int               `json:"vcpu"`
	MemoryMB   int               `json:"memory_mb"`
	Port       int               `json:"port"`
	EnvVars    map[string]string `json:"env_vars"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

type Invocation struct {
	ID           string    `json:"id"`
	FunctionID   string    `json:"function_id"`
	Status       string    `json:"status"`
	DurationMS   int       `json:"duration_ms"`
	StartedAt    time.Time `json:"started_at"`
	ErrorMessage string    `json:"error_message,omitempty"`
}
