# Aether - Project Context

> LLM-readable summary of the current implementation state.
>
> Authoritative companions: [`ARCHITECTURE.md`](ARCHITECTURE.md) (design
> direction and migration plan) and [`../README.md`](../README.md) (how to run
> it). `DESIGN.md` is superseded and `DEVELOPER_NOTES.md` is historical.

## What This Is

Aether is a **Firecracker microVM execution platform**. It began as a
Function-as-a-Service platform and now supports two workload shapes that share
the same machinery:

- **Serverless functions** — an HTTP server in a microVM, cold-started on
  demand, reverse-proxied, autoscaled, scaled to zero.
- **Process jobs** — boot a microVM, run a command, stream its output, capture
  its exit status, destroy the VM. This is the shape a coding/SRE agent (for
  example `opencode run`) needs.

The design rule is that nothing below the workload layer knows what a function
is; see `ARCHITECTURE.md`.

## Verification Status

Only claims backed by an actual execution are listed as verified. Everything
else is marked unverified on purpose.

**Verified by running it:**

| Capability | Evidence | Root? |
|---|---|---|
| Process job, full path | Real Redis Stream entry → real worker → real microVM running `sh -c 'echo hello; sleep 2; exit 42'` → stdout captured → exit status 42 → durable job record `state=done` → stream ACKed at spawn (`XPENDING 0`). `scripts/e2e-job.sh` | no |
| Guest networking (bridge) | From inside the guest: `github.com` resolved to an IP and an outbound HTTPS request returned 200. `scripts/test-guest-egress.sh` | yes |
| netns addressing | Structural test: guest gateway on the in-namespace bridge, TAP attached, host route for the guest /30, `ip_forward=1` in the namespace | yes |
| Firecracker + kernel | Real boots on Firecracker **v1.17.0**, guest kernel **6.18.48** | — |
| Ordered drives | Real VM with the `rootfs` device plus an extra read-only drive attached in order | no |
| MMDS bootstrap + fail-closed init | Demonstrated by the egress run: the guest loaded metadata and ran the entrypoint instead of bailing | yes |

**Not verified:**

- The full **HTTP function path** end to end since the reliability work
  (create → cold start → MMDS → readiness → proxy → scale-down). The pieces are
  implemented and covered by unit tests; the composed path has not been re-run
  recently.
- **MMDS command delivery** for process jobs that have a NIC. Offline jobs
  carry their command in the rootfs `/init` instead.
- Job cancellation.

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.24, one workspace (`go.work`) with modules `gateway`, `worker`, `shared`, `init` |
| Virtualization | Firecracker **v1.17.0** (`firecracker-go-sdk` v1.0.0 — still the newest tagged release) |
| Guest kernel | **6.18.48**, resolved from date-stamped `firecracker-ci` artifacts |
| API router | chi/v5 |
| Discovery / registry | etcd v3 |
| Provisioning | **Redis Streams** (`stream:vm_provision`, consumer group `aether-workers`) |
| Object storage | S3-compatible (`minio-go` client); local dev runs `ghcr.io/ferdzo/fs`, not a MinIO server |
| Database | SQLite (gateway): `functions`, `invocations` |
| Networking | `NET_MODE=bridge` (default), `netns`, `none` |
| Observability | OpenTelemetry traces/logs + Prometheus metrics; Loki/Tempo/Grafana/otel-collector |

## Services

### Gateway (`gateway/`)
- Listens on `:8080`.
- `POST /jobs` does **not** exist yet; jobs are submitted directly to the stream.
- Function management (`/api/functions`), code upload, invocation history, logs.
- Invocation routing at `/functions/{id}/*`: looks up instances in etcd, and on a
  miss runs the cold-start path under a `singleflight` guard.
- Connects to etcd, Redis, SQLite and object storage.

### Worker (`worker/`)
- Registers itself in etcd under `/workers/<id>` with a lease.
- Consumes the provision stream with `XReadGroup`; ACKs only on success.
- A stale-claim reaper (`XPendingExt` + `XClaim`, ~30s interval, ~70s idle)
  re-delivers entries whose worker died.
- **Function path:** resolve code and runtime image → provision network → boot →
  HTTP readiness → per-instance reverse proxy on ports 30000+ → register the
  instance in etcd → autoscale.
- **Job path:** `Mode == "process"` → no readiness, no proxy, no autoscaling;
  record the job, ACK at spawn, classify the outcome asynchronously.
- Job instances are deliberately **not** placed in the scaler's instance map and
  are not registered as function instances.

### Guest (`init/` → `aether-env`)
- Parses `aether_token=` from the kernel command line, fetches MMDS with bounded
  retries, and **fails closed** (non-zero exit) if a boot token is present but
  metadata cannot be loaded. It never falls back to a default entrypoint.
- Writes `/etc/resolv.conf` from the optional MMDS `dns` list.
- **HTTP mode:** `exec`s the entrypoint (argv override, or extension dispatch).
- **Process mode** (`mode:"process"` via MMDS, or the `--process` argv form): runs
  a supervisor that starts the command in its own process group, enforces an
  optional timeout (kill the group, report exit 124), reaps stragglers, syncs,
  prints `AETHER_EXIT:<nonce>:<code>` as the final stdout line, and resets the
  guest via `reboot(2)`.
- The sentinel exists because the workload is PID 1: a normal exit would panic
  the kernel, and Firecracker's own exit code is not the workload's.

## Protocols and Keys

**Redis**
- Stream `stream:vm_provision`, group `aether-workers`. Entry shape:
  `XADD stream:vm_provision * job '<json>'` where the JSON is `protocol.Job`.
- Pub/sub `channel:code_update` — function code changed; workers invalidate the
  code cache and stop that function's instances.
- `job:req:<requestID>` — short-TTL in-flight marker used for job idempotency.

**etcd**
- `/workers/<workerID>` — worker registration (lease).
- `/functions/<functionID>/instances/<instanceID>` — function instance registry.
- `/jobs/<jobID>` — durable job records (`protocol.JobRecord`), no lease.

**Object storage**
- `function-code/<functionID>/code.ext4` — uploaded, built code image.
- `runtimes/<runtime>/rootfs.ext4` — runtime image published by
  `scripts/build-runtime.sh`.

**MMDS payload**
- Common: `token`, `env`, `dns` (and `entrypoint`, `port` for functions).
- Process mode adds: `mode`, `command`, `timeout_s`, `exit_nonce`.
- Offline (`NET_MODE=none`) jobs send neither a token nor MMDS, because MMDS is
  only reachable over a NIC.

## What Works

### Function management and invocation
CRUD, code upload (zip/tar.gz → ext4), `GET/POST /functions/{id}/*` invocation,
invocation history, and a logs endpoint backed by Loki. Cold start:
gateway finds no instances → validates the function and its code → publishes a
provision job (singleflight per function) → worker boots and registers an
instance → gateway watches etcd and proxies the request to the worker's
per-instance proxy port, which forwards to the guest.

### Autoscaling
The worker's scaler ticks every second: scale up when average concurrency
exceeds 3 and the instance count is below 10; scale down an instance idle for
more than 30s while keeping a minimum of 1; scale to zero after 5 minutes of
total idleness, with a 10-minute warm window that holds recently invoked
functions at the minimum. Jobs are not scaled.

### Process jobs
Dispatch, idempotency (durable job record plus a Redis in-flight marker),
durable outcome recording, and ACK at spawn. See "Verification Status" for the
real end-to-end evidence.

### Networking
- `bridge` (default): one bridge, a TAP per VM, static guest IP from the kernel
  command line, idempotent NAT rules for egress.
- `netns`: a named namespace per instance with an in-namespace bridge holding the
  guest gateway, a TAP attached to it, and a separate transport /30 for the
  host↔namespace veth.
- `none`: no NIC at all. Used for offline jobs and for unprivileged testing;
  MMDS is unavailable in this mode.
- Guests receive resolvers via the MMDS `dns` list.

### Storage and image caches
Code and runtime images are cached on the worker and pulled from object storage
on first use. Both caches stream to a temporary file and publish by atomic
rename, with a per-key lock so a key is fetched once under concurrency.

## What Doesn't Work / Not Implemented

| Feature | Status |
|---------|--------|
| **Jobs HTTP API** | Not implemented; jobs are submitted directly to the provision stream |
| **Request timeouts** | No timeout on function invocations |
| **Health checks** | No periodic instance health monitoring |
| **Authentication** | Optional bearer token on the management API (`AUTH_TOKEN`); the invocation route and worker proxy ports are unauthenticated |
| **Job cancellation** | No way to cancel a running job |
| **Workspace volumes** | No writable/persistent volume support for jobs yet |

## Known Gaps / Deferred

- A `JobRecord` has no heartbeat or expiry, so a worker dying mid-job leaves the
  record `running` until a reconciler exists.
- Offline jobs rely on the command baked into the rootfs `/init`; delivering it
  properly needs a config drive (deferred).
- vsock is not implemented, although the kernel supports it. It is the eventual
  route to interactive sessions and trustworthy structured status.
- The stream has no `XTRIM`/DLQ; a malformed payload redelivers indefinitely.
- Multi-tenancy (jailer, cgroups, CPU templates, admission control) and API auth
  are out of scope for now.
- Orphaned VMs/TAPs/netns from a killed worker are avoided by collision-safe
  naming but not actively reconciled.

## Configuration

Worker (see `worker/.env.sample`): `WORKER_ID`, `WORKER_IP`, `REDIS_ADDR`,
`ETCD_ENDPOINTS`, `FIRECRACKER_BIN`, `KERNEL_PATH`, `RUNTIME_PATH`, `SOCKET_DIR`,
`CODE_CACHE_DIR`, `RUNTIMES_CACHE_DIR`, `NET_MODE`, `BRIDGE_NAME`, `BRIDGE_CIDR`,
`NETNS_SUPERNET`, `GUEST_DNS`, `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY`,
`MINIO_SECRET_KEY`, `MINIO_BUCKET`, `OTLP_ENDPOINT`.

Gateway: `ETCD_ENDPOINTS`, `REDIS_ADDR`, `PORT`, `DB_PATH`, `MINIO_*`,
`LOKI_URL`, `AUTH_TOKEN`.

## Adding New Runtimes

`scripts/build-runtime.sh <docker-image> <name>` exports an image, installs
`aether-env`, writes an `/init` that mounts the code drive at `/code` and execs
`aether-env`, builds the ext4, and publishes it as
`runtimes/<name>/rootfs.ext4`. A function then selects it with its `runtime`
field; the worker resolves and caches it on first use.
