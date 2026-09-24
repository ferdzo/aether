# Aether

Firecracker microVM runtime. Three workload shapes:

- **Functions** — an HTTP server per microVM, cold-started on demand, reverse-proxied, autoscaled, scaled to zero.
- **Jobs** — one command per microVM; the exit code is recorded and the VM is destroyed.
- **Executions** — one long-lived microVM you run many commands in, sharing a writable `/workspace`.

Aether exposes its own API; orchestration lives outside it.

## Requirements

- Linux with KVM (`/dev/kvm`)
- Docker (infrastructure and rootfs builds)
- Go 1.24
- Root for bridge/TAP/netns networking. `NET_MODE=none` needs no privileges.

## Quick start

```bash
scripts/setup.sh                        # firecracker, kernel, aether-env, node rootfs, worker/.env
cd deployment && docker compose up -d   # etcd, object storage, redis, observability
cd gateway && go build && ./gateway     # :8080
cd worker  && go build && sudo ./worker # root for bridge/TAP; NET_MODE=none to run unprivileged
```

## Executions

Create a VM, run commands in it, destroy it.

```bash
B=http://localhost:8080/api/executions

# required: runtime, timeout_seconds (lifetime), workspace_mb
curl -s -X POST $B -H 'Content-Type: application/json' \
  -d '{"id":"ex1","runtime":"exec","timeout_seconds":900,"workspace_mb":64}'

# argv only; no implicit shell
curl -s -X POST $B/ex1/exec -d '{"argv":["echo","hello"]}'
curl -s -X POST $B/ex1/exec -d '{"argv":["sh","-c","echo abc > /workspace/file"]}'
curl -s -X POST $B/ex1/exec -d '{"argv":["cat","/workspace/file"]}'   # -> abc

# stream output live instead of waiting for the result
curl -sN -X POST $B/ex1/exec \
  -d '{"argv":["sh","-c","echo one; sleep 2; echo two"],"stream":true}'

curl -s  $B/ex1/exec/<exec_id>          # record: state, pid, exit code, timings
curl -sN $B/ex1/exec/<exec_id>/events   # replay/attach to the event stream
curl -s -X POST $B/ex1/exec/<exec_id>/signal -d '{"signal":"SIGTERM"}'
curl -s -X DELETE $B/ex1
```

| Endpoint | Description |
|---|---|
| `POST /api/executions` | Create: `runtime`, `timeout_seconds`, `workspace_mb`; optional `id`, `vcpu`, `memory_mb`, `env_vars` |
| `GET /api/executions/{id}` | State, worker address, workspace path |
| `POST /api/executions/{id}/exec` | Run: `argv` required; optional `cwd`, `env`, `timeout_seconds`, `stream` |
| `GET /api/executions/{id}/exec/{exec_id}` | One exec's record |
| `GET /api/executions/{id}/exec/{exec_id}/events` | SSE `started`/`stdout`/`stderr`/`exited`, with `Last-Event-ID` resume |
| `POST /api/executions/{id}/exec/{exec_id}/signal` | `SIGTERM`/`SIGINT`/`SIGHUP`/`SIGKILL` to that exec's process group |
| `DELETE /api/executions/{id}` | Stop the VM and release its resources |

One exec runs at a time per execution; a concurrent one gets `409`. A non-zero exit is an exec result, not an execution failure. `/workspace` is a per-execution writable disk that survives across execs; the VM root filesystem is read-only.

The `runtime` must already exist in storage as `runtimes/<name>/rootfs.ext4`
(`scripts/build-runtime.sh`, or `AETHER_JOB_INIT=exec-service scripts/build-job-rootfs.sh` for an execution image).

## Jobs

One command, one VM, then destroyed.

```bash
curl -s -X POST http://localhost:8080/api/jobs -H 'Content-Type: application/json' \
  -d '{"runtime":"exec","command":["sh","-c","go test ./..."],"timeout_seconds":600,"workspace_mb":256}'

curl -s http://localhost:8080/api/jobs/<job_id>              # state, exit_code, workspace_path
curl -s -X DELETE http://localhost:8080/api/jobs/<job_id>    # cancel a running job
```

## Functions

| Endpoint | Description |
|---|---|
| `POST`, `GET /api/functions` | Create, list |
| `GET`, `PUT`, `DELETE /api/functions/{id}` | Read, update, delete |
| `POST /api/functions/{id}/code` | Upload code (zip/tar.gz, built into an ext4) |
| `GET /api/functions/{id}/invocations`, `/logs` | Invocation history, logs |
| `ANY /functions/{id}/*` | Invoke |

## Configuration

Worker (`worker/.env.sample` covers the required subset):

| Variable | Meaning |
|---|---|
| `WORKER_ID`, `WORKER_IP` | Identity; `WORKER_IP` must be reachable by the gateway |
| `REDIS_ADDR`, `ETCD_ENDPOINTS` | Infrastructure |
| `FIRECRACKER_BIN`, `KERNEL_PATH`, `RUNTIME_PATH` | Boot assets |
| `SOCKET_DIR` | Firecracker API sockets and vsock paths |
| `NET_MODE` | `bridge` (default), `netns`, or `none` for network-less VMs |
| `BRIDGE_NAME`, `BRIDGE_CIDR`, `NETNS_SUPERNET`, `GUEST_DNS` | Networking, guest resolvers |
| `WORKSPACE_DIR`, `WORKSPACE_TTL` | Workspace images and retention |
| `WORKER_CONTROL_PORT`, `WORKER_CONTROL_TOKEN` | Worker control API the gateway proxies to |
| `MINIO_ENDPOINT`, `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY` | S3-compatible storage |
| `OTLP_ENDPOINT` | Telemetry; empty disables it |

Gateway: `ETCD_ENDPOINTS`, `REDIS_ADDR`, `PORT`, `DB_PATH`, `MINIO_*`, `LOKI_URL`, `AUTH_TOKEN`.

## Verifying

```bash
scripts/e2e-exec.sh              # guest exec service, real VM
scripts/e2e-execution.sh         # executions API end to end
scripts/e2e-execution-stream.sh  # live events, records, signals
scripts/e2e-job.sh               # one-shot job
scripts/e2e-job-api.sh           # job over HTTP
scripts/e2e-job-cancel.sh        # job cancellation
sudo scripts/e2e-execution-network.sh  # DNS, HTTPS, git clone inside an execution
sudo scripts/test-guest-egress.sh      # guest networking, function path
```

## Documentation

- `documentation/ARCHITECTURE.md` — design direction and migration plan
- `documentation/CONTEXT.md` — current implementation state

## License

MIT
