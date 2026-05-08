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

## End-user setup

The Astery desktop helper runs ffmpeg jobs locally so cloud minutes stay cheap.

1. **Download** the build for your OS from the Releases page (https://github.com/AsteryVN/engine-tools/releases/latest).
2. **Unzip** the archive to a permanent folder — `~/Applications/astery-engine-tools` on macOS/Linux, `C:\Program Files\Astery\engine-tools` on Windows.
3. **First-launch unsigned-binary bypass:**
   - **macOS:** Right-click `astery-engine-tools` → Open → confirm. Or `xattr -d com.apple.quarantine ./astery-engine-tools`.
   - **Windows:** "Windows protected your PC" → More info → Run anyway.
   - **Linux:** `chmod +x astery-engine-tools`.
4. **Pair the desktop:** in Astery Engine web app, open Settings → Desktops → Pair new device. Copy the 6-digit code.
5. **Start the helper:** open a terminal in the unzipped folder and run `./astery-engine-tools`. Paste the pair code when prompted.
6. **Verify online:** the Desktops page should now show your machine with a green "Online" pill within 60 seconds.
7. **Leave it running.** The helper polls for jobs every 5 seconds and handles ffmpeg automatically — no further action needed.

### Troubleshooting

- **"Offline" pill won't go green** — check the helper terminal for auth errors; re-pair if the token expired.
- **ffmpeg auto-install fails (Linux)** — install via your package manager: `apt install ffmpeg` / `dnf install ffmpeg`.
- **Slow uploads** — set `ASTERY_TMPDIR` to an SSD path; the helper writes intermediate audio there.
- **Behind a corporate proxy** — set `HTTPS_PROXY=http://...` before launching.

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
