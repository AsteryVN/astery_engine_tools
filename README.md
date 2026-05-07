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

## Desktop shell (v0.2.x)

The optional Tauri 2 shell lives at [`tauri-app/`](tauri-app/README.md). Two
runtime topologies are supported:

- **Tauri spawns daemon** (default consumer install): `cargo tauri build`
  bundles the daemon as a sidecar; the shell starts it on launch and kills
  it on `WindowEvent::CloseRequested`.
- **Daemon spawns shell** (`engine-toold --with-ui`): for service-mode
  deploys where the daemon is the long-lived parent. Daemon `exec`s the
  side-by-side `astery-engine-tools-ui` binary; either side detects
  pre-existing instances via the IPC port file.

```bash
make tauri-dev    # local dev: daemon (make run) + Tauri window
make tauri-build  # platform installer (.dmg / .msi / .AppImage)
make tauri-test   # pnpm typecheck + vitest
```

v0.2.x installers are **unsigned** — macOS Gatekeeper and Windows SmartScreen
will warn on first launch. Code signing is a follow-up. See
[`tauri-app/README.md`](tauri-app/README.md#unsigned-artifact-warnings).

## Releases

Tagged releases (`v*`) auto-publish 6 daemon archives + `SHA256SUMS` + `release-manifest.json` to the GitHub release. See [`docs/RELEASE.md`](docs/RELEASE.md) for the runbook (tag flow, hotfix, rollback, signing-key rotation, arm64 fallback).

## License

MIT — see `LICENSE`.
