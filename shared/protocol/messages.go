package protocol

import "time"

// Pushed to Redis: "queue:vm_provision"
type Job struct {
    RequestID  string `json:"request_id"`
    FunctionID string `json:"function_id"`
    ImageID    string `json:"image_id"`
    VCPU       int    `json:"vcpu"`
    MemoryMB   int    `json:"memory_mb"`
}

// Stored in etcd: "/workers/<worker_id>"
type WorkerNode struct {
    ID            string    `json:"id"`
    Hostname      string    `json:"hostname"`
    PublicIP      string    `json:"public_ip"`
    TotalCPU      int       `json:"total_cpu"`
    TotalMemoryMB int       `json:"total_mem"`
    Version       string    `json:"version"`
    LastHeartbeat time.Time `json:"last_heartbeat"`
}

// FunctionInstance represents a single running Firecracker microVM
// Stored in etcd: "/functions/<function_id>/instances/<worker_id>-<instance_id>"
type FunctionInstance struct {
    InstanceID string `json:"instance_id"`
    FunctionID string `json:"function_id"`
    WorkerID   string `json:"worker_id"`

    HostIP     string `json:"host_ip"`
    ProxyPort  int    `json:"proxy_port"`
    InternalIP string `json:"internal_ip"`

    Status    string    `json:"status"`
    StartedAt time.Time `json:"started_at"`
}

// --- Keys & Constants ---

const (
    QueueVMProvision = "queue:vm_provision"
    EtcdFuncPrefix   = "/functions/"
    EtcdWorkerPrefix = "/workers/"
)

// InstanceKey returns: /functions/resize-image/instances/inst-xxx
func InstanceKey(functionID, instanceID string) string {
    return EtcdFuncPrefix + functionID + "/instances/" + instanceID
}

// WorkerKey returns: /workers/worker-1
func WorkerKey(workerID string) string {
    return EtcdWorkerPrefix + workerID
}