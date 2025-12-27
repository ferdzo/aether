# Project: Go-Firecracker FaaS Platform
## 1. Project Overview
We are building a custom Function-as-a-Service (FaaS) platform from scratch to learn distributed systems and virtualization.
**Goal:** Create a system where a user can request a function execution, and the system spins up a Firecracker microVM, executes the code, and returns the result.

## 2. Core Architecture
The system follows a "Control Plane / Data Plane" separation.

### 2.1 The Tech Stack
* **Language:** Go (Golang) 1.21+
* **Virtualization:** Firecracker MicroVM (via `firecracker-go-sdk`)
* **Queue/Signaling:** Redis (List primitives)
* **State & Discovery:** etcd (Leases & Watchers)
* **OS:** Linux (Requires KVM access)

### 2.2 Component Roles
1.  **API Gateway (The Controller)**
    * Ingests HTTP requests (`GET /run/:func_id`).
    * Checks **etcd** for active workers.
    * If no worker exists: Pushes job to **Redis**, watches **etcd**, waits.
    * If worker exists: Proxies HTTP request directly to Worker IP:Port.

2.  **Worker Agent (The Data Plane)**
    * Runs on the compute node.
    * Loops on `BRPOP` from **Redis** to pick up jobs.
    * Launches Firecracker VMs using the Go SDK.
    * Configures Host-Local Networking (TAP devices).
    * Registers specific VM IP:Port in **etcd** with a Lease (TTL).
    * Proxies traffic from Host Port -> VM Internal IP.

## 3. Data Contracts & Schema

### 3.1 Redis (Work Queue)
* **Key:** `queue:vm_provision`
* **Type:** List (LPUSH / BRPOP)
* **Payload:**
    ```json
    {
      "request_id": "req-uuid-123",
      "function_id": "resize-image",
      "cpu": 1,
      "memory_mb": 128
    }
    ```

### 3.2 etcd (Service Discovery)
* **Key Pattern:** `/functions/<function_id>/instances/<worker_id>`
* **Value:**
    ```json
    {
      "worker_ip": "10.0.0.5",  // The public IP of the Worker Node
      "proxy_port": 30005,      // The port exposed on the host
      "internal_vm_ip": "172.16.0.2"
    }
    ```
* **Lifecycle:** Keys must be attached to an etcd **Lease** (10s TTL) with KeepAlive.

## 4. Networking Strategy (Phase 1: TAP/Bridge)
*Do not implement vsock yet. We are doing standard IP networking first.*

1.  **Bridge Setup:** Host needs a bridge `fc-bridge0` (IP: `172.16.0.1/24`).
2.  **Per-VM Setup:**
    * Agent creates a TAP device (e.g., `tap0`).
    * Agent attaches `tap0` to `fc-bridge0`.
    * Agent boots Firecracker with kernel args to set Guest IP (e.g., `ip=172.16.0.2::172.16.0.1:255.255.255.0::eth0:off`).
3.  **Proxying:**
    * Agent opens a listener on Host (e.g., `:30005`).
    * Agent uses `httputil.ReverseProxy` to forward traffic to `http://172.16.0.2:80`.

## 5. Implementation Roadmap
Copilot, please guide implementation in this order. Do not jump ahead.

### Phase 1: The Firecracker Driver
**Task:** Create `cmd/agent/main.go`.
* Hardcode paths to a Kernel (`vmlinux`) and RootFS (`rootfs.ext4`).
* Use `firecracker-go-sdk` to start 1 VM.
* Ensure the VM process stays alive.

### Phase 2: The Networking Lab
**Task:** Update `cmd/agent`.
* Programmatically create a TAP interface.
* Configure VM network config in SDK.
* Verify connectivity (Agent can `ping` VM).

### Phase 3: The Control Loop
**Task:** Create `cmd/gateway` and update `cmd/agent`.
* Implement Redis `BRPOP` in Agent.
* Implement etcd Registration (with Lease) in Agent.
* Implement Gateway "Check etcd -> Enqueue -> Watch" logic.

## 6. Coding Constraints
* **No Frameworks:** Use Go Standard Library (`net/http`) where possible.
* **Concurrency:** Use Goroutines for the Agent's job loop.
* **Error Handling:** Fail fast. If VM fails to boot, log error and drop the job (for now).
* **Logging:** Use structured logging (JSON) for easier debugging.