# Aether Developer Notes

Internal documentation on implementation details and design decisions.

---

## Table of Contents

1. [Project Structure](#project-structure)
2. [Gateway Implementation](#gateway-implementation)
3. [Worker Implementation](#worker-implementation)
4. [VM Management](#vm-management)
5. [Networking](#networking)
6. [MMDS & Environment Variables](#mmds--environment-variables)
7. [Code Building & Storage](#code-building--storage)
8. [Auto-Scaling](#auto-scaling)
9. [Service Discovery](#service-discovery)
10. [Telemetry & Observability](#telemetry--observability)
11. [Design Decisions](#design-decisions)

---

## Project Structure

```
aether/
├── gateway/                 # HTTP API gateway
│   ├── main.go             # Entry point, wires everything together
│   ├── functions/          # Function CRUD API handlers
│   │   └── api.go          # REST handlers for /api/functions
│   └── internal/           
│       ├── router.go       # Request routing + cold start trigger
│       ├── discovery.go    # etcd client for instance discovery
│       ├── proxy.go        # HTTP reverse proxy to workers
│       ├── redis.go        # Redis client wrapper
│       └── etcd.go         # etcd connection helper
│
├── worker/                  # VM manager
│   ├── main.go             # Entry point
│   └── internal/
│       ├── worker.go       # Core: job queue, spawning, lifecycle
│       ├── instance.go     # Single VM instance management
│       ├── scaler.go       # Auto-scaling logic
│       ├── code_cache.go   # Local caching of code images
│       ├── etcd.go         # Instance registration
│       ├── redis.go        # Job queue client
│       ├── proxy.go        # Per-instance reverse proxy
│       └── config.go       # Configuration struct
│
├── shared/                  # Shared libraries (used by gateway + worker)
│   ├── vm/
│   │   └── vm.go           # Firecracker SDK wrapper
│   ├── network/
│   │   ├── bridge.go       # Linux bridge + TAP management
│   │   └── ip.go           # IP allocation
│   ├── db/
│   │   ├── db.go           # SQLite wrapper + CRUD
│   │   └── migrations/     # SQL migration files
│   ├── storage/
│   │   └── minio.go        # MinIO S3 client
│   ├── builder/
│   │   └── builder.go      # Builds ext4 from zip/tar.gz
│   ├── protocol/
│   │   └── messages.go     # Shared structs: Job, FunctionInstance, etc.
│   ├── id/
│   │   └── id.go           # ID generators (fn-xxx, inst-xxx, etc.)
│   ├── logger/
│   │   └── logger.go       # Structured logging (slog)
│   └── telemetry/
│       └── telemetry.go    # OpenTelemetry setup + VM log capture
│
├── init/                    # Runs INSIDE the VM
│   ├── main.go             # aether-env binary source
│   └── go.mod              # Separate module (builds standalone)
│
└── scripts/                 # Helper scripts
    ├── docker-to-rootfs.sh # Convert Docker image to ext4
    └── create-code-image.sh # Build ext4 from code directory
```

---

## Gateway Implementation

### Entry Point (`gateway/main.go`)

1. Loads `.env` file
2. Connects to etcd, Redis, MinIO, SQLite
3. Runs DB migrations
4. Sets up chi router with middleware
5. Mounts routes:
   - `/api/functions` → CRUD API
   - `/functions/{funcID}/*` → Invocation handler

### Request Routing (`gateway/internal/router.go`)

The `Handler` struct handles all function invocations:

```go
type Handler struct {
    discovery   *Discovery       // etcd client
    redis       *RedisClient     // for pushing cold start jobs
    db          *db.DB           // for validating functions exist
    coldStartSF singleflight.Group  // prevents duplicate cold starts
}
```

**Flow:**

1. Extract `funcID` from URL
2. Query etcd for instances: `discovery.GetInstances(funcID)`
3. If instances exist → pick random one, proxy request
4. If no instances:
   a. Validate function exists in DB with uploaded code
   b. Trigger cold start via `singleflight` (prevents N concurrent cold starts)
   c. Wait for instance to appear in etcd (with timeout)
   d. Proxy to new instance

**Why singleflight?**

Without it, 100 concurrent requests to a cold function would trigger 100 cold starts. With singleflight, only ONE cold start happens, and all 100 requests wait for it.

```go
result, err, _ := h.coldStartSF.Do(fn.ID, func() (interface{}, error) {
    // Only one goroutine executes this
    h.redis.PushJob(&job)
    return h.discovery.WaitForInstance(ctx, fn.ID, 30*time.Second)
})
```

### Function API (`gateway/functions/api.go`)

Standard REST handlers:

- `Create` - Generate ID, insert into DB
- `List` - Query all from DB
- `Get` - Query by ID
- `Update` - Update fields
- `Delete` - Remove from DB
- `UploadCode` - Build ext4, upload to MinIO, publish update event

**Code Upload Flow:**

```go
func (api *FunctionsAPI) UploadCode(w http.ResponseWriter, r *http.Request) {
    // 1. Read uploaded file
    file, header, _ := r.FormFile("file")
    data, _ := io.ReadAll(file)
    
    // 2. Build ext4 image
    ext4Data, _ := builder.BuildFromArchive(data, header.Filename)
    
    // 3. Upload to MinIO
    objectPath := fnID + "/code.ext4"
    api.minio.PutObject("function-code", objectPath, ext4Data)
    
    // 4. Update DB
    api.db.UpdateFunction(fnID, db.FunctionUpdate{CodePath: objectPath})
    
    // 5. Notify workers of new code
    api.redis.Publish(ctx, "channel:code_update", fnID)
}
```

---

## Worker Implementation

### Entry Point (`worker/main.go`)

1. Loads `.env`
2. Connects to etcd, Redis, MinIO
3. Registers worker in etcd (with lease for auto-expiry)
4. Creates `CodeCache` for local code caching
5. Starts 3 goroutines:
   - `worker.Run()` - Main job queue consumer
   - `scaler.Run()` - Auto-scaling loop
   - `worker.WatchCodeUpdates()` - Redis pub/sub for code changes

### Worker Core (`worker/internal/worker.go`)

**Data Structures:**

```go
type Worker struct {
    instances      map[string][]*Instance  // functionID -> running instances
    functionConfig map[string]FunctionConfig  // cached function configs
    nextPort       int                     // next proxy port to assign
    codeCache      *CodeCache              // local code file cache
    // ...
}

type FunctionConfig struct {
    VCPU    int64
    MemMB   int64
    Port    int
    EnvVars map[string]string
}
```

**Job Queue Consumer:**

```go
func (w *Worker) watchQueue(ctx context.Context) {
    for {
        // BLPOP blocks until job available (5s timeout)
        result, _ := client.BLPop(ctx, 5*time.Second, "queue:vm_provision").Result()
        
        var job protocol.Job
        json.Unmarshal([]byte(result[1]), &job)
        
        // Spawn requested number of instances in parallel
        for i := 0; i < job.Count; i++ {
            go w.SpawnInstance(job.FunctionID)
        }
    }
}
```

**Spawning an Instance:**

```go
func (w *Worker) SpawnInstance(functionID string) (*Instance, error) {
    // 1. Get code (downloads from MinIO if not cached)
    codePath, _ := w.codeCache.EnsureCode(functionID)
    
    // 2. Get function config
    fnCfg := w.functionConfig[functionID]
    
    // 3. Generate boot token for MMDS
    bootToken := id.GenerateToken()
    mmdsData := map[string]interface{}{
        "token": bootToken,
        "env":   fnCfg.EnvVars,
    }
    
    // 4. Create instance
    instance := NewInstance(functionID, w.vmMgr, w.bridgeMgr)
    instance.Start(InstanceConfig{
        KernelPath: w.cfg.KernelPath,
        RuntimePath: w.cfg.RuntimePath,
        CodePath: codePath,
        MMDSData: mmdsData,
        BootToken: bootToken,
        // ...
    })
    
    // 5. Wait for function to be ready
    instance.WaitReady(fnCfg.Port, 30*time.Second)
    
    // 6. Start reverse proxy
    proxyPort := w.nextPort++
    instance.StartProxy(proxyPort, fnCfg.Port)
    
    // 7. Register in etcd
    w.registry.RegisterInstance(functionID, instance.ID, proxyPort, instance.vmIP)
    
    return instance, nil
}
```

### Instance (`worker/internal/instance.go`)

Represents a single running VM:

```go
type Instance struct {
    ID              string
    FunctionID      string
    Status          InstanceStatus
    vmMgr           *vm.Manager
    bridgeMgr       *network.BridgeManager
    vm              *vm.VM          // Firecracker machine
    tap             *network.TAPDevice
    vmIP            string
    activeRequests  int64           // atomic, for scaling
    lastRequestTime time.Time       // for idle detection
    proxyPort       int
    proxyServer     *http.Server
}
```

**Start Flow:**

1. Allocate IP from bridge manager
2. Create TAP device
3. Attach TAP to bridge
4. Build VM config (kernel, rootfs, code drive, MMDS)
5. Launch Firecracker via SDK
6. Wait for VM to boot

**WaitReady:**

Polls the function's HTTP endpoint until it responds:

```go
func (i *Instance) WaitReady(port int, timeout time.Duration) error {
    url := fmt.Sprintf("http://%s:%d/", i.vmIP, port)
    deadline := time.Now().Add(timeout)
    
    for time.Now().Before(deadline) {
        resp, err := http.Get(url)
        if err == nil && resp.StatusCode < 500 {
            return nil  // Ready!
        }
        time.Sleep(50 * time.Millisecond)
    }
    return fmt.Errorf("timeout")
}
```

**Proxy:**

Each instance has its own HTTP server that tracks active requests:

```go
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    p.instance.IncrementActiveRequests()
    defer p.instance.DecrementActiveRequests()
    p.proxy.ServeHTTP(w, r)  // httputil.ReverseProxy
}
```

---

## VM Management

### Firecracker Wrapper (`shared/vm/vm.go`)

Wraps the firecracker-go-sdk:

```go
type Config struct {
    KernelPath    string
    RootFSPath    string              // Base runtime (node-rootfs.ext4)
    CodeDrivePath string              // Function code (code.ext4)
    SocketPath    string              // API socket path
    VCPUCount     int64
    MemSizeMB     int64
    TAPDeviceName string              // Network interface
    VMIP          string
    GatewayIP     string
    BootToken     string              // For MMDS validation
    MMDSData      map[string]interface{}  // Metadata to serve
}
```

**Launch Flow:**

```go
func (m *Manager) Launch(cfg Config) (*VM, error) {
    // 1. Build kernel boot args
    bootArgs := fmt.Sprintf(
        "console=ttyS0 reboot=k panic=1 pci=off init=/init ip=%s::%s:255.255.255.0::eth0:off",
        cfg.VMIP, cfg.GatewayIP,
    )
    if cfg.BootToken != "" {
        bootArgs += " aether_token=" + cfg.BootToken
    }
    
    // 2. Configure drives
    drives := []models.Drive{
        {DriveID: "rootfs", PathOnHost: cfg.RootFSPath, IsRootDevice: true},
        {DriveID: "code", PathOnHost: cfg.CodeDrivePath, IsReadOnly: true},
    }
    
    // 3. Configure network with MMDS
    networkInterface := firecracker.NetworkInterface{
        StaticConfiguration: &firecracker.StaticNetworkConfiguration{
            HostDevName: cfg.TAPDeviceName,
            MacAddress:  generateMACFromIP(cfg.VMIP),
        },
        AllowMMDS: true,  // Critical for MMDS access!
    }
    
    // 4. Configure MMDS
    fcCfg := firecracker.Config{
        MmdsVersion: firecracker.MMDSv1,
        MmdsAddress: net.ParseIP("169.254.169.254"),
        // ...
    }
    
    // 5. Create and start machine
    machine, _ := firecracker.NewMachine(ctx, fcCfg, ...)
    machine.Start(ctx)
    
    // 6. Set MMDS data (after start!)
    machine.SetMetadata(ctx, cfg.MMDSData)
    
    return &VM{Machine: machine, ...}, nil
}
```

**Key Detail:** `SetMetadata` must be called AFTER `Start()`. The MMDS data is set via the Firecracker API socket after the VM is running.

---

## Networking

### Bridge Manager (`shared/network/bridge.go`)

Manages the Linux bridge and TAP devices:

```go
type BridgeManager struct {
    bridgeName string           // e.g., "fc-bridge0"
    bridgeCIDR string           // e.g., "172.16.0.0/24"
    gatewayIP  string           // e.g., "172.16.0.1"
    allocatedIPs map[string]bool
    nextTAPIndex int
}
```

**EnsureBridge:**

```bash
# What it does internally:
ip link add name fc-bridge0 type bridge
ip addr add 172.16.0.1/24 dev fc-bridge0
ip link set fc-bridge0 up
```

**CreateTAPDevice:**

```bash
ip tuntap add mode tap name tap0
ip link set tap0 up
ip link set tap0 master fc-bridge0
```

**IP Allocation:**

Simple sequential allocation from the /24 subnet:
- `.1` = gateway (bridge IP)
- `.2` = first VM
- `.3` = second VM
- etc.

---

## MMDS & Environment Variables

### The Problem

Functions need environment variables (secrets, config). How do we get them from the gateway's database into the VM?

### The Solution: MMDS

Firecracker's MMDS (Microvm Metadata Service) is a per-VM HTTP endpoint at `169.254.169.254` (same as AWS EC2 metadata).

**Flow:**

```
Gateway DB                Worker                    VM
    │                        │                       │
    │  Job with env_vars     │                       │
    ├───────────────────────>│                       │
    │                        │                       │
    │                        │ Configure MMDS        │
    │                        │ {"token":"X","env":{}}│
    │                        │                       │
    │                        │ Boot with token in    │
    │                        │ kernel args           │
    │                        ├──────────────────────>│
    │                        │                       │
    │                        │     GET 169.254...    │
    │                        │<──────────────────────│
    │                        │                       │
    │                        │  {"token":"X",        │
    │                        │   "env":{"KEY":"V"}}  │
    │                        ├──────────────────────>│
    │                        │                       │
    │                        │      Validate token   │
    │                        │      Export env vars  │
    │                        │      Start function   │
```

### Why the Token?

The boot token ensures the init process is reading the correct MMDS. It's passed via kernel command line and must match the token in MMDS.

Without it, a compromised VM could theoretically spoof another VM's metadata (though Firecracker already isolates MMDS per-VM).

### aether-env Binary (`init/main.go`)

A static Go binary that runs inside the VM:

```go
func main() {
    // 1. Parse token from /proc/cmdline
    bootToken := parseBootToken()
    
    // 2. Fetch MMDS
    req, _ := http.NewRequest("GET", "http://169.254.169.254/", nil)
    req.Header.Set("Accept", "application/json")  // Critical!
    resp, _ := client.Do(req)
    
    // 3. Parse and validate
    var metadata MMDSData
    json.Unmarshal(body, &metadata)
    if metadata.Token != bootToken {
        // Abort
    }
    
    // 4. Export env vars
    for key, value := range metadata.Env {
        os.Setenv(key, value)
    }
    
    // 5. Exec the function
    syscall.Exec(binary, os.Args[1:], os.Environ())
}
```

**Key Detail:** Must use `Accept: application/json` header. Without it, MMDS returns a directory listing:

```
env/
token
```

With the header, it returns JSON:

```json
{"token":"tok-xxx","env":{"KEY":"value"}}
```

---

## Code Building & Storage

### Build Flow

1. User uploads `code.zip` or `code.tar.gz`
2. Gateway extracts to temp directory
3. Creates ext4 filesystem with the code
4. Uploads to MinIO

### Builder (`shared/builder/builder.go`)

Uses `mke2fs -d` to create ext4 from a directory:

```go
func BuildFromArchive(data []byte, filename string) ([]byte, error) {
    // 1. Extract archive to temp dir
    tempDir := extractArchive(data, filename)
    
    // 2. Create ext4 image
    outputFile := tempDir + ".ext4"
    cmd := exec.Command("mke2fs",
        "-t", "ext4",
        "-d", tempDir,      // Source directory
        outputFile,
        "10M",              // Size
    )
    cmd.Run()
    
    // 3. Read and return
    return os.ReadFile(outputFile)
}
```

### Storage

MinIO stores code at: `function-code/{function_id}/code.ext4`

### Caching

Workers cache code locally to avoid repeated MinIO downloads:

```go
func (c *CodeCache) EnsureCode(functionID string) (string, error) {
    localPath := filepath.Join(c.cacheDir, functionID+".ext4")
    
    // Check memory cache
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

---

## Auto-Scaling

### Scaler (`worker/internal/scaler.go`)

Runs every second, checks each function:

```go
func (s *Scaler) checkFunction(functionID string, instances []*Instance) {
    // Calculate metrics
    totalActive := sum(inst.GetActiveRequests() for inst in instances)
    avgConcurrency := totalActive / len(instances)
    minIdleDuration := min(inst.IdleDuration() for inst in instances)
    
    // Scale UP: high concurrency
    if avgConcurrency > threshold && len(instances) < max {
        go s.worker.SpawnInstance(functionID)
    }
    
    // Scale to ZERO: all idle for 5+ minutes
    if totalActive == 0 && minIdleDuration > 5*time.Minute {
        for _, inst := range instances {
            go s.worker.StopInstance(functionID, inst.ID)
        }
        return
    }
    
    // Normal scale DOWN: keep min instances warm
    if len(instances) > minInstances {
        for _, inst := range instances {
            if inst.GetActiveRequests() == 0 && inst.IdleDuration() > 30*time.Second {
                go s.worker.StopInstance(functionID, inst.ID)
                break  // Only scale down one at a time
            }
        }
    }
}
```

### Request Tracking

Each instance tracks active requests atomically:

```go
func (i *Instance) IncrementActiveRequests() {
    atomic.AddInt64(&i.activeRequests, 1)
    i.mu.Lock()
    i.lastRequestTime = time.Now()
    i.mu.Unlock()
}
```

---

## Service Discovery

### etcd Keys

```
/workers/{worker_id} → WorkerNode JSON
/functions/{function_id}/instances/{instance_id} → FunctionInstance JSON
```

### Registration

Worker registers itself with a lease (TTL). If worker dies, lease expires, and registration is auto-deleted.

```go
func (r *Registry) RegisterWorker() (func(), error) {
    lease, _ := r.client.Grant(ctx, 30)  // 30 second TTL
    
    r.client.Put(ctx, workerKey, workerJSON, clientv3.WithLease(lease.ID))
    
    // Keep alive in background
    keepAliveCh, _ := r.client.KeepAlive(ctx, lease.ID)
    go func() {
        for range keepAliveCh {}  // Consume keep-alive responses
    }()
    
    return func() { r.client.Revoke(ctx, lease.ID) }, nil
}
```

### Discovery

Gateway watches etcd for instance changes:

```go
func (d *Discovery) WaitForInstance(ctx context.Context, functionID string, timeout time.Duration) {
    prefix := "/functions/" + functionID + "/instances/"
    watchChan := d.client.Watch(ctx, prefix, clientv3.WithPrefix())
    
    for resp := range watchChan {
        for _, ev := range resp.Events {
            if ev.Type == clientv3.EventTypePut {
                // New instance registered!
                return parseInstance(ev.Kv.Value)
            }
        }
    }
}
```

---

## Design Decisions

### Why Firecracker?

- Sub-second boot times (vs 10+ seconds for traditional VMs)
- Strong isolation (vs containers)
- Minimal attack surface
- Used by AWS Lambda

### Why Go?

- Single binary deployment
- Good concurrency primitives
- Strong typing
- Fast compilation

### Why etcd for Discovery?

- Built-in TTL/leases
- Watch API for real-time updates
- Consistent (Raft consensus)
- Already battle-tested (Kubernetes uses it)

### Why Redis Queue (not Kafka, RabbitMQ)?

- Simple LPUSH/BLPOP is enough
- Already using Redis (for pub/sub)
- No need for persistence (jobs are ephemeral)

### Why SQLite (not Postgres)?

- POC simplicity
- No external dependency
- Easy to migrate later (SQL is SQL)

### Why ext4 for Code?

- Firecracker mounts drives directly
- No filesystem overhead
- Read-only mount for security

### Why singleflight for Cold Starts?

Prevents thundering herd. 100 requests to cold function = 1 cold start, not 100.

### Why MMDS for Env Vars?

- Built into Firecracker
- No network dependency
- Per-VM isolation
- Same pattern as AWS Lambda/EC2

---

## Init Script (Inside the VM)

The `/init` script is the first process that runs inside the Firecracker VM (PID 1). It's responsible for setting up the environment and launching the function.

### Boot Sequence

```
Firecracker starts VM
         │
         ▼
Kernel boots (vmlinux)
         │
         ▼
Kernel runs /init (from kernel args: init=/init)
         │
         ▼
┌─────────────────────────────────────────────────┐
│ /init script runs as PID 1                      │
│                                                 │
│ 1. Mount /proc (required for /proc/cmdline)    │
│ 2. Mount /sys                                   │
│ 3. Mount /dev (for device access)               │
│ 4. Mount code drive (/dev/vdb → /code)          │
│ 5. cd /code                                     │
│ 6. exec aether-env node handler.js              │
└─────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────┐
│ aether-env (replaces init as PID 1)             │
│                                                 │
│ 1. Read /proc/cmdline for boot token            │
│ 2. Fetch MMDS at 169.254.169.254               │
│ 3. Validate token                               │
│ 4. Export env vars                              │
│ 5. exec node handler.js                         │
└─────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────┐
│ node handler.js (is now PID 1)                  │
│                                                 │
│ • Has all env vars available                    │
│ • Listens on configured port                   │
│ • Handles requests                              │
└─────────────────────────────────────────────────┘
```

### Minimal Init Script

```bash
#!/bin/sh

# Mount essential filesystems
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null

# Mount code drive (second drive attached by Firecracker)
# /dev/vdb is the "code" drive in vm.go
mkdir -p /code
mount /dev/vdb /code 2>/dev/null

# Change to code directory
cd /code

# Set PATH (node might be in /usr/local/bin)
export PATH=/usr/local/bin:/usr/bin:/bin

# Load env vars from MMDS and run function
# aether-env will:
#   1. Fetch env vars from MMDS
#   2. Export them
#   3. exec the command (replacing itself)
exec /usr/bin/aether-env node handler.js
```

### Why Each Step Matters

**1. Mount /proc**
```bash
mount -t proc proc /proc
```
Required for:
- `/proc/cmdline` - aether-env reads boot token from here
- Process information
- System info

**2. Mount /sys**
```bash
mount -t sysfs sysfs /sys
```
Required for:
- Device information
- Kernel parameters
- (Often optional for simple functions)

**3. Mount /dev**
```bash
mount -t devtmpfs devtmpfs /dev 2>/dev/null
```
Required for:
- Block devices (`/dev/vdb` for code drive)
- TTY devices
- `2>/dev/null` because it might already be mounted

**4. Mount Code Drive**
```bash
mkdir -p /code
mount /dev/vdb /code 2>/dev/null
```
- `/dev/vdb` is the second drive (code.ext4)
- `/dev/vda` is the first drive (rootfs)
- The code image contains `handler.js` or similar

**5. Set PATH**
```bash
export PATH=/usr/local/bin:/usr/bin:/bin
```
Different Docker images put binaries in different places:
- Alpine: `/usr/bin/node`
- Node official image: `/usr/local/bin/node`
- Setting PATH avoids hardcoding paths

**6. Exec aether-env**
```bash
exec /usr/bin/aether-env node handler.js
```
- `exec` replaces the shell with aether-env
- aether-env then `exec`s node
- This chain means node becomes PID 1
- Important: PID 1 must handle signals properly

### Why PID 1 Matters

In Linux, PID 1 is special:
- If PID 1 exits, kernel panics
- PID 1 should reap zombie processes
- Signals work differently

That's why we use `exec` - the final process (node) becomes PID 1 and handles everything.

### Drive Layout in Firecracker

```
┌─────────────────────────────────────────┐
│ Firecracker VM                          │
│                                         │
│ /dev/vda (rootfs) ─────────────────────┐│
│   Contains:                            ││
│   ├── /init (this script)              ││
│   ├── /usr/bin/aether-env              ││
│   ├── /usr/local/bin/node              ││
│   └── (rest of OS)                     ││
│                                         │
│ /dev/vdb (code drive) ─────────────────┐│
│   Contains:                            ││
│   ├── handler.js                       ││
│   ├── node_modules/ (if bundled)       ││
│   └── (function code)                  ││
│                                         │
│ Mounted at runtime:                     │
│   /dev/vda → / (by kernel)             │
│   /dev/vdb → /code (by init script)    │
└─────────────────────────────────────────┘
```

### Kernel Command Line

The VM boots with these kernel args (set in `vm.go`):

```
console=ttyS0 reboot=k panic=1 pci=off init=/init ip=172.16.0.2::172.16.0.1:255.255.255.0::eth0:off aether_token=tok-xxxxx
```

| Arg | Purpose |
|-----|---------|
| `console=ttyS0` | Serial console output |
| `reboot=k` | Reboot on panic |
| `panic=1` | Wait 1 second before reboot |
| `pci=off` | Disable PCI (not needed) |
| `init=/init` | Run /init as first process |
| `ip=...` | Static IP configuration |
| `aether_token=xxx` | Boot token for MMDS validation |

### Debugging Init Problems

**Problem: Kernel panic after init exits**
```
Kernel panic - not syncing: Attempted to kill init!
```
Cause: Init script (or aether-env) exited with error.
Fix: Check VM stdout for error messages before panic.

**Problem: /dev/vdb not found**
```
mount: /dev/vdb: No such file or directory
```
Cause: Code drive not attached, or wrong device name.
Fix: Check vm.go is attaching the code drive correctly.

**Problem: node not found**
```
aether-env: command not found: node
```
Cause: Node is at different path (e.g., /usr/local/bin/node).
Fix: Set PATH in init script, or use full path.

**Problem: aether-env not found**
```
/init: /usr/bin/aether-env: not found
```
Cause: aether-env not copied to rootfs.
Fix: Copy the binary to rootfs at /usr/bin/aether-env.

### Complete Init Script with Error Handling

```bash
#!/bin/sh
set -e  # Exit on error (but see note below)

echo "=== Aether Init Starting ==="

# Mount filesystems
echo "Mounting /proc..."
mount -t proc proc /proc

echo "Mounting /sys..."
mount -t sysfs sysfs /sys

echo "Mounting /dev..."
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true

# Mount code drive
echo "Mounting code drive..."
mkdir -p /code
if mount /dev/vdb /code 2>/dev/null; then
    echo "Code drive mounted at /code"
else
    echo "WARNING: No code drive found"
fi

# List code directory for debugging
echo "Contents of /code:"
ls -la /code

# Set PATH
export PATH=/usr/local/bin:/usr/bin:/bin

# Check if handler exists
cd /code
if [ ! -f handler.js ]; then
    echo "ERROR: handler.js not found in /code"
    echo "Available files:"
    ls -la
    # Sleep so we can debug
    exec sleep infinity
fi

# Run function
echo "Starting function..."
exec /usr/bin/aether-env node handler.js
```

**Note on `set -e`:** Be careful with `set -e` in init scripts. If any command fails, the script exits, and kernel panics. For debugging, you might want to remove it and add explicit error handling.

### Preparing the Rootfs

To add init and aether-env to a rootfs:

```bash
# Mount the rootfs
sudo mount -o loop node-rootfs.ext4 /mnt

# Copy aether-env binary
sudo cp init/aether-env /mnt/usr/bin/aether-env
sudo chmod +x /mnt/usr/bin/aether-env

# Create init script
sudo tee /mnt/init > /dev/null << 'EOF'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null
mkdir -p /code
mount /dev/vdb /code 2>/dev/null
cd /code
export PATH=/usr/local/bin:/usr/bin:/bin
exec /usr/bin/aether-env node handler.js
EOF

sudo chmod +x /mnt/init

# Unmount
sudo umount /mnt
```

---

## Telemetry & Observability

Aether uses OpenTelemetry to collect distributed traces and VM logs.

### Infrastructure

All observability services run via Docker Compose (`deployment/compose.yml`):

| Service | Port | Purpose |
|---------|------|---------|
| OTel Collector | 4318 | Receives OTLP from Gateway/Worker |
| Tempo | 3200 | Trace storage |
| Loki | 3100 | Log storage |
| Grafana | 3000 | Visualization UI |

### Traces

Gateway and Worker emit spans for key operations:

| Service | Span | When |
|---------|------|------|
| Gateway | `function.invoke` | Every function request |
| Gateway | `function.coldstart` | When triggering cold start |
| Worker | `job.process` | Processing cold start job |
| Worker | `instance.spawn` | Spawning a VM |

Spans include attributes like `function.id`, `instance.id`, `invocation.id`.

### VM Log Capture

The `VMLogWriter` in `shared/telemetry/telemetry.go` captures VM stdout/stderr:

1. Worker creates `VMLogWriter` for each instance
2. Passed to Firecracker as `Stdout`/`Stderr` writers
3. Lines are parsed and sent to OTel as log records
4. Labels: `function.id`, `instance.id`, `stream` (stdout/stderr)

Logs still print to console for debugging.

### Configuration

Set in `.env` for both Gateway and Worker:

```
OTLP_ENDPOINT=localhost:4318
```

Omit or leave empty to disable telemetry.

### Viewing in Grafana

**Traces:** Explore → Tempo → Search by `service.name`

**Logs:** Explore → Loki with queries like:
- `{job="aether-worker"} | json | line_format "{{.body}}"`
- Exclude kernel logs: `| body !~ "^\\[\\s+[0-9]+\\.[0-9]+\\].*"`

---

## Future Improvements

1. **Request Timeouts** - Prevent hanging requests
2. **Health Checks** - Periodic liveness probes
3. **Multi-Worker Load Balancing** - Distribute cold starts evenly
4. **Authentication** - API keys, JWT
5. **Custom Runtimes** - Python, Go, Rust
