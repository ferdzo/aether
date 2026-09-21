# Aether — Minimal Architecture for Functions + Process Jobs

Status: design proposal (no behavior changes implemented)

Scope: evolve Aether from a Firecracker FaaS into a Firecracker execution platform that runs **serverless functions** and **isolated process jobs** (coding/SRE agents such as OpenCode). Nothing else is in scope.

This document supersedes the earlier, over-generalized proposal (public `Machine` abstraction, `controlplane/`, `controllers/`, `SERVICE/JOB/SESSION`, generic scheduler, volume manager). Those ideas are explicitly deferred in §14, each with the trigger that would revive it.

Decisions carried in:

- **Single trusted operator.** Multi-tenancy (jailer/cgroups/quota) is out of scope.
- **Jobs are not durable inside Aether.** If a worker dies mid-job, the job is lost. Durability comes from the retained workspace; restart is orchestrated externally (Minion/CI), not by Aether.
- **The goal is that Aether still looks and feels like Aether.** Function behaviour does not change.

---

## 1. Current architecture, and where the FaaS assumptions actually are

Current control flow (all verified against source):

```
POST /api/functions            gateway/functions/api.go     -> SQLite `functions`, code.ext4 -> object store
GET  /functions/{fnID}/...     gateway/internal/router.go   -> etcd lookup -> random instance -> HTTP proxy
                                              | (no instance) -> XADD protocol.Job -> Redis Stream
Redis Stream stream:vm_provision -> worker/internal/worker.go watchQueue/handleJob
   -> SpawnInstance: EnsureCode(fnID) -> runtime resolution -> Instance.Start -> WaitReady(HTTP)
   -> StartProxy -> RegisterInstance(functionID, proxyPort) in etcd
gateway -> worker proxyPort -> guest :3000
```

The FaaS assumptions are **concentrated, not pervasive**. They live in exactly these places:

| Where | Assumption | Generic? |
|---|---|---|
| `worker/internal/worker.go` `handleJob`/`SpawnInstance` (239-432) | the unit of work is a function; must serve HTTP; `EnsureCode(functionID)`; defaults port 3000 / entrypoint `handler.js` | function-specific |
| `worker/internal/instance.go` `WaitReady` (233-252) | readiness = HTTP GET `<500` | function-specific |
| `worker/internal/instance.go` `StartProxy` (254-284) | every instance gets an HTTP reverse proxy | function-specific, but the listener plumbing is generic |
| `worker/internal/scaler.go` | autoscale/scale-to-zero driven by HTTP request counters | function-specific |
| `worker/internal/code_cache.go`, `runtime_cache.go` | artifacts keyed by functionID / runtime name | function-specific naming, generic cache |
| `shared/protocol/messages.go` `Job`, `FunctionInstance`, `ChannelCodeUpdate`, `EtcdFuncPrefix` | wire + etcd layout modeled on functions | mostly generic, named around functions |
| `shared/db/db.go` + migrations, `gateway/functions/*`, `shared/metrics`, `shared/telemetry`, `gateway/functions/loki.go` | function CRUD, `invocations`, `function_id` labels | function-specific |
| `init/main.go` | guest must be an HTTP function; `entrypoint`+`port`; one-shot `syscall.Exec` | function-specific |

**Genuinely generic already** (do not restructure): `shared/vm/vm.go`, `shared/network/*`, `shared/storage/minio.go`, `shared/system`, `shared/logger`, `shared/builder`, `worker/internal/redis.go`, `gateway/internal/etcd.go`.

Two facts that shape everything below:

1. **`vm.Config.Stdout/Stderr` are already `io.Writer`** (`shared/vm/vm.go:40-41`), wired by `Instance.Start` to `telemetry.VMLogWriter` (`instance.go:186-187`). With `console=ttyS0`, Firecracker forwards guest serial output to the VMM process stdout, which lands in those writers. **Process stdout/stderr capture is therefore already ~free.** (Caveats in §6.)
2. **`worker/internal/instance.go` is not actually HTTP-specific in its structure.** `Instance.Start` does network + `vmMgr.Launch` + monitor + logs. HTTP-only behaviour (`WaitReady`, `StartProxy`) is invoked by the *caller* (`SpawnInstance`), not baked into `Instance`. The seam for a second execution mode already exists.

---

## 2. Re-evaluation of the previous proposal

| Proposed abstraction | Concrete problem it solves now | Required for functions + jobs? | Simpler alternative | Cost of postponing |
|---|---|---|---|---|
| Public `Machine` interface / `MachineSpec` | testability, second VMM backend | **No** | `vm.Manager`+`vm.VM` already is the machine layer; extend `vm.Config` | none until a second backend/snapshot is needed |
| `MachineInstance`/`FunctionInstance`/`AgentInstance` hierarchy | separate per-kind state | **No** | keep one `Instance`; a `JobRunner` *composes* it | none |
| Generic workload controllers | isolate policy from infra | **No** | function logic stays in `worker`+`gateway`; job logic added alongside in `worker/internal/job.go` | none |
| `SERVICE/JOB/SESSION` framework | uniform lifecycle vocabulary | **No** | `protocol.Job.Mode ∈ {http, process}` | none until interactive attach |
| Generic scheduler | worker choice | **No** | existing Redis Streams consumer group | none |
| Placement/reservation | capacity, affinity | **No** | none | none until durable jobs |
| Generic volume manager | volumes outliving instances | **No** | worker-local per-job dir + retention TTL | none until workspace reuse/affinity |
| Persistent session semantics | survive worker death | **No** | retain workspace; re-run externally | none (accepted v1 limitation) |
| `controlplane/` package | control/data split | **No** | not needed | none |
| `WorkloadKind` everywhere | discriminator | **No** | one `Mode` field on the provision message only | none |
| `FunctionID`→`WorkloadID` rename | terminology | **No** | keep `FunctionID` in function code, `JobID` in job code | none — pure churn |

Answers to the four questions, applied to every row above:

1. **What problem does it solve?** Only testability/backend-swap/fleet-scale problems that do not exist at this scope.
2. **Required?** No. Neither the function path nor the agent job needs it.
3. **Simpler?** Yes — in each case an existing type or a local function already carries the requirement.
4. **What is lost by postponing?** Nothing on this path. Each has a named trigger in §14.

**Verdict on `Execution` (the core question).** The useful common abstraction *is* execution, and it is **already present in the code in three layers**:

- wire: `protocol.Job`
- runtime: `InstanceConfig` (`instance.go:59-69`) — image, vcpu, mem, port, MMDS
- machine: `vm.Config` (`vm.go:26-42`)

A function and a job both need: an image, resources, environment, drives, a command, and a way to learn it is alive/finished. The difference is interaction, not infrastructure. Therefore: **do not introduce a new `ExecutionSpec` type** — extend `InstanceConfig`/`protocol.Job` with the two fields jobs need (`Mode`, `Command`, `Drives`) and let the worker dispatch on mode. Adding a fourth type would be churn without a requirement.

**Is `http|process` sufficient?** For `sh -c 'echo hello; sleep 2; exit 42'` and for a 30–60 min `opencode run`, yes. The concrete requirement that would force a third mode is **interactive/TTY attach** (stdin, SIGINT-to-workload, live duplex) — e.g. driving the OpenCode TUI. That is not needed now and is deferred (§14).

**Where does the mode switch live?** In the **worker dispatch**, not in `Instance` and not in `InstanceConfig`:

```
processMessage -> handleJob(job)
     if job.Mode == "process" -> Worker.startJob(job)   // new worker/internal/job.go
     else                     -> Worker.SpawnInstance(...) (existing HTTP path)
```

`Instance` gains **nothing** about modes. The "expected completion vs crash" question is inherently post-hoc and belongs to `JobRunner` (§6).

---

## 3. Minimal target architecture

```
Function (SQLite + gateway/functions)          Job (POST /jobs)
        │                                              │
        │ FunctionMetadata                             │ JobSpec
        ▼                                              ▼
   protocol.Job (Mode=http)                      protocol.Job (Mode=process)
        └──────────────────┬───────────────────────────┘
                           ▼
              Redis Stream stream:vm_provision
                           ▼
        worker/internal/worker.go  dispatch on Mode
             ├── SpawnInstance  (HTTP: WaitReady + StartProxy + RegisterInstance)
             └── startJob       (PROCESS: JobRunner; async; no proxy; no etcd instance key)
                           ▼
                 worker/internal/instance.go  (unchanged responsibilities)
                           ▼
                     shared/vm  ->  Firecracker
```

No new top-level packages. `shared/`, `worker/`, `gateway/`, `init/` stay.

---

## 4. HTTP / function execution flow (unchanged)

```
gateway/router.go coldStart -> XADD Job{Mode:"http", FunctionID, Runtime, Entrypoint, Port, ...}
worker handleJob -> SpawnInstance(functionID)
    EnsureCode(functionID)                         [unchanged]
    runtimeCache.Ensure(fnCfg.Runtime)             [unchanged]
    Instance.Start(cfg)                            [unchanged]
    WaitReady(functionPort, 30s)                   [unchanged]
    StartProxy(proxyPort, functionPort)            [unchanged]
    instances[functionID] append; RegisterInstance [unchanged]
scaler.go                                          [unchanged]
```

Only one change is allowed to touch this path: `mmdsData["port"]` must send `functionPort` (currently sends `fnCfg.Port`, which is 0 when unset — `worker.go:369`; harmless today only because the guest ignores `Port`).

---

## 5. Process / job execution flow (new)

```
POST /jobs {image, command, env, vcpu, mem, timeout_s, workspace_mb}
   -> gateway XADD Job{Mode:"process", JobID, ...}
   -> worker handleJob: ACK IMMEDIATELY after spawn, dispatch async
        startJob(job):
          1. job record written to etcd (/jobs/{id}, state=provisioning)   [idempotency marker]
          2. resolve/pull runtime image (content)
          3. create per-job writable workspace ext4  (seed if requested)
          4. Instance.Start(cfg)  with Drives=[rootfs(ro), workspace(rw)]
          5. write MMDS {token, mode:"process", command, env, exit_nonce, timeout_s}
          6. launch goroutine JobRunner (NOT in w.instances)
          7. ACK stream entry  -> provisioning complete
        JobRunner:
          - sink = bounded, non-blocking job log writer (rolling tail)
          - scan sink for `AETHER_EXIT:<nonce>:<code>` (rolling buffer, at VM exit)
          - host deadline as backstop -> Instance.Stop()
          - on VM exit: sentinel seen -> state=done, exit_code=N
                        no sentinel     -> state=failed (crash)
                        deadline         -> state=timeout
          - Instance.Stop(); keep workspace for retention TTL; record result in etcd
```

`GET /jobs/{id}` reads the etcd record. No proxy port, no `RegisterInstance`, no scaler involvement.

**Why ACK-at-spawn is mandatory, not an optimization.** The consumer is strictly serial (`watchQueue` → `processMessage` → `handleJob`, `worker.go:114-231`). If `handleJob` awaited job completion, one 60-minute job would block all provisioning on that worker, and if the entry were left pending, the 30s/70s reaper (`worker.go:148-211`) would re-run the whole job on a second worker. Invariant to encode in a comment: **provisioning completion = ACK; execution outcome is not an ACK condition.** Corollary: a job that exits non-zero, times out, or crashes is **still ACKed**; otherwise the reaper re-runs a failed 60-minute job.

### 5.1 Exit-status channel (v1): console sentinel, quarantined

Guest stdout is already captured (§1). Exit status is obtained by making PID 1 a small supervisor instead of `syscall.Exec`: fork the command, `wait`, kill the process group, `sync`/`tcdrain`, print `AETHER_EXIT:<nonce>:<code>` to the console, then **`reboot -f`**.

Verified constraints (from code + Firecracker behaviour):

- **`poweroff` does not terminate Firecracker on x86** (no ACPI PM). The supervisor must trigger a guest reset — `syscall.Reboot(syscall.RB_AUTOBOOT)` / `reboot -f` — so that `reboot=k` + i8042 makes the VMM exit.
- **The serial stream is chunked, not line-oriented**, and carries kernel `printk` interleaved with workload output. The sentinel scanner must scan a rolling buffer across `Write` calls (mirror the logic in `telemetry.VMLogWriter.Write`, `telemetry.go:131-149`, but do not reuse its line-lossy behaviour).
- **Backpressure is real.** `exec.Wait` drains stdout copy goroutines before returning, so a blocking sink stalls VM-death detection. Job sinks must be bounded and non-blocking (write raw output to a per-job file + keep a bounded tail).
- **Scanning at VM exit is race-free**: because `exec.Wait` drains the copy goroutines, all serial bytes have been delivered to the writer by the time `Instance.monitorVM`'s `vm.Wait()` returns.
- **The nonce is not secret** (delivered via MMDS, also in `/proc/cmdline`). A workload can forge its own exit code. That is acceptable for self-reported status of a job the tenant owns; it must not be reused for anything security-bearing.

Two hard rules, so this stays cheap to replace with vsock later:

1. **The console carries exit status and human logs only.** Never parse structured output or artifacts from it — artifacts go through the workspace drive.
2. **Classify after the fact, never pre-set a flag.** Do not add `Instance.ExitExpected`. A VM exit *without* the sentinel is a crash, decided by `JobRunner`.

---

## 6. Exact existing files/types to modify

| File | Change |
|---|---|
| `shared/vm/vm.go` | replace `Config.CodeDrivePath` with `Config.Drives []DriveSpec{Path, IsReadOnly}`; keep rootfs/device order stable; fix `Shutdown()` (cancel *after*, or delete it — only `Stop()` is used) |
| `shared/protocol/messages.go` | extend `Job` with `Mode`, `Command []string`, `TimeoutSeconds`, `WorkspaceMB`; add `JobStatus`; keep all existing fields |
| `worker/internal/instance.go` | `InstanceConfig.Drives`; pass `Drives` through to `vm.Config`; move callback assignment before `Start`; fix `Status` locking |
| `worker/internal/worker.go` | dispatch on `job.Mode`; add async `startJob`; extract shared network+launch helper out of `SpawnInstance`; ACK-at-spawn rule; propagate ctx (replace `context.Background()` at :245); keep job instances out of `w.instances`; startup GC |
| `worker/internal/job.go` *(new)* | `JobRunner` composing `*Instance`: sentinel scanner, deadline, classification, result recording |
| `worker/internal/workspace.go` *(new)* | create/seed/retain/GC per-job workspace ext4 |
| `worker/internal/etcd.go` | `PutJob`/`GetJob` (include `worker_id`, `heartbeat_at`, `state`); RequestID idempotency marker |
| `init/main.go` | process mode: MMDS `command`/`timeout_s`/`exit_nonce`; supervisor instead of `syscall.Exec`; fail-closed in job mode; sentinel + `reboot` |
| `shared/telemetry/telemetry.go` | bounded, non-blocking job log writer (raw file + tail); never one synchronous OTLP emit per line |
| `shared/network/bridge.go` | add working egress NAT for bridge mode; fix `SetupNAT` (empty bridge name, ignored errors) |
| `shared/network/instance_netns.go` | fix host→guest addressing (§10, REQUIRED) |
| `gateway/main.go`, `gateway/jobs/api.go` *(new)* | `POST /jobs`, `GET /jobs/{id}`; no proxy/discovery |
| `scripts/build-runtime.sh`, `Makefile`, `scripts/setup.sh` | a job-capable rootfs variant containing OpenCode/shell/coreutils and a process-mode `/init` |
| `worker/internal/runtime_cache.go`, `code_cache.go` | stream to temp + atomic rename; per-key singleflight (existing race) |

Additive defaults keep the function path working: `Job.Mode` empty ⇒ `http`; `Config.CodeDrivePath` can remain readable during rollout.

---

## 7. Exact existing types/files that must remain unchanged

Do not touch (beyond the specific edits in §6):

- `shared/vm/vm.go` **structure** — `Manager.Launch`, SDK usage, boot args, MMDS/`AllowMMDS` logic, MAC generation. Only the drive list and `Shutdown` change.
- `shared/network/bridge.go` **model** (bridge + TAP + IPAM) and `netns` lifecycle — only fix defects.
- `shared/storage/minio.go`, `shared/builder/builder.go`, `shared/system`, `shared/logger`, `shared/id`, `shared/metrics` (add at most one `mode` label).
- `shared/db/*` and migrations — no schema change required for v1.
- `gateway/functions/api.go`, `gateway/internal/{router,discovery,proxy,redis,etcd}.go` — unchanged.
- `worker/internal/scaler.go`, `proxy.go`, `code_cache.go`, `runtime_cache.go` (beyond the atomicity fix), `config.go` (add fields only).
- `init/main.go` **HTTP path** — `syscall.Exec` behaviour for functions stays exactly as-is; process mode is an added branch.
- All etcd key prefixes, Redis stream/group/channel names, metric names and Loki labels. **No renames.**

---

## 8. Required Firecracker-level changes

Minimal:

1. **Arbitrary drives** instead of `CodeDrivePath`: `Drives []DriveSpec{Path string, IsReadOnly bool}`, appended in order (rootfs=vda, code/workspace=vdb, …). Missing declared drive = hard error (today a missing code drive is silently skipped, `vm.go:104-113`, and the guest then panics).
2. **Per-execution stdout/stderr sink** — already possible (`Config.Stdout/Stderr`); use it for the job writer.
3. **Fix `VM.Shutdown()`** (`vm.go:196-199` cancels the ctx before using it). Either fix or delete; the job path uses `Stop()` + deadline.
4. **Read-only job rootfs** (`IsReadOnly: true` for the job path only) while the workspace drive is writable. This avoids 900 MB copies per job *and* neutralizes the shared-rw-ext4 hazard for jobs.
5. **Not required for v1: vsock.** The kernel already has `CONFIG_VSOCKETS=y`/`CONFIG_VIRTIO_VSOCKETS=y` and the SDK supports `VsockDevices`, so it can be added later without re-architecting. It becomes necessary only for interactive attach or trustworthy structured status (§14).

---

## 9. Required guest/init changes

Minimal, and HTTP behaviour is untouched:

1. New job path in `init/main.go`, selected by an MMDS `mode` field (or an `aether_mode=job` kernel arg), which:
   - fetches MMDS and **fails closed** on error in job mode (today it warns and runs `handler.js` — `init/main.go:26-36`; in job mode that would execute the wrong thing);
   - reads `command`, `env`, `timeout_s`, `exit_nonce`;
   - forks the command as a child, streams stdout/stderr to the console (inherited fds);
   - on exit: kill process group, `sync`, `tcdrain`, print `AETHER_EXIT:<nonce>:<code>`, then `reboot -f`;
   - enforces the guest-side timeout.
2. The existing `syscall.Exec` path stays for HTTP mode.
3. A **job rootfs variant** whose `/init` mounts the writable workspace (e.g. `/workspace`, `HOME=/workspace`) and the read-only artifact drive. Today's init hardcodes `/dev/vdb → /code` (`scripts/build-runtime.sh`, `scripts/setup.sh:51-60`) and `aether-env node handler.js`, so a job image needs its own init — and `/init` must call `aether-env` with **no argv** so the MMDS `command` is honoured.
4. DNS: inject `/etc/resolv.conf` (nothing in the current pipeline provides a resolver).

---

## 10. Existing bugs, classified

**EXISTING BUG — fix independently, do not turn into architecture**

| Bug | Location |
|---|---|
| `Shutdown()` cancels ctx before use; effectively dead code | `shared/vm/vm.go:196-199` |
| ACK after failed registration (failure only logged, entry ACKed) | `worker/internal/worker.go:424-426` |
| `handleJob` ignores caller ctx (`context.Background()`) so SIGTERM can't cancel provisioning | `worker/internal/worker.go:245` |
| `Instance.Status` written unlocked while read under lock (data race) | `instance.go:111,203,288,335` vs `:384-386` |
| VM-death callbacks wired *after* `Start`, so a very short job leaks net/TAP | `worker/internal/worker.go:384-392` |
| MMDS sends `port=fnCfg.Port` (0) while the instance uses 3000 | `worker/internal/worker.go:369` |
| MMDS fetch fails open to `handler.js` | `init/main.go:26-36` |
| Restart reuses orphan TAP/IP/netns names | `shared/network/bridge.go:141-148`, `instance_netns.go` |
| Runtime/code cache `io.ReadAll` + non-atomic `os.WriteFile`, no per-key lock | `runtime_cache.go:71-80`, `code_cache.go:63-74` |
| `SetupNAT` ignores errors, called with empty bridge name, non-idempotent, no cleanup | `shared/network/bridge.go:150-166`, `worker/main.go:154` |
| Bad-JSON stream payloads redeliver forever; no `XTRIM`/DLQ | `worker/internal/worker.go:224-228` |
| Shared writable rootfs across instances (corruption + cross-workload persistence) | `shared/vm/vm.go:100`, `runtime_cache.go:42` |
| `StopInstance` holds the global mutex while draining the proxy (≤5s) | `worker/internal/worker.go:489-519` |

**REQUIRED FOR JOB SUPPORT — must land for real jobs (not for the hello-world test)**

| Requirement | Why |
|---|---|
| Working egress + DNS | `opencode run`/`git clone` have **no network path today**: bridge mode never calls `SetupNAT`; netns is the only mode with NAT and its addressing is wrong (§10 netns row). |
| Async dispatch + ACK-at-spawn invariant | otherwise the serial consumer blocks 60 min and the reaper re-runs jobs |
| Deadlines/kill semantics; timeout is still ACKed | jobs need bounded runtime; a killed/timed-out job must not be replayed |
| Workspace lifecycle + retention (not delete-on-completion) | the agent's edits are the point; `GET /jobs/{id}` workspace access |
| Read-only job rootfs + writable workspace drive | avoids 900 MB copies and cross-job corruption |
| `Drives` on `vm.Config` | arbitrary block devices |
| Guest supervisor + sentinel + `reboot -f` | exit status; PID 1 exit otherwise panics |
| Job record with `worker_id`/`heartbeat_at` (even without failover) | otherwise a dead-worker job reads `running` forever, unresolvable later |
| Bounded, non-blocking job log sink | a chatty 60-min agent would otherwise stall the VM and flood OTLP |
| Job rootfs image with OpenCode + process `/init` | content, not infrastructure |
| `POST /jobs` / `GET /jobs/{id}` | submission + result |
| netns address correction | see below |

**netns correction (REQUIRED if jobs use netns):** `Setup` puts `HostIP=.1` on the root-namespace veth and `GuestIP=.2` on the peer *inside* the netns, then uses `.2` as the guest's static IP (`instance.go:136`). Inside the netns `.2` is a local address, so host→guest traffic to `.2` is consumed by the netns, not forwarded to the TAP, and the guest's ARP for the gateway gets no reply. **Netns mode cannot work as written** — my earlier review flagged this as *[unverified]*; the review of the addressing logic confirms the structural fault. Bridge mode is the safe default for jobs until this is fixed.

**FUTURE IMPROVEMENT — safe to defer**

Streams/SQLite GC, image GC/eviction, request timeouts, auth, observability rework, orphan reconciler, per-VM cgroups/jailer.

---

## 11. Minimal migration sequence

Each step is independently shippable and keeps functions working.

- **P0 — correctness prerequisites (no new features).** Fix the EXISTING BUG table. Specifically: `vm.Shutdown`, ACK-after-registration, ctx propagation, `Status` locking, callbacks-before-`Start`, cache atomicity, `SetupNAT`/bridge-mode egress + DNS, netns addressing. Include a real egress verification (fetch from inside a VM).
- **P1 — drives + workspace.** `vm.Config.Drives`; `InstanceConfig.Drives`; `worker/internal/workspace.go`; read-only job rootfs; writable workspace with retention TTL.
- **P2 — guest supervisor.** `init/main.go` process mode (`mode`/`command`/`timeout_s`/`exit_nonce`, fail-closed, fork+wait+flush+sentinel+`reboot`); job rootfs image (OpenCode/shell/coreutils + process `/init` + `resolv.conf`).
- **P3 — worker job path.** `worker/internal/job.go` (`JobRunner`), `protocol.Job` extensions, `startJob` + async dispatch + ACK-at-spawn, etcd job records + RequestID idempotency, bounded job log sink, job instances kept out of `w.instances`, startup orphan GC.
- **P4 — API.** `gateway/jobs/api.go`: `POST /jobs`, `GET /jobs/{id}` (and workspace access). No proxy, no discovery.
- **P5 — hygiene.** Delivery-count cap/DLQ, `XTRIM`, workspace TTL GC, one `mode` metric label, job Loki labels.
- **P6 — only on a trigger from §14.** vsock, scheduler/placement, volume manager, interactive attach.

---

## 12. Acceptance tests

**T1 — process execution (the primary test).** Submit `sh -c 'echo hello; sleep 2; exit 42'`.
- host receives stdout containing `hello`
- job record reports `exit_code = 42`, state `done`
- no proxy port allocated, no etcd instance key, no scaler involvement
- instance and network resources destroyed; workspace retained per policy

**T2 — no HTTP requirement.** The job image has no HTTP server; T1 still passes.

**T3 — failure classification.** `sh -c 'kill -9 $$'` (or a crash before the sentinel) ⇒ state `failed`, not `done`.

**T4 — timeout.** `sleep 600` with `timeout_s=5` ⇒ state `timeout`, VM destroyed, entry ACKed (no re-delivery).

**T5 — long job.** A 30–60 min `opencode run`-equivalent: provisioning ACKs promptly; worker continues serving other provisioning; stdout streams to the bounded sink without stalling the VM.

**T6 — egress (required for real jobs).** From inside a job VM: `git clone` a public repo and reach a package mirror; `resolv.conf` resolves.

**T7 — function regression.** Existing function still cold-starts, proxies, scales, and updates code; `GET /functions/{id}/*` behaviour byte-identical.

**T8 — no double-provision.** Kill a worker mid-provision; the entry is reclaimed once (RequestID marker) and a slow image pull does not double-spawn at 70 s.

**T9 — no orphans.** Kill the worker with jobs running; restart; no orphan TAPs/netns/sockets/job dirs beyond the retention policy.

---

## 13. Workspace policy (decide now, it is API-visible)

Because a coding agent's value is its edits, workspaces are **retained**, not deleted on completion:

- created per job under a worker-local dir (e.g. `<WORKSPACE_DIR>/<jobID>/`), sized `workspace_mb`
- retained for a TTL after completion; `GET /jobs/{id}` exposes existence + result; workspace retrieval is a P4 endpoint
- GC is a worker startup/lazy sweep keyed by job directory name
- no cross-job reuse, no affinity, no snapshots — that is the volume-manager trigger (§14)

---

## 14. Explicitly deferred (with the trigger that revives each)

| Deferred | Revive when |
|---|---|
| vsock first-class | interactive/TTY attach, stdin, dependable structured status, or health without HTTP |
| public `Machine`/`MachineSpec` interface | second VMM backend, snapshot/restore, warm-pool pause/resume |
| generic scheduler / placement / reservation | jobs must survive worker death, or heterogeneous workers / image locality / workspace affinity |
| volume manager | workspaces outlive jobs, are reused across jobs, or need snapshots |
| `SERVICE/JOB/SESSION` framework | multi-step sessions / interactive workloads |
| generic controllers / `controlplane/` | fleet-wide reconciliation or failover |
| snapshot/restore | fast agent/CI start (keep identity re-injectable so it stays possible) |
| `FunctionID`→`WorkloadID` rename | never; it is churn |

The design rule that makes this safe: **jobs are jobs, functions are functions, and they share `Instance` + `vm` + network + storage.** No layer learns a new vocabulary until a real requirement forces it.
