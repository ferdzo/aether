package protocol

import "time"

// Job represents a function execution request
type Job struct {
	RequestID  string `json:"request_id"`
	FunctionID string `json:"function_id"`
	VCPU       int    `json:"vcpu"`
	MemoryMB   int    `json:"memory_mb"`
}

// WorkerInfo represents a registered worker in etcd
type WorkerInfo struct {
	WorkerID   string `json:"worker_id"`
	WorkerIP   string `json:"worker_ip"`
	ProxyPort  int    `json:"proxy_port"`
	InternalIP string `json:"internal_vm_ip"`
}

// FunctionInstance represents a running function
type FunctionInstance struct {
	FunctionID string    `json:"function_id"`
	WorkerID   string    `json:"worker_id"`
	VMIP       string    `json:"vm_ip"`
	Port       int       `json:"port"`
	StartedAt  time.Time `json:"started_at"`
}

// Redis keys
const (
	QueueVMProvision = "queue:vm_provision"
)

// etcd key patterns
const (
	EtcdFunctionPrefix = "/functions/"
	EtcdWorkerPrefix   = "/workers/"
)

func FunctionKey(functionID, workerID string) string {
	return EtcdFunctionPrefix + functionID + "/instances/" + workerID
}

func WorkerKey(workerID string) string {
	return EtcdWorkerPrefix + workerID
}

