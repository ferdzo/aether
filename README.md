# Aether

Serverless functions on Firecracker microVMs.

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                           GATEWAY                                │
│  • API for function management (CRUD)                            │
│  • Routes invocations to workers                                 │
│  • Triggers cold starts when no instances available              │
└──────────────────────────────────────────────────────────────────┘
        │                    │                    │
        ▼                    ▼                    ▼
   ┌─────────┐         ┌─────────┐          ┌─────────┐
   │  etcd   │         │  Redis  │          │  MinIO  │
   │(discovery)        │ (queue) │          │ (code)  │
   └─────────┘         └─────────┘          └─────────┘
        ▲                    │                    ▲
        │                    ▼                    │
┌──────────────────────────────────────────────────────────────────┐
│                           WORKER                                 │
│  • Spawns Firecracker VMs                                        │
│  • Runs reverse proxy per instance                               │
│  • Auto-scales based on load                                     │
│  • Caches function code locally                                  │
└──────────────────────────────────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────────────────────────────────┐
│                      FIRECRACKER VMs                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐                        │
│  │ fn-abc   │  │ fn-abc   │  │ fn-xyz   │                        │
│  │ :30000   │  │ :30001   │  │ :30002   │                        │
│  └──────────┘  └──────────┘  └──────────┘                        │
└──────────────────────────────────────────────────────────────────┘
```

## Quick Start

```bash
# 1. Start infrastructure
cd deployment && docker-compose up -d

# 2. Build and run gateway
cd gateway && go build && ./gateway

# 3. Build and run worker (requires sudo)
cd worker && go build && sudo ./worker

# 4. Create and invoke a function
curl -X POST http://localhost:8080/api/functions \
  -H "Content-Type: application/json" \
  -d '{"name": "hello", "runtime": "node", "env_vars": {"SECRET": "123"}}'

zip code.zip handler.js
curl -X POST http://localhost:8080/api/functions/{id}/code -F "file=@code.zip"

curl http://localhost:8080/functions/{id}/
```

## API

| Endpoint | Description |
|----------|-------------|
| `POST /api/functions` | Create function |
| `GET /api/functions` | List functions |
| `POST /api/functions/{id}/code` | Upload code |
| `ANY /functions/{id}/*` | Invoke function |

## Configuration

See `.env.example` files in `gateway/` and `worker/`.

**Critical worker settings:**
- `WORKER_IP` - Must be reachable by gateway
- `SOCKET_DIR` - Must exist (`mkdir -p /tmp/firecracker`)

## Documentation

- `CONTEXT.md` - Project overview for LLMs
- `DEVELOPER_NOTES.md` - Implementation details
- `DESIGN.md` - Architecture deep-dive

## Requirements

- Linux with KVM (`/dev/kvm`)
- Firecracker binary
- vmlinux kernel
- Prepared rootfs with `/init` and `/usr/bin/aether-env`

## License

MIT License
