# Project: Go-Firecracker FaaS Platform

> ## Status update (post-roadmap)
>
> This file is an early project charter. The Phase 1/2/3 roadmap in §5 has been
> completed and overtaken; keep the constraints in §6 as policy, not the phases.
>
> Since this was written, the following have been implemented:
> - **Queue:** the Redis list was replaced by a durable Redis Stream
>   (`stream:vm_provision`) read by consumer group `aether-workers`
>   (`XReadGroup`), with `XACK` on success and a stale-claim reaper
>   (`XPendingExt`/`XClaim`).
> - **Networking:** `fc-bridge0`/TAP plus a `NET_MODE=netns` alternative, both
>   with host egress NAT and guest DNS injection (`GUEST_DNS` → MMDS →
>   `/etc/resolv.conf`); `NET_MODE=none` runs offline, NIC-less VMs.
> - **Multi-runtime images:** guest rootfs images are resolved by name from
>   object storage (`runtimes/<name>/rootfs.ext4`) instead of one hardcoded rootfs.
> - **Warm scaling:** recently invoked functions keep a warm window and never
>   scale below `MinInstances`; scaling values remain hardcoded in `worker/main.go`.
> - **Durable provisioning:** ACK-at-spawn with in-flight/durable guards, so a slow
>   spawn (or a long job) is not re-run and blocks nothing; process jobs also get
>   an etcd `JobRecord` that outlives the worker.
> - **Job execution path:** a process-mode guest supervisor (`init/main.go`) and a
>   worker job core (`worker/internal/job.go`) run non-HTTP workloads, reporting
>   `AETHER_EXIT:<nonce>:<code>` and resetting the VM.
>
> Authoritative docs: [`ARCHITECTURE.md`](../documentation/ARCHITECTURE.md)
> (direction) and [`CONTEXT.md`](../documentation/CONTEXT.md) (current state).

## 1. Project Overview
We are building a custom Function-as-a-Service (FaaS) platform from scratch to learn distributed systems and virtualization.
**Goal:** Create a system where a user can request a function execution, and the system spins up a Firecracker microVM, executes the code, and returns the result.

## 2. Core Architecture
The system follows a "Control Plane / Data Plane" separation.

### 2.1 The Tech Stack
* **Language:** Go (Golang) 1.21+
* **Virtualization:** Firecracker MicroVM (via `firecracker-go-sdk`)
* **Queue/Signaling:** Redis (Streams; the original List design is obsolete — see the status update above)
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
    * Reads the provision stream from **Redis** (`XReadGroup`, consumer group `aether-workers`) to pick up jobs.
    * Launches Firecracker VMs using the Go SDK.
    * Configures Host-Local Networking (TAP devices).
    * Registers specific VM IP:Port in **etcd** with a Lease (TTL).
    * Proxies traffic from Host Port -> VM Internal IP.

## 3. Data Contracts & Schema

### 3.1 Redis (Work Queue)
* **Key:** `stream:vm_provision` (the historical list `queue:vm_provision` is no longer used)
* **Type:** Stream with consumer group `aether-workers` (`XADD` / `XReadGroup`, `XACK` on success)
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
* **Key Pattern:** `/functions/<function_id>/instances/<instance_id>`
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