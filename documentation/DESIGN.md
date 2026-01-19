# Aether - Design Document

A Function-as-a-Service (FaaS) platform built on Firecracker microVMs.

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                                  GATEWAY                                     │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │ Functions   │  │   Router    │  │  Discovery  │  │    Cold Start       │ │
│  │ API (CRUD)  │  │  (chi)      │  │  (etcd)     │  │    (singleflight)   │ │
│  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────────────┘ │
│                           │                                                  │
│                    ┌──────▼──────────┐                                       │
│                    │  Scale-Up Logic │  <-- Tracks active requests          │
│                    │   (1s interval) │      Pushes provision jobs           │
│                    └──────┬──────────┘                                       │
└───────────────────────────┼──────────────────────────────────────────────────┘
         │                  │                  │                   │
         ▼                  ▼                  ▼                   ▼
┌─────────────┐      ┌─────────────┐   ┌─────────────┐     ┌─────────────┐
│    MinIO    │      │    Redis    │   │    etcd     │     │  PostgreSQL │
│  (code.ext4)│      │   (queue)   │   │  (registry) │     │  (metadata) │
└─────────────┘      └─────────────┘   └─────────────┘     └─────────────┘
         ▲                  │                  ▲                   ▲
         │                  ▼                  │                   │
┌─────────────────────────────────────────────────────────────────────────────┐
│                                WORKER(S)                                     │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────┐ │
│  │ Code Cache  │  │Scale-Down   │  │  Registry   │  │    Job Consumer     │ │
│  │  (local)    │  │   Reaper    │  │  (etcd)     │  │ + Capacity Gate     │ │
│  │             │  │ (10s check) │  │             │  │   (Redis BLPOP)     │ │
│  └─────────────┘  └─────────────┘  └─────────────┘  └─────────────────────┘ │
│         │                │                │                   │              │
│         ▼                ▼                ▼                   ▼              │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │                           Instance Manager                               ││
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐                               ││
│  │  │Instance 1│  │Instance 2│  │Instance 3│                               ││
│  │  │ :30000   │  │ :30001   │  │ :30002   │                               ││
│  │  └──────────┘  └──────────┘  └──────────┘                               ││
│  └─────────────────────────────────────────────────────────────────────────┘│
│         │                │                │                                  │
│         ▼                ▼                ▼                                  │
│  ┌─────────────────────────────────────────────────────────────────────────┐│
│  │                      Firecracker microVMs                                ││
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐                   ││
│  │  │    VM 1      │  │    VM 2      │  │    VM 3      │                   ││
│  │  │ 172.16.0.2   │  │ 172.16.0.3   │  │ 172.16.0.4   │                   ││
│  │  └──────────────┘  └──────────────┘  └──────────────┘                   ││
│  └─────────────────────────────────────────────────────────────────────────┘│
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Implementation Details

### 1. Gateway Service

The gateway is the API entry point. It handles function management and request routing.

#### 1.1 Function Management API

**File:** `gateway/functions/api.go`

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/functions` | GET | List all functions |
| `/api/functions` | POST | Create a new function |
| `/api/functions/{id}` | GET | Get function details |
| `/api/functions/{id}` | PUT | Update function |
| `/api/functions/{id}` | DELETE | Delete function |
| `/api/functions/{id}/code` | POST | Upload function code (zip/tar.gz) |
| `/api/functions/{id}/invocations` | GET | Get invocation history |

**Code Upload Flow:**
1. Receive archive (zip or tar.gz) via POST body
2. Extract archive to temp directory
3. Build ext4 image using `mke2fs -d`
4. Upload ext4 to MinIO (`function-code/{id}/code.ext4`)
5. Update function metadata with code path
6. Publish code update event to Redis (`channel:code_update`)

```go
// gateway/functions/api.go
ext4Data, err := builder.BuildFromArchive(archiveData, filename)
api.minio.PutObject(codeBucket, objectPath, ext4Data)
api.redis.Publish(ctx, protocol.ChannelCodeUpdate, fnID)
```

#### 1.2 Request Router

**File:** `gateway/internal/router.go`

The router handles function invocations at `/functions/{funcID}/*`.

**Request Flow:**
1. Extract function ID from URL
2. Query etcd for available instances
3. If no instances → trigger cold start
4. Pick random instance from pool
5. Proxy request to worker

**Cold Start Prevention:**
Uses `golang.org/x/sync/singleflight` to deduplicate concurrent cold start requests:

```go
// Only one cold start per function, concurrent requests share the result
result, err, _ := h.coldStartSF.Do(fn.ID, func() (interface{}, error) {
    h.redis.PushJob(&job)
    return h.discovery.WaitForInstance(ctx, fn.ID, 30*time.Second)
})
```

#### 1.3 Service Discovery

**File:** `gateway/internal/discovery.go`

Queries etcd for registered instances:

```go
func (d *Discovery) GetInstances(ctx context.Context, functionID string) ([]protocol.FunctionInstance, error) {
    prefix := "/functions/" + functionID + "/instances/"
    resp, _ := d.client.Get(ctx, prefix, etcd.WithPrefix())
    // Parse JSON from each key-value pair
}

func (d *Discovery) WaitForInstance(ctx context.Context, functionID string, timeout time.Duration) {
    // Uses etcd Watch to wait for a new instance to appear
    watchChan := d.client.Watch(ctx, prefix, etcd.WithPrefix())
}
```

---

### 2. Worker Service

The worker manages Firecracker VMs and handles function execution.

#### 2.1 Job Consumer

**File:** `worker/internal/worker.go`

Listens to Redis queue for cold start requests:

```go
func (w *Worker) watchQueue(ctx context.Context) error {
    for {
        result, _ := client.BLPop(ctx, 5*time.Second, "queue:vm_provision").Result()
        w.handleJob([]byte(result[1]))
    }
}
```

**Job Structure:**
```go
type Job struct {
    RequestID  string
    FunctionID string
    VCPU       int
    MemoryMB   int
    Port       int
    Count      int  // Number of instances to spawn
}
```

#### 2.2 Instance Spawning

**File:** `worker/internal/worker.go`

The `SpawnInstance` function creates a new VM:

```go
func (w *Worker) SpawnInstance(functionID string) (*Instance, error) {
    // 1. Get code from cache (downloads from MinIO if needed)
    codePath, _ := w.codeCache.EnsureCode(functionID)
    
    // 2. Create instance with allocated resources
    instance := NewInstance(functionID, w.vmMgr, w.bridgeMgr)
    instance.Start(cfg)
    
    // 3. Wait for function to be ready (HTTP health check)
    instance.WaitReady(functionPort, 30*time.Second)
    
    // 4. Start reverse proxy on worker
    instance.StartProxy(proxyPort, functionPort)
    
    // 5. Register in etcd for discovery
    w.registry.RegisterInstance(functionID, instance.ID, proxyPort, instance.vmIP)
}
```

#### 2.3 Instance Management

**File:** `worker/internal/instance.go`

Each instance represents a running Firecracker VM:

```go
type Instance struct {
    ID              string
    FunctionID      string
    Status          InstanceStatus  // starting, ready, stopping, stopped
    vmMgr           *vm.Manager
    bridgeMgr       *network.BridgeManager
    vm              *vm.VM
    tap             *network.TAPDevice
    vmIP            string
    activeRequests  int64           // Atomic counter for scaling
    lastRequestTime time.Time       // For idle detection
    proxyPort       int
    proxyServer     *http.Server
}
```

**Instance Lifecycle:**
1. **Start:** Allocate IP → Create TAP → Attach to bridge → Launch Firecracker
2. **WaitReady:** Poll HTTP endpoint until function responds
3. **StartProxy:** Bind local port, reverse proxy to VM
4. **Stop:** Close proxy → Stop VM → Delete TAP → Release IP

#### 2.4 Code Cache

**File:** `worker/internal/code_cache.go`

Caches function code locally to avoid repeated MinIO downloads:

```go
func (c *CodeCache) EnsureCode(functionID string) (string, error) {
    localPath := filepath.Join(c.cacheDir, functionID+".ext4")
    
    // Check in-memory cache
    if path, ok := c.cached[functionID]; ok {
        return path, nil
    }
    
    // Check disk
    if _, err := os.Stat(localPath); err == nil {
        c.cached[functionID] = localPath
        return localPath, nil
    }
    
    // Download from MinIO
    obj, _ := c.minio.GetObject(c.bucket, functionID+"/code.ext4")
    data, _ := io.ReadAll(obj)
    os.WriteFile(localPath, data, 0644)
    
    return localPath, nil
}
```

#### 2.5 Auto-Scaling (Split Architecture)

**Scaling is now split between Gateway (scale-up) and Worker (scale-down).**

##### Gateway Scaler - Control Plane

**File:** `gateway/internal/scaler.go`

The gateway tracks active requests and decides when to scale up:

```go
type Scaler struct {
    mu             sync.RWMutex
    activeRequests map[string]int  // function_id -> active count
    redis          *redis.Client
    discovery      *internal.Discovery
    db             *db.DB
}

func (s *Scaler) check() {
    for functionID, activeReq := range s.activeRequests {
        currentInstances := s.discovery.GetInstanceCount(functionID)
        targetInstances := int(math.Ceil(float64(activeReq) / 3.0))
        
        if targetInstances > currentInstances {
            deficit := targetInstances - currentInstances
            for i := 0; i < deficit; i++ {
                s.pushProvisionJob(functionID)
            }
        }
    }
}
```

**Scale-Up Flow:**
1. Router calls `TrackRequest(fnID, +1)` on request start
2. Router calls `TrackRequest(fnID, -1)` on request complete  
3. Scaler checks every 1s: if `active/instances > 3`, push provision job
4. Fetches complete function metadata from database (entrypoint, VCPU, memory, port, env)
5. Pushes fully-populated Job to `queue:vm_provision`

##### Worker Reaper - Scale-Down

**File:** `worker/internal/reaper.go`

Workers manage instance lifecycle and scale down based on idle time:

```go
type Reaper struct {
    worker        *Worker
    checkInterval time.Duration  // 10s
}

func (r *Reaper) check() {
    for _, instances := range r.worker.instances {
        if len(instances) > 1 {
            r.scaleToOne(functionID, instances)  // 30s idle → 1 hot
        } else if len(instances) == 1 {
            r.scaleToZero(functionID, instances) // 5min idle → 0
        }
    }
}
```

**Scale-Down Flow:**
1. Proxy calls `instance.Touch()` on every request
2. Reaper checks every 10s for idle instances
3. Two-tier strategy:
   - Multiple instances: Scale to 1 hot after 30s idle
   - Single instance: Scale to zero after 5 minutes idle
4. Only stops 1 instance per cycle to prevent stampede

##### Worker Capacity Gate

**File:** `worker/internal/worker.go`

Workers self-regulate job consumption based on capacity:

```go
func (w *Worker) watchQueue(ctx context.Context) {
    for {
        // Capacity check before consuming job
        totalInstances := w.countAllInstances()
        freeRAM := system.GetFreeRAM()
        
        if totalInstances >= 10 || freeRAM < 500 {
            time.Sleep(500 * time.Millisecond)
            continue
        }
        
        // Consume job (1s timeout)
        result, err := w.redis.BLPop(ctx, 1*time.Second, "queue:vm_provision")
        if err == nil {
            w.handleJob(ctx, result[1])
        }
    }
}
```

##### Configuration

| Parameter | Value | Description |
|-----------|-------|-------------|
| CheckInterval (Gateway) | 1s | How often scaler checks |
| ScaleUpThreshold | 3 req/instance | Trigger scale up |
| CheckInterval (Reaper) | 10s | How often reaper runs |
| ScaleDownAfter | 30s | Idle time before scaling to 1 |
| ScaleToZeroAfter | 5m | Complete idle before scaling to 0 |
| MaxInstances | 10 | Per-worker capacity limit |
| MinFreeRAM | 500MB | Required RAM before spawning |

##### Multi-Worker Behavior

- Multiple workers compete for jobs via atomic BLPop
- Each worker checks capacity independently (no coordination)
- Reaper runs on all workers, but only stops 1 instance per cycle
- Gateway is unaware of worker count (fully decoupled)

**See:** `documentation/SPLIT_SCALER.md` for detailed analysis and test results.

#### 2.6 Code Update Handler

**File:** `worker/internal/worker.go`

Subscribes to Redis for code update notifications:

```go
func (w *Worker) WatchCodeUpdates(ctx context.Context) {
    pubsub := w.redis.Subscribe(ctx, "channel:code_update")
    
    for msg := range pubsub.Channel() {
        functionID := msg.Payload
        w.codeCache.Invalidate(functionID)  // Clear cache
        // Kill all running instances (they'll restart with new code)
        for _, inst := range w.instances[functionID] {
            go w.StopInstance(functionID, inst.ID)
        }
    }
}
```

---

### 3. Firecracker VM Management

**File:** `shared/vm/vm.go`

#### 3.1 VM Configuration

```go
type Config struct {
    KernelPath    string  // vmlinux kernel
    RootFSPath    string  // Base runtime (node-rootfs.ext4)
    CodeDrivePath string  // Function code (code.ext4)
    SocketPath    string  // Firecracker API socket
    VCPUCount     int64   // CPU cores
    MemSizeMB     int64   // Memory in MB
    TAPDeviceName string  // Network interface
    VMIP          string  // VM's IP address
    GatewayIP     string  // Bridge IP (172.16.0.1)
}
```

#### 3.2 VM Launch

```go
func (m *Manager) Launch(cfg Config) (*VM, error) {
    // Kernel boot args with static IP configuration
    bootArgs := fmt.Sprintf(
        "console=ttyS0 reboot=k panic=1 pci=off ipv6.disable=1 init=/init ip=%s::%s:255.255.255.0::eth0:off",
        cfg.VMIP, cfg.GatewayIP,
    )
    
    // Two drives: rootfs (runtime) + code overlay
    drives := []models.Drive{
        {DriveID: "rootfs", PathOnHost: cfg.RootFSPath, IsRootDevice: true},
        {DriveID: "code", PathOnHost: cfg.CodeDrivePath, IsReadOnly: true},
    }
    
    // Unique MAC address derived from IP
    mac := generateMACFromIP(cfg.VMIP)  // AA:FC:00:00:XX:YY
    
    machine, _ := firecracker.NewMachine(ctx, fcCfg, ...)
    machine.Start(ctx)
}
```

---

### 4. Network Management

**File:** `shared/network/bridge.go`

#### 4.1 Bridge Setup

Creates a Linux bridge for VM networking:

```go
func (bm *BridgeManager) EnsureBridge() error {
    // Create bridge: ip link add name fc-bridge0 type bridge
    // Add IP: ip addr add 172.16.0.1/24 dev fc-bridge0
    // Bring up: ip link set fc-bridge0 up
}
```

#### 4.2 TAP Device Management

Each VM gets a dedicated TAP device:

```go
func (bm *BridgeManager) CreateTAPDevice(tapName string) (*TAPDevice, error) {
    // ip tuntap add mode tap name tap0
    // ip link set tap0 up
}

func (bm *BridgeManager) AttachTAPToBridge(tapName string) error {
    // ip link set tap0 master fc-bridge0
}
```

#### 4.3 IP Allocation

```go
func (bm *BridgeManager) AllocateVMIP() (string, error) {
    // Allocates next available IP from 172.16.0.0/24
    // Skips .0 (network) and .1 (gateway)
}
```

---

### 5. Data Storage

#### 5.1 SQLite Database

**File:** `shared/db/db.go`

**Functions Table:**
```sql
CREATE TABLE functions (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    runtime TEXT NOT NULL,
    code_path TEXT DEFAULT '',
    vcpu INTEGER DEFAULT 1,
    memory_mb INTEGER DEFAULT 128,
    port INTEGER DEFAULT 3000,
    env_vars TEXT DEFAULT '{}',  -- JSON
    created_at DATETIME,
    updated_at DATETIME
);
```

**Invocations Table:**
```sql
CREATE TABLE invocations (
    id TEXT PRIMARY KEY,
    function_id TEXT NOT NULL,
    status TEXT NOT NULL,  -- success, error, timeout
    duration_ms INTEGER,
    started_at DATETIME,
    error_message TEXT,
    FOREIGN KEY (function_id) REFERENCES functions(id)
);
```

#### 5.2 etcd Keys

```
/workers/{worker_id}                          → WorkerNode JSON
/functions/{function_id}/instances/{inst_id}  → FunctionInstance JSON
```

**FunctionInstance:**
```json
{
    "instance_id": "inst-xxx",
    "function_id": "fn-xxx",
    "worker_id": "worker-xxx",
    "host_ip": "10.0.0.1",
    "proxy_port": 30000,
    "internal_ip": "172.16.0.2",
    "status": "ready",
    "started_at": "2024-01-01T00:00:00Z"
}
```

#### 5.3 Redis

**Queues:**
- `queue:vm_provision` - Cold start job queue (LPUSH/BLPOP)

**Pub/Sub Channels:**
- `channel:code_update` - Notifies workers of code changes

#### 5.4 MinIO

```
function-code/
  └── {function_id}/
      └── code.ext4     -- Built from uploaded zip/tar.gz
```

---

### 6. Request Flow

#### 6.1 Function Invocation (Cold Start)

```
1. User: GET /functions/fn-xxx/hello
         │
2. Gateway: Query etcd for instances
         │
3. Gateway: No instances → Check DB for function
         │
4. Gateway: LPUSH job to Redis queue
         │
5. Gateway: Watch etcd for new instance (30s timeout)
         │
6. Worker: BLPOP receives job
         │
7. Worker: Download code from MinIO (if not cached)
         │
8. Worker: Create TAP, allocate IP
         │
9. Worker: Launch Firecracker VM
         │
10. Worker: Wait for HTTP ready (poll VM)
          │
11. Worker: Start proxy on :30000
          │
12. Worker: Register instance in etcd
          │
13. Gateway: Watch triggered, gets instance
          │
14. Gateway: Proxy request to worker:30000
          │
15. Worker: Proxy forwards to VM:3000
          │
16. VM: Function handles request, returns response
```

#### 6.2 Function Invocation (Warm)

```
1. User: GET /functions/fn-xxx/hello
         │
2. Gateway: Query etcd → Found 3 instances
         │
3. Gateway: Random pick → instance on worker:30001
         │
4. Gateway: Proxy to worker:30001
         │
5. Worker: Proxy to VM 172.16.0.3:3000
         │
6. VM: Response
```

#### 6.3 Code Update

```
1. User: POST /api/functions/fn-xxx/code [code.zip]
         │
2. Gateway: Extract zip, build ext4
         │
3. Gateway: Upload to MinIO
         │
4. Gateway: Update DB with code_path
         │
5. Gateway: PUBLISH to channel:code_update
         │
6. Worker: Receives notification
         │
7. Worker: Invalidate code cache
         │
8. Worker: Kill all running instances of fn-xxx
         │
9. Next request: Cold start with new code
```

---

## Configuration

### Gateway Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| ETCD_ENDPOINTS | - | Comma-separated etcd endpoints |
| REDIS_ADDR | - | Redis address |
| PORT | 8080 | Gateway listen port |
| DB_PATH | - | SQLite database path |
| MINIO_ENDPOINT | - | MinIO endpoint |
| MINIO_ACCESS_KEY | - | MinIO access key |
| MINIO_SECRET_KEY | - | MinIO secret key |

### Worker Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| WORKER_ID | auto | Worker identifier |
| WORKER_IP | - | Worker's public IP (for instance registration) |
| ETCD_ENDPOINTS | - | Comma-separated etcd endpoints |
| REDIS_ADDR | - | Redis address |
| FIRECRACKER_BIN | firecracker | Path to firecracker binary |
| KERNEL_PATH | - | Path to vmlinux kernel |
| RUNTIME_PATH | - | Path to base rootfs.ext4 |
| SOCKET_DIR | - | Directory for VM sockets |
| BRIDGE_NAME | fc-bridge0 | Network bridge name |
| BRIDGE_CIDR | 172.16.0.0/24 | Bridge network CIDR |
| MINIO_ENDPOINT | - | MinIO endpoint |
| MINIO_ACCESS_KEY | - | MinIO access key |
| MINIO_SECRET_KEY | - | MinIO secret key |
| MINIO_BUCKET | function-code | MinIO bucket for code |
| CODE_CACHE_DIR | /var/aether/cache | Local code cache directory |

### Scaling Configuration (Hardcoded)

| Setting | Value | Description |
|---------|-------|-------------|
| CheckInterval | 1s | How often scaler runs |
| ScaleUpThreshold | 3 | Avg concurrent requests to trigger scale up |
| ScaleDownAfter | 30s | Idle time before scaling down extra instances |
| MinInstances | 1 | Minimum instances to keep warm |
| MaxInstances | 10 | Maximum instances per function |
| ScaleToZeroAfter | 5m | Complete idle time before scaling to zero |

---

## Project Structure

```
aether/
├── gateway/                 # API Gateway service
│   ├── main.go             # Entry point
│   ├── functions/          # Function CRUD API
│   │   └── api.go
│   └── internal/           # Internal packages
│       ├── router.go       # Request routing + cold start
│       ├── discovery.go    # etcd discovery
│       ├── proxy.go        # Reverse proxy
│       ├── redis.go        # Redis client
│       └── etcd.go         # etcd client
│
├── worker/                  # Worker service
│   ├── main.go             # Entry point
│   └── internal/           # Internal packages
│       ├── worker.go       # Core worker logic
│       ├── instance.go     # VM instance management
│       ├── scaler.go       # Auto-scaling
│       ├── code_cache.go   # Code caching
│       ├── etcd.go         # etcd registry
│       ├── redis.go        # Redis client
│       ├── proxy.go        # Per-instance proxy
│       └── config.go       # Configuration
│
├── shared/                  # Shared packages
│   ├── vm/                 # Firecracker VM management
│   │   └── vm.go
│   ├── network/            # Bridge/TAP management
│   │   ├── bridge.go
│   │   └── ip.go
│   ├── db/                 # SQLite database
│   │   ├── db.go
│   │   └── migrations/
│   ├── storage/            # MinIO client
│   │   └── minio.go
│   ├── builder/            # Code image builder
│   │   └── builder.go
│   ├── protocol/           # Shared types
│   │   └── messages.go
│   ├── logger/             # Structured logging
│   │   └── logger.go
│   ├── id/                 # ID generation
│   │   └── id.go
│   └── system/             # System info
│       └── system.go
│
├── deployment/              # Deployment configs
│   └── compose.yml         # Docker compose (etcd, Redis, MinIO)
│
├── scripts/                 # Helper scripts
│   ├── docker-to-rootfs.sh
│   ├── create-code-image.sh
│   └── prepare-runtime.sh
│
└── go.work                  # Go workspace
```

---

## Known Limitations

1. **Multi-worker load balancing**: First worker to `BLPOP` gets all cold start jobs
2. **Invocation logging**: Table exists but nothing writes to it
3. **Environment variables**: Stored in DB but not passed to VMs (needs MMDS)
4. **Health checks**: No instance health monitoring
5. **Request timeouts**: No timeout handling on function calls
6. **Authentication**: No API authentication
7. **Metrics**: No Prometheus/observability endpoints
