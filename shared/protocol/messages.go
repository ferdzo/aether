package protocol

import "time"

// Job represents a request to PROVISION a VM (not execute code)
// Pushed to Redis: "queue:vm_provision"
type Job struct {
    RequestID  string `json:"request_id"`
    FunctionID string `json:"function_id"`
    ImageID    string `json:"image_id"` // Useful to know which rootfs to load
    VCPU       int    `json:"vcpu"`
    MemoryMB   int    `json:"memory_mb"`
}

// WorkerNode represents the heavy physical server (The Host)
// Stored in etcd: "/workers/<worker_id>"
type WorkerNode struct {
    ID            string    `json:"id"`
    Hostname      string    `json:"hostname"`
    PublicIP      string    `json:"public_ip"` // How the Gateway reaches this node
    TotalCPU      int       `json:"total_cpu"` // Capacity
    TotalMemoryMB int       `json:"total_mem"` // Capacity
    Version       string    `json:"version"`   // Agent version (good for upgrades)
    LastHeartbeat time.Time `json:"last_heartbeat"`
}

// FunctionInstance represents a single running Firecracker microVM
// Stored in etcd: "/functions/<function_id>/instances/<worker_id>"
type FunctionInstance struct {
    FunctionID string    `json:"function_id"`
    WorkerID   string    `json:"worker_id"` // Which node is holding me?
    
    // Networking
    HostIP    string `json:"host_ip"`    // The Worker's public IP
    ProxyPort int    `json:"proxy_port"` // The port exposed on the host (e.g., 30005)
    InternalIP string `json:"internal_ip"` // The TAP IP (e.g., 172.16.0.2)
    
    // Metadata
    Status    string    `json:"status"` // "starting", "ready", "error"
    StartedAt time.Time `json:"started_at"`
}

// --- Keys & Constants ---

const (
    QueueVMProvision = "queue:vm_provision"
    EtcdFuncPrefix   = "/functions/"
    EtcdWorkerPrefix = "/workers/"
)

// InstanceKey returns: /functions/resize-image/instances/worker-1-vm-5
// Note: We need a unique ID for the VM, not just the WorkerID, 
// in case one worker runs multiple instances of the same function (scaling).
func InstanceKey(functionID, workerID, vmID string) string {
    return EtcdFuncPrefix + functionID + "/instances/" + workerID + "-" + vmID
}

// WorkerKey returns: /workers/worker-1
func WorkerKey(workerID string) string {
    return EtcdWorkerPrefix + workerID
}