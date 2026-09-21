# Aether — Design Document (Superseded)

> **This document is superseded. Do not use it to describe the current system.**
>
> - **Design direction:** [`ARCHITECTURE.md`](ARCHITECTURE.md)
> - **Current implementation state:** [`CONTEXT.md`](CONTEXT.md)
>
> It is kept only so existing links do not rot.

## Still-true high-level intent

Aether runs isolated workloads in per-workload Firecracker microVMs, driven by a
two-process split: a **gateway** (function CRUD, discovery, invocation routing,
cold-start triggering) and a **worker** (VM lifecycle, provisioning, networking,
and a per-instance HTTP proxy for functions). Coordination uses etcd for
discovery/leases and Redis for provisioning signals. The core isolation goal —
one microVM per workload, not containers — is unchanged.

## Stale claims (do not cite)

Everything below appeared in earlier revisions of this file and is no longer
true, or is no longer the whole story:

- **Queue is a Redis list (`queue:vm_provision`, `LPUSH`/`BLPOP`).** The code uses
  a Redis Stream, `stream:vm_provision`, read by consumer group `aether-workers`
  via `XReadGroup`; success is `XACK`ed and stale pending entries are reclaimed
  with `XPendingExt`/`XClaim` (`worker/internal/worker.go`,
  `shared/protocol/messages.go`).
- **MinIO-specific setup.** Object storage is a generic S3-compatible `minio-go`
  client (`shared/storage/minio.go`); local dev provisions `ghcr.io/ferdzo/fs`
  (compose service `fs`, port 2600), not MinIO.
- **"Environment variables are not passed to VMs (needs MMDS)."** MMDS delivery
  exists: the worker builds the payload (`buildMMDSData`, `buildJobMMDSData`) and
  the guest `aether-env` fetches `169.254.169.254`, failing closed when a boot
  token is present but metadata cannot be fetched (`init/main.go`).
- **"No Prometheus/observability endpoints."** A worker `/metrics` endpoint
  (`:9090`), Prometheus scrape config, and OTel traces/logs exist
  (`shared/metrics`, `shared/telemetry`, `deployment/`).
- **"Invocation logging: table exists but nothing writes to it."** Invocations are
  written by `gateway/internal/router.go` (`recordInvocation` →
  `db.CreateInvocation`) on both success and error.
- **The hardcoded scaling config block.** Still hardcoded, but the values live in
  `worker/main.go` with `ScalingConfig` in `worker/internal/config.go`, and the
  struct has since gained `Enabled` and `WarmWindow` (recently invoked functions
  do not drop below `MinInstances`).
- **Every workload is an HTTP server.** Process jobs are first-class:
  `protocol.Job.Mode == "process"` dispatches to `worker/internal/job.go`, and the
  guest supervisor in `init/main.go` runs a command, emits
  `AETHER_EXIT:<nonce>:<code>`, and resets the VM — no port, proxy, or HTTP
  readiness. `NET_MODE=none` runs these jobs with no NIC at all.
