# Aether

Functions and isolated process jobs on Firecracker microVMs.

Aether started as a Function-as-a-Service platform and is becoming a general
microVM execution platform: a **serverless function** and a **coding/SRE agent
job** both need an isolated execution environment, and differ mainly in how the
host talks to them. Design direction is in
[`documentation/ARCHITECTURE.md`](documentation/ARCHITECTURE.md); the current
implementation state is in [`documentation/CONTEXT.md`](documentation/CONTEXT.md).

## Status

Proof of concept, single trusted operator. The management API supports an
optional bearer token (`AUTH_TOKEN`); the invocation route and the worker's
per-instance proxy ports are unauthenticated. No multi-tenancy, jailer or
cgroup isolation yet.

**Verified by actually running it:**

| Capability | Evidence | Root? |
|---|---|---|
| Process job, full path | A real Redis Stream entry drove the real worker to boot a real microVM running `sh -c 'echo hello; sleep 2; exit 42'`; stdout captured, exit status **42** recorded durably, stream entry ACKed at spawn (`XPENDING 0`). `scripts/e2e-job.sh` | no |
| Guest networking (bridge) | From **inside** the guest: resolved `github.com` and completed an outbound HTTPS request (200). `scripts/test-guest-egress.sh` | yes |
| MMDS command delivery (bridge) | A bridge-mode job whose rootfs carries **no** command and **no** nonce still completed with `exit_code=42`, so `mode`/`command`/`timeout_s`/`exit_nonce` must have arrived over MMDS. `scripts/e2e-job-bridge.sh` | yes |
| netns addressing | Structural lifecycle test (guest gateway on the in-namespace bridge, TAP attached, host route, `ip_forward=1`) | yes |
| Firecracker + kernel | Real boots on Firecracker **v1.17.0** with guest kernel **6.18.48** | — |
| Ordered drives | Real VM with `rootfs` plus an extra read-only drive attached in order | no |

**Not verified** (stated plainly rather than implied): the full HTTP function
path end to end since the recent reliability work (create → cold start → MMDS →
readiness → proxy → scale-down); job cancellation.

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
   function path: code → runtime → boot → HTTP readiness → reverse proxy
                  → register → autoscale / scale-to-zero
   job path:      dispatch on Mode:"process" → boot (no proxy, no readiness)
                  → record job → ACK at spawn → record outcome asynchronously
                              │
                              ▼
                     Firecracker microVMs
                              │
                              ▼
   guest: init/ → aether-env
     MMDS with bounded retries, fails closed if a boot token cannot be resolved
     writes /etc/resolv.conf from the MMDS dns list
     HTTP:    exec the entrypoint
     PROCESS: supervise the command, enforce the timeout (exit 124),
              print AETHER_EXIT:<nonce>:<code> as the final line, reset the guest
```

A workload is PID 1, so a normal exit would panic the kernel and Firecracker's
own exit code is not the workload's. That is why the guest reports its exit
status as a sentinel on the console and then performs a guest reset (`poweroff`
does not terminate Firecracker on x86).

## Quick start

```bash
# 1. Assets: Firecracker binary, kernel, aether-env, node rootfs, worker/.env
scripts/setup.sh

# 2. Infrastructure (etcd, redis, object storage, observability)
cd deployment && docker compose up -d

# 3. Gateway
cd gateway && go build && ./gateway

# 4. Worker — sudo for bridge/TAP networking, or NET_MODE=none for offline jobs
cd worker && go build && sudo ./worker
#   NET_MODE=none ./worker     # no root: network-less microVMs only

# 5. Create and invoke a function
curl -X POST http://localhost:8080/api/functions \
  -H "Content-Type: application/json" \
  -d '{"name":"hello","runtime":"node"}'
zip code.zip handler.js
curl -X POST http://localhost:8080/api/functions/{id}/code -F "file=@code.zip"
curl http://localhost:8080/functions/{id}/
```

## Verifying it works

Three harnesses, all of which run the real thing rather than mocks:

```bash
# Process job end to end, NO root needed. Brings up its own Redis and the
# compose etcd/fs, then asserts the job record is state=done, exit_code=42 and
# the stream entry is ACKed.
scripts/e2e-job.sh

# Guest networking, ROOT required (bridge/TAP/NAT). Builds a guest and asserts
# it can resolve github.com and make an outbound HTTPS request from inside the VM.
sudo scripts/test-guest-egress.sh

# Process job in BRIDGE mode with a real NIC, ROOT required. Proves MMDS command
# delivery: the rootfs carries no command and no nonce, so the guest can only
# run if MMDS supplied both.
sudo scripts/e2e-job-bridge.sh
```

Related:

```bash
scripts/build-job-rootfs.sh [command]        # minimal job rootfs (alpine + aether-env)
scripts/build-runtime.sh <image> <name>      # publish a runtime rootfs to storage
```

## API

| Endpoint | Description |
|---|---|
| `POST /api/functions` | Create function |
| `GET /api/functions` | List functions |
| `GET /api/functions/{id}` | Get function |
| `PUT /api/functions/{id}` | Update function |
| `DELETE /api/functions/{id}` | Delete function |
| `POST /api/functions/{id}/code` | Upload code (zip/tar.gz → ext4) |
| `GET /api/functions/{id}/invocations` | Invocation history |
| `GET /api/functions/{id}/logs` | Function logs (Loki) |
| `ANY /functions/{id}/*` | Invoke function |
| `POST /api/jobs` | Submit a process job |
| `GET /api/jobs/{id}` | Job status and result |

Jobs are submitted over HTTP: `POST /api/jobs` takes `{runtime, command[],
timeout_seconds, vcpu, memory_mb, env_vars}` and returns `202` with a job id;
`GET /api/jobs/{id}` returns the durable record (`state`, `exit_code`, ...).
`scripts/e2e-job-api.sh` drives that loop against a real microVM.

## Configuration

Worker (see `worker/.env.sample`, which ships the required subset — `NET_MODE`,
`GUEST_DNS`, `RUNTIMES_CACHE_DIR` and `CODE_CACHE_DIR` are read by the code but
are not in the sample):

| Variable | Meaning |
|---|---|
| `WORKER_ID`, `WORKER_IP` | Identity; `WORKER_IP` must be reachable by the gateway |
| `REDIS_ADDR`, `ETCD_ENDPOINTS` | Infrastructure |
| `FIRECRACKER_BIN`, `KERNEL_PATH`, `RUNTIME_PATH` | Boot assets |
| `SOCKET_DIR` | Firecracker API sockets (created if missing) |
| `NET_MODE` | `bridge` (default), `netns`, or `none` (offline, unprivileged) |
| `BRIDGE_NAME`, `BRIDGE_CIDR`, `NETNS_SUPERNET` | Networking |
| `GUEST_DNS` | Resolvers injected into guests via MMDS (default `1.1.1.1,8.8.8.8`) |
| `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY`, `MINIO_BUCKET` | S3-compatible storage |
| `OTLP_ENDPOINT` | Telemetry (empty disables) |

Gateway: `ETCD_ENDPOINTS`, `REDIS_ADDR`, `PORT`, `DB_PATH`, `MINIO_*`,
`LOKI_URL`, `AUTH_TOKEN`.

## Requirements

- Linux with KVM (`/dev/kvm`)
- Firecracker binary and a bootable guest kernel
- A runtime rootfs containing `/init` and `/usr/bin/aether-env`
- Root for bridge/TAP/netns networking; booting and offline mode work unprivileged

## Documentation

| File | Contents |
|---|---|
| [`documentation/ARCHITECTURE.md`](documentation/ARCHITECTURE.md) | Design direction and migration plan |
| [`documentation/CONTEXT.md`](documentation/CONTEXT.md) | Current implementation state |
| [`documentation/DESIGN.md`](documentation/DESIGN.md) | Superseded — signpost only |
| [`documentation/DEVELOPER_NOTES.md`](documentation/DEVELOPER_NOTES.md) | Historical build notes (see banner) |

## License

MIT License.
