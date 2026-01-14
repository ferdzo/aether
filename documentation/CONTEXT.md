# Aether - Project Context

> LLM-readable summary of the current implementation state.

## What This Is

Aether is a **proof-of-concept Function-as-a-Service (FaaS) platform** that runs user functions inside **Firecracker microVMs**. It provides strong isolation, fast cold starts (~1.5s), and auto-scaling. Multi-host capable.

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.21+ |
| Virtualization | Firecracker microVMs |
| API Router | chi/v5 |
| Service Discovery | etcd v3 |
| Job Queue | Redis (LPUSH/BLPOP) |
| Object Storage | MinIO (S3-compatible) |
| Database | SQLite |
| Networking | Linux bridge + TAP devices |

## Services

### Gateway (`gateway/`)
- Listens on `:8080`
- Routes: `/functions/{funcID}/*` → function invocation
- Routes: `/api/functions` → CRUD API
- Connects to: etcd, Redis, MinIO, SQLite

### Worker (`worker/`)
- Runs on each host with Firecracker
- Consumes cold start jobs from Redis
- Manages Firecracker VMs
- Runs reverse proxy per instance (ports 30000+)
- Connects to: etcd, Redis, MinIO

## What Works ✅

### Function Management API
- `POST /api/functions` - Create function (name, runtime, vcpu, memory, port)
- `GET /api/functions` - List all functions
- `GET /api/functions/{id}` - Get function details
- `PUT /api/functions/{id}` - Update function
- `DELETE /api/functions/{id}` - Delete function
- `POST /api/functions/{id}/code` - Upload code (zip/tar.gz → builds ext4)

### Function Invocation
- `GET/POST /functions/{funcID}/*` - Invoke function
- Cold start triggered if no instances exist
- Random load balancing across available instances
- Request proxied: Gateway → Worker:30000 → VM:3000

### Cold Start Flow
1. Gateway checks etcd for instances
2. No instances → validates function exists in DB with uploaded code
3. Pushes job to Redis `queue:vm_provision`
4. Worker picks up job via BLPOP
5. Worker downloads code from MinIO (cached locally)
6. Worker creates TAP device, allocates IP (172.16.0.x)
7. Worker launches Firecracker VM
8. Worker waits for HTTP ready (polls VM)
9. Worker starts reverse proxy on port 30000+
10. Worker registers instance in etcd
11. Gateway discovers instance, proxies request

### Auto-Scaling
- Scaler runs every 1 second on worker
- Scale UP: avg concurrent requests > 3 → spawn new instance
- Scale DOWN: instance idle > 30s (keep min 1)
- Scale to ZERO: all instances idle > 5 minutes → terminate all

### Code Updates
- Upload new code via API
- Gateway publishes to Redis `channel:code_update`
- Workers invalidate cache + kill running instances
- Next request cold starts with new code

### Multi-Worker Support
- Multiple workers can register in etcd
- Each worker manages its own VMs
- Gateway discovers all instances across workers

### Environment Variables
- Set via API: `POST /api/functions` with `env_vars` field
- Passed to VMs via MMDS (Firecracker metadata service)
- Boot token validation for security
- Available in function as `process.env.VAR_NAME`

## What Doesn't Work / Not Implemented ❌

| Feature | Status |
|---------|--------|
| **Invocation logging** | Table exists, nothing writes to it |
| **Request timeouts** | No timeout on function calls |
| **Health checks** | No periodic health monitoring of instances |
| **Authentication** | No API auth |
| **Metrics/observability** | No Prometheus endpoints |
| **Multi-worker load balancing** | First worker to BLPOP gets all cold start jobs |

## Data Flow Diagram

```
User Request
     │
     ▼
┌─────────┐   etcd lookup   ┌─────────┐
│ Gateway │ ───────────────▶│  etcd   │
└─────────┘                 └─────────┘
     │                           ▲
     │ (no instances)            │ register
     ▼                           │
┌─────────┐   BLPOP job    ┌─────────┐
│  Redis  │ ◀──────────────│ Worker  │
└─────────┘                └─────────┘
                                │
                                ▼
                          ┌──────────┐
                          │ Firecracker│
                          │    VM     │
                          └──────────┘
```

## Key Files

| File | Purpose |
|------|---------|
| `gateway/main.go` | Gateway entry point |
| `gateway/internal/router.go` | Request routing, cold start logic |
| `gateway/functions/api.go` | Function CRUD API handlers |
| `worker/main.go` | Worker entry point |
| `worker/internal/worker.go` | Job consumer, instance spawning, MMDS setup |
| `worker/internal/scaler.go` | Auto-scaling logic |
| `worker/internal/instance.go` | VM instance lifecycle |
| `worker/internal/code_cache.go` | MinIO code caching |
| `shared/vm/vm.go` | Firecracker VM management + MMDS config |
| `shared/network/bridge.go` | TAP/bridge networking |
| `shared/db/db.go` | SQLite operations |
| `shared/storage/minio.go` | MinIO client |
| `shared/builder/builder.go` | Code → ext4 image builder |
| `shared/protocol/messages.go` | Shared data structures |
| `init/main.go` | aether-env binary source (MMDS fetcher) |

## Database Schema

```sql
-- functions table
id TEXT PRIMARY KEY
name TEXT NOT NULL
runtime TEXT NOT NULL
code_path TEXT DEFAULT ''
vcpu INTEGER DEFAULT 1
memory_mb INTEGER DEFAULT 128
port INTEGER DEFAULT 3000
env_vars TEXT DEFAULT '{}'  -- JSON
created_at DATETIME
updated_at DATETIME

-- invocations table (exists but unused)
id TEXT PRIMARY KEY
function_id TEXT NOT NULL
status TEXT NOT NULL
duration_ms INTEGER
started_at DATETIME
error_message TEXT
```

## etcd Keys

```
/workers/{worker_id}                          → WorkerNode JSON
/functions/{function_id}/instances/{inst_id}  → FunctionInstance JSON
```

## Redis

- Queue: `queue:vm_provision` (cold start jobs)
- Pub/Sub: `channel:code_update` (code change notifications)

## MinIO

```
function-code/
  └── {function_id}/
      └── code.ext4
```

## VM Networking

- Bridge: `fc-bridge0` at `172.16.0.1/24`
- VMs get IPs: `172.16.0.2`, `172.16.0.3`, ...
- Each VM has its own TAP device attached to bridge
- Worker proxy binds to `0.0.0.0:30000+` and forwards to VM

## Runtime Requirements

- Linux with KVM support (`/dev/kvm`)
- Firecracker binary
- vmlinux kernel
- Base rootfs (e.g., `node-rootfs.ext4` for Node.js functions)
- Root/sudo for network and VM operations

## Environment Variables (MMDS)

Environment variables are passed to VMs via Firecracker's **MMDS (Microvm Metadata Service)**.

### How It Works

```
┌─────────────────────────────────────────────────────────────┐
│                         WORKER                               │
│  1. Generate boot token                                      │
│  2. Configure MMDS: {"token": "tok-xxx", "env": {...}}      │
│  3. Pass token in kernel args: aether_token=tok-xxx         │
│  4. Start VM                                                 │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                           VM                                 │
│  /init runs:                                                 │
│  1. Mount /proc, /sys, /dev                                  │
│  2. Mount code drive (/dev/vdb → /code)                      │
│  3. Run: aether-env node handler.js                          │
│                                                              │
│  aether-env:                                                 │
│  1. Parse boot token from /proc/cmdline                      │
│  2. GET http://169.254.169.254/ (Accept: application/json)   │
│  3. Validate token matches                                   │
│  4. Export env vars                                          │
│  5. exec node handler.js                                     │
└─────────────────────────────────────────────────────────────┘
```

### Security Model

- **VM Isolation**: Each VM has its own MMDS, VMs cannot access each other's metadata
- **Within VM**: Any process can read MMDS (like AWS Lambda)
- **Token Validation**: Boot token ensures init is reading the correct metadata

### Key Components

| Component | Location | Purpose |
|-----------|----------|---------|
| `aether-env` | `/usr/bin/aether-env` in rootfs | Fetches MMDS, exports env, execs command |
| `/init` | Root of rootfs | Mounts filesystems, runs aether-env |
| MMDS data | Set by worker via Firecracker API | Contains token + env vars |

### Rootfs Setup

The rootfs needs two files:

**1. `/init` script:**
```bash
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /code
mount /dev/vdb /code 2>/dev/null
cd /code
export PATH=/usr/local/bin:/usr/bin:/bin
exec /usr/bin/aether-env node handler.js
```

**2. `/usr/bin/aether-env` binary:**
```bash
# Build from init/ directory
cd init && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o aether-env .

# Copy to rootfs
sudo mount -o loop node-rootfs.ext4 /mnt
sudo cp aether-env /mnt/usr/bin/aether-env
sudo chmod +x /mnt/usr/bin/aether-env
# Also create/update /mnt/init
sudo umount /mnt
```

## Runtime Support

- **Current:** Node.js only
- **Extensible:** Any runtime possible with custom rootfs + init script
- **Function format:** Currently expects `handler.js`, but easily configurable

## Deployment

- **Architecture:** Multi-host capable
- **Scope:** Proof of Concept (no target scale requirements)

### Worker Configuration

Critical `.env` settings for worker:

```bash
WORKER_IP=<public_ip_reachable_by_gateway>  # NOT 127.0.0.1!
KERNEL_PATH=/path/to/vmlinux
RUNTIME_PATH=/path/to/node-rootfs.ext4
SOCKET_DIR=/tmp/firecracker  # Must exist!
BRIDGE_NAME=fc-bridge0
BRIDGE_CIDR=172.16.0.0/24
```

**Common issues:**
- `WORKER_IP` wrong → gateway can't reach worker proxy
- `SOCKET_DIR` doesn't exist → Firecracker fails to start
- Firewall blocking ports 30000+ → requests timeout

## Adding New Runtimes

To add a new runtime (e.g., Python):

1. Create a rootfs image with the runtime installed:
   ```bash
   docker create python:3.11-slim
   # Export and convert to ext4
   ```

2. Update `/init` to run the correct interpreter:
   ```bash
   exec /usr/bin/aether-env python3 handler.py
   ```

3. Copy `aether-env` to the rootfs

4. Use the new rootfs when starting workers
