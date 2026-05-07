# Astery Engine Tools — Desktop Shell

Tauri 2 + React 18 + Vite + Tailwind UI for the `engine-toold` daemon.

## Layout

```
tauri-app/
├── src/                # React renderer (Vite bundles into ../dist)
│   ├── pages/          # Pairing, Dashboard, Logs
│   ├── components/     # Hand-rolled primitives (Button, Card, Badge, Spinner)
│   └── lib/            # ipc.ts (typed client), types.ts
└── src-tauri/          # Rust shell + sidecar wiring
    ├── src/main.rs     # tauri::Builder, window event handlers
    ├── src/daemon.rs   # ipc_token, ipc_port, start_daemon_sidecar
    ├── tauri.conf.json # bundle targets, externalBin sidecar declaration
    └── binaries/       # engine-toold-<triple> staged here at release time
```

## Development

```bash
# 1. Point the daemon at your local Astery Engine backend.
#    The default in v0.2.0+ is the production cloud (engine.asteryvn.com)
#    so released AppImages work out of the box. Devs override here:
export ENGINE_CLOUD_URL=http://localhost:8080/api

# 2. Run the daemon manually (no sidecar in dev mode):
make run                 # from repo root → daemon on 127.0.0.1:<random>

# 3. In another terminal:
make tauri-dev           # opens the desktop window + Vite HMR on :1420
```

When `pnpm tauri dev` (or `make tauri-dev`) spawns the bundled sidecar,
it inherits the parent shell's environment — so `ENGINE_CLOUD_URL` set
in step 1 propagates automatically.

The renderer reads `<data-dir>/ipc.token` and `<data-dir>/ipc.port` via
`invoke('ipc_token')` / `invoke('ipc_port')` — never via the JS `fs` plugin.
This is enforced by a CI grep gate.

## Production build

`cargo tauri build` requires the daemon binary staged under
`src-tauri/binaries/engine-toold-<rust-target-triple>` (e.g.
`engine-toold-x86_64-apple-darwin`). The release-installer GitHub Actions
workflow handles staging; manual builds need the operator to copy the
correct triple in by hand.

Targets:

- macOS: `.dmg` (x86_64 + aarch64)
- Windows: `.msi`
- Linux: `.AppImage`

Artifacts land in `src-tauri/target/release/bundle/`.

## Unsigned-artifact warnings

v0.2.x ships unsigned. macOS Gatekeeper blocks first launch — right-click
the `.dmg` contents and choose **Open** to bypass. Windows SmartScreen
shows a "Windows protected your PC" warning — click **More info → Run
anyway**. Apple Developer ID + Authenticode signing land in a follow-up.

## Tests

```bash
make tauri-test          # pnpm typecheck + vitest
cd tauri-app/src-tauri && cargo test    # Rust unit tests
```
