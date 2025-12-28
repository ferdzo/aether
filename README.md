# Aether - Function as a Service Platform

A custom FaaS platform built with Go and Firecracker microVMs for learning distributed systems and virtualization.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         AETHER FaaS                             │
│                                                                 │
│   ┌──────────┐      ┌───────┐      ┌────────┐                   │
│   │ Gateway  │────▶│ Redis │────▶│ Worker │                   │
│   │  (API)   │      │ Queue │      │ Agent  │                   │
│   └────┬─────┘      └───────┘      └───┬────┘                   │
│        │                               │                        │
│        │           ┌──────┐            │       ┌──────────────┐ │
│        └─────────▶│ etcd │◀──────────┴─────▶│  Firecracker │ │
│                    │      │                    │     VMs      │ │
│                    └──────┘                    └──────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```


## License

MIT
