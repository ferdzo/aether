package protocol

import "time"

type Job struct {
	RequestID    string            `json:"request_id"`
	FunctionID   string            `json:"function_id"`
	ImageID      string            `json:"image_id"`
	VCPU         int               `json:"vcpu"`
	MemoryMB     int               `json:"memory_mb"`
	Port         int               `json:"port"`
	Count        int               `json:"count"`
	EnvVars      map[string]string `json:"env_vars,omitempty"`
	TraceContext map[string]string `json:"trace_context,omitempty"` // For distributed tracing
}

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
	InstanceID string `json:"instance_id"`
	FunctionID string `json:"function_id"`
	WorkerID   string `json:"worker_id"`
	HostIP     string `json:"host_ip"`
	ProxyPort  int    `json:"proxy_port"`
	InternalIP string `json:"internal_ip"`
	Status     string `json:"status"`
	StartedAt  time.Time `json:"started_at"`
}

const (
	QueueVMProvision  = "queue:vm_provision"
	ChannelCodeUpdate = "channel:code_update"
	EtcdFuncPrefix    = "/functions/"
	EtcdWorkerPrefix  = "/workers/"
)

func InstanceKey(functionID, instanceID string) string {
	return EtcdFuncPrefix + functionID + "/instances/" + instanceID
}

func WorkerKey(workerID string) string {
	return EtcdWorkerPrefix + workerID
}

type FunctionMetadata struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Runtime   string            `json:"runtime"`
	CodePath  string            `json:"code_path"`
	VCPU      int               `json:"vcpu"`
	MemoryMB  int               `json:"memory_mb"`
	Port      int               `json:"port"`
	EnvVars   map[string]string `json:"env_vars"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type Invocation struct {
	ID           string    `json:"id"`
	FunctionID   string    `json:"function_id"`
	Status       string    `json:"status"`
	DurationMS   int       `json:"duration_ms"`
	StartedAt    time.Time `json:"started_at"`
	ErrorMessage string    `json:"error_message,omitempty"`
}