# Aether

Firecracker microVM runtime for serverless functions and isolated process jobs.

Aether runs two kinds of workload, each in its own microVM:

- **Functions** are HTTP servers. They are cold-started on demand, reached
  through a per-instance reverse proxy, autoscaled, and scaled to zero.
- **Jobs** are commands. The VM boots, the command runs, its stdout and exit
  status are captured, and the VM is destroyed. This is the shape a coding or
  SRE agent needs.

Design direction: [`documentation/ARCHITECTURE.md`](documentation/ARCHITECTURE.md).
Implementation state: [`documentation/CONTEXT.md`](documentation/CONTEXT.md).

## Status

Proof of concept, single operator. The management API accepts an optional
bearer token (`AUTH_TOKEN`); the invocation route and the per-instance proxy
ports are unauthenticated. There is no multi-tenancy, jailer or cgroup
isolation.

### Verified

Every entry is backed by a test that boots a real microVM. Harness names are in
the last column; see [Verifying](#verifying-it-works).

| Capability | Evidence | Root |
|---|---|---|
| Process job | A stream entry drove the worker to boot a VM running `sh -c 'echo hello; sleep 2; exit 42'`; stdout was captured, `exit_code=42` was recorded, and the stream entry was acked at spawn. `scripts/e2e-job.sh` | no |
| Job with a NIC | A bridge-mode job completed with `exit_code=42` from a rootfs containing neither the command nor the exit nonce, which requires MMDS to have supplied both. `scripts/e2e-job-bridge.sh` | yes |
| Guest networking | From inside the guest: `github.com` resolved and an outbound HTTPS request returned 200. `scripts/test-guest-egress.sh` | yes |
| netns addressing | The guest gateway sits on the in-namespace bridge, the TAP is attached to it, the host has a route for the guest /30, and forwarding is enabled in the namespace. `scripts/test-guest-egress.sh` | yes |
| Firecracker and kernel | Real boots on Firecracker v1.17.0 with guest kernel 6.18.48. | — |
| Ordered drives | A VM attached the root device plus an extra read-only drive in order. | no |

### Unverified

- The HTTP function path end to end since the reliability work: create, cold
  start, MMDS, readiness, proxy, scale-down.
- Job cancellation.

## How it works

```
                       gateway :8080
   function CRUD · /functions/{id}/* routing · cold-start singleflight · SQLite
                              │
        ┌─────────────────────┼──────────────────────┐
        ▼                     ▼                      ▼
      etcd                 Redis                 S3 storage
  /workers/            stream:vm_provision      function-code/
  /functions/{id}/     group: aether-workers    runtimes/
    instances/         channel:code_update
  /jobs/{id}
                              │
                              ▼
                            worker
   functions: code → runtime → boot → HTTP readiness → reverse proxy
              → register → autoscale
   jobs:      Mode "process" → boot, no proxy and no readiness check
              → record → ack at spawn → record the outcome asynchronously
                              │
                              ▼
                     Firecracker microVMs
                              │
                              ▼
   guest: init/ → aether-env
     fetches MMDS with bounded retries; exits non-zero if a boot token is
     present but metadata cannot be loaded
     writes /etc/resolv.conf from the MMDS dns list
     functions: execs the entrypoint
     jobs:      supervises the command, enforces the timeout (exit 124),
                prints AETHER_EXIT:<nonce>:<code> as its final line, resets
```

The workload is PID 1 in the guest, so a normal exit would panic the kernel and
Firecracker's own exit code is unrelated to the workload's. Process mode
therefore reports the exit status on the console and then performs a guest
reset; `poweroff` does not terminate Firecracker on x86.

## Quick start

```bash
# Assets: Firecracker, kernel, aether-env, node rootfs, worker/.env
scripts/setup.sh

# Infrastructure: etcd, redis, object storage, observability
cd deployment && docker compose up -d

# Gateway
cd gateway && go build && ./gateway

# Worker. sudo for bridge/TAP; NET_MODE=none for network-less jobs.
cd worker && go build && sudo ./worker

curl -X POST http://localhost:8080/api/functions \
  -H "Content-Type: application/json" \
  -d '{"name":"hello","runtime":"node"}'
zip code.zip handler.js
curl -X POST http://localhost:8080/api/functions/{id}/code -F "file=@code.zip"
curl http://localhost:8080/functions/{id}/
```

## Verifying it works

```bash
# Process job, no root. Starts its own redis and uses the compose etcd/fs.
# Asserts the job record is state=done with exit_code=42 and that the stream
# entry is acked.
scripts/e2e-job.sh

# The same job submitted over HTTP, through the gateway API.
scripts/e2e-job-api.sh

# Guest networking and MMDS delivery, root required (bridge, TAP, NAT).
sudo scripts/test-guest-egress.sh
sudo scripts/e2e-job-bridge.sh
```

Supporting scripts:

```bash
scripts/build-job-rootfs.sh [command]     # job rootfs; AETHER_JOB_INIT=mmds for no baked command
scripts/build-runtime.sh <image> <name>   # publish a runtime rootfs to storage
```

## API

| Endpoint | Description |
|---|---|
| `POST /api/functions` | Create function |
| `GET /api/functions` | List functions |
| `GET /api/functions/{id}` | Get function |
| `PUT /api/functions/{id}` | Update function |
| `DELETE /api/functions/{id}` | Delete function |
| `POST /api/functions/{id}/code` | Upload code (zip/tar.gz, built into an ext4) |
| `GET /api/functions/{id}/invocations` | Invocation history |
| `GET /api/functions/{id}/logs` | Function logs (Loki) |
| `ANY /functions/{id}/*` | Invoke function |
| `POST /api/jobs` | Submit a job: `runtime`, `command`, `timeout_seconds`, `vcpu`, `memory_mb`, `env_vars` |
| `GET /api/jobs/{id}` | Job state and exit code |

## Configuration

Worker (`worker/.env.sample` covers the required subset; `NET_MODE`, `GUEST_DNS`,
`RUNTIMES_CACHE_DIR` and `CODE_CACHE_DIR` are read by the code but are not in the
sample):

| Variable | Meaning |
|---|---|
| `WORKER_ID`, `WORKER_IP` | Identity; `WORKER_IP` must be reachable by the gateway |
| `REDIS_ADDR`, `ETCD_ENDPOINTS` | Infrastructure |
| `FIRECRACKER_BIN`, `KERNEL_PATH`, `RUNTIME_PATH` | Boot assets |
| `SOCKET_DIR` | Firecracker API sockets, created if missing |
| `NET_MODE` | `bridge` (default), `netns`, or `none` for network-less VMs |
| `BRIDGE_NAME`, `BRIDGE_CIDR`, `NETNS_SUPERNET` | Networking |
| `GUEST_DNS` | Resolvers written into the guest, default `1.1.1.1,8.8.8.8` |
| `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY`, `MINIO_BUCKET` | S3-compatible storage |
| `OTLP_ENDPOINT` | Telemetry; empty disables it |

Gateway: `ETCD_ENDPOINTS`, `REDIS_ADDR`, `PORT`, `DB_PATH`, `MINIO_*`,
`LOKI_URL`, `AUTH_TOKEN`.

## Requirements

- Linux with KVM (`/dev/kvm`)
- A Firecracker binary and a bootable guest kernel
- A runtime rootfs containing `/init` and `/usr/bin/aether-env`
- Root for bridge/TAP/netns networking. Booting and `NET_MODE=none` do not need it.

## Documentation

| File | Contents |
|---|---|
| [`documentation/ARCHITECTURE.md`](documentation/ARCHITECTURE.md) | Design direction and migration plan |
| [`documentation/CONTEXT.md`](documentation/CONTEXT.md) | Current implementation state |
| [`documentation/DESIGN.md`](documentation/DESIGN.md) | Superseded |
| [`documentation/DEVELOPER_NOTES.md`](documentation/DEVELOPER_NOTES.md) | Historical build notes |

## License

MIT License.
