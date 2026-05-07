# Astery Engine Tools

Distributed local compute runtime for the Astery platform.

`engine-toold` is a Go daemon that pairs with the Astery cloud control plane and executes pluggable workloads locally — FFmpeg video processing today, with local AI inference / GPU / captions / batch automation as future executors. An optional Tauri shell provides a desktop UI; the daemon also runs headless for server / NAS / CI deployments.

This repo is consumed as a git submodule by [`AsteryVN/astery_engine`](https://github.com/AsteryVN/astery_engine) at `engine-tools/`. Architecture lives there at `wiki/architecture/engine-tools-hybrid.md` (the canonical spec).

## Status

Pre-v0.1. Implementation in progress on the cloud branch `feature/engine-tools-mvp`.

## Design summary

```
Cloud Control Plane (astery_engine)
        │
        │ HTTPS (device session JWT, lease-based claim, polling source-of-truth + WS acceleration)
        │
        ▼
Engine Node (this repo)
   cmd/engine-toold/         primary daemon (headless capable)
   internal/runtime/         scheduler · resource manager · executor registry
   internal/tools/           FFmpeg manager (system / static download)
   internal/executors/       pluggable workload runners (clip-video first)
   internal/sync/            polling + WS + lease heartbeat
   internal/auth/            pairing + ed25519 + OS keyring
   internal/storage/         XDG/AppData/Library data dir
   internal/upload/          S3 multipart resumable
   internal/jobqueue/        local SQLite (jobs · job_events · job_attempts)
   internal/ipc/             localhost HTTP JSON for Tauri shell
   internal/observability/   wide events per Astery logging convention
   tauri-app/                optional Rust shell + minimal control panel UI
```

## Build (Linux)

```bash
make build       # → ./bin/engine-toold
make test
make run         # foreground, --headless
```

## License

MIT — see `LICENSE`.
