# Release runbook — engine-toold

This document is the source of truth for cutting + publishing an `engine-toold` release. The pipeline lives at [`.github/workflows/release.yml`](../.github/workflows/release.yml).

> **Scope.** v0.1.x ships only the **headless Go daemon** as platform archives. Native installers (.dmg/.msi/.AppImage) require the Tauri shell — that lands in v0.2.x and gets a sibling `release-installer.yml`. The cloud manifest's `kind` field separates the two.

---

## TL;DR — cutting a normal release

```bash
# 1. Update the version in code if you bump it (e.g. main.version default).
git tag -a v0.1.1 -m "engine-toold v0.1.1"
git push origin v0.1.1
# 2. Watch the workflow — pushing a v* tag triggers it automatically.
gh run watch --repo AsteryVN/astery_engine_tools
```

The workflow builds 6 archives (3 OS × 2 arch), assembles `SHA256SUMS` and `release-manifest.json`, uploads to the GitHub release, marks it as latest, and runs a smoke validation. Total runtime ≈ 5–10 min on hosted runners.

---

## What gets published

For tag `v0.1.0-mvp`, the GitHub release will contain **8 assets**:

| Asset | Notes |
|---|---|
| `astery-engine-tools_v0.1.0-mvp_darwin_x64.tar.gz` | macOS x86_64 daemon |
| `astery-engine-tools_v0.1.0-mvp_darwin_arm64.tar.gz` | macOS Apple Silicon daemon |
| `astery-engine-tools_v0.1.0-mvp_linux_x64.tar.gz` | Linux amd64 daemon |
| `astery-engine-tools_v0.1.0-mvp_linux_arm64.tar.gz` | Linux arm64 daemon |
| `astery-engine-tools_v0.1.0-mvp_windows_x64.zip` | Windows amd64 daemon |
| `astery-engine-tools_v0.1.0-mvp_windows_arm64.zip` | Windows arm64 daemon |
| `SHA256SUMS` | per-archive checksums (sorted) |
| `release-manifest.json` | machine-readable summary (see schema below) |

Each archive contains `engine-toold[.exe]`, `LICENSE`, and `README.md`.

**Filename convention** — `astery-engine-tools_${version}_${os}_${arch}.${ext}`. The cloud manifest endpoint synthesizes URLs using exactly this template; **do not change one without changing the other**, or `<DownloadAppCard />` on the cloud will 404.

---

## release-manifest.json schema

```jsonc
{
  "schema_version": 1,
  "version": "v0.1.0-mvp",
  "repo": "AsteryVN/astery_engine_tools",
  "released_at": "2026-05-07T15:30:00Z",
  "artifacts": [
    {
      "name": "astery-engine-tools_v0.1.0-mvp_darwin_arm64.tar.gz",
      "os": "darwin",          // "darwin" | "windows" | "linux"
      "arch": "arm64",         // "x64" | "arm64"
      "kind": "daemon",        // "daemon" | "installer" (installer = future Tauri shell)
      "ext": "tar.gz",
      "sha256": "…",
      "size_bytes": 12345678,
      "download_url": "https://github.com/.../astery-engine-tools_v0.1.0-mvp_darwin_arm64.tar.gz",
      "build_time": "2026-05-07T15:30:00Z",
      "minimum_server_version": "0.0.0"  // cloud version required by this build
    }
    // … 5 more
  ]
}
```

A canonical example lives at [`scripts/release/fixtures/release-manifest.example.json`](../scripts/release/fixtures/release-manifest.example.json).

---

## Triggers

| Trigger | When |
|---|---|
| `push: tags: ['v*']` | Normal path — push a `vX.Y.Z[-suffix]` tag and the pipeline runs. |
| `workflow_dispatch` | Manual re-run for a tag that already exists. Useful if the first run failed mid-publish. Inputs: `version` (the tag, e.g. `v0.1.0-mvp`), `mark_latest` (default `true`). |

```bash
# Manual re-run via gh CLI:
gh workflow run release.yml \
  --repo AsteryVN/astery_engine_tools \
  -f version=v0.1.0-mvp \
  -f mark_latest=true
```

---

## Hotfix builds

For a hotfix on top of an existing release, append a `-hotfixN` suffix and push a new tag. Treat hotfixes like full releases — the workflow republishes the **full set** of 6 archives + SHA256SUMS + manifest under the hotfix tag. The cloud manifest endpoint is env-driven, so promoting a hotfix is a deploy of new env values (`ENGINE_TOOLS_LATEST_VERSION=v0.1.1-hotfix1`, etc.) on the cloud — not a code change in this repo.

```bash
git checkout v0.1.0-mvp
git checkout -b hotfix/v0.1.1-hotfix1
# … fix …
git tag -a v0.1.1-hotfix1 -m "engine-toold v0.1.1-hotfix1"
git push origin v0.1.1-hotfix1
```

---

## Rollback

A rollback removes the GitHub release + tag, then promotes the prior version on the cloud:

```bash
gh release delete v0.1.1 --repo AsteryVN/astery_engine_tools --cleanup-tag
# Cloud-side: redeploy with ENGINE_TOOLS_LATEST_VERSION=v0.1.0-mvp
```

The "latest" pointer flips automatically once the cloud env reload lands. Existing pinned downloads keep working until the GitHub release is deleted, so prefer `release edit --draft` first if you only want to hide the release while a fix is in flight.

---

## arm64 fallback strategy

Today the matrix builds **all 6 archives on a single `ubuntu-latest` runner** via Go cross-compilation (`GOOS`+`GOARCH`). This is sufficient for a pure Go daemon — no native CGO, no shell bundling.

When the Tauri shell ships, native runners become necessary because Tauri compilation is not cross-compilable in the general case. The matrix structure is intentionally **set up to swap runners per row**, not per OS. Migration plan when Tauri lands:

| Row | Today | Future (Tauri shell) |
|---|---|---|
| `darwin / x64` | ubuntu-latest | `macos-13` (Intel) or `macos-14` cross via `lipo` |
| `darwin / arm64` | ubuntu-latest | `macos-14` (Apple Silicon, GitHub-hosted) |
| `windows / x64` | ubuntu-latest | `windows-latest` |
| `windows / arm64` | ubuntu-latest | `windows-11-arm` (GitHub-hosted, beta) **or** `windows-latest` cross-build via `cargo build --target aarch64-pc-windows-msvc` |
| `linux / x64` | ubuntu-latest | `ubuntu-latest` |
| `linux / arm64` | ubuntu-latest | `ubuntu-24.04-arm` (GitHub-hosted) **or** `ubuntu-latest` with `aarch64-linux-gnu-gcc` cross-toolchain |

If GitHub doesn't yet provide a hosted arm64 runner for a given row at Tauri-shipping time, the documented fallback is a self-hosted runner labelled `self-hosted-arm64-${platform}`. Drop the label into the matrix entry — no other workflow changes required.

---

## Code-signing key rotation (placeholder)

> **Not yet implemented.** v0.1.x ships unsigned daemon binaries. Users either run via cloud-paired flow (which trusts the daemon over an authenticated session) or accept the unsigned binary at install time.

When code-signing lands (planned with the Tauri installer release):

- Apple notarization keys live in repo secrets `APPLE_TEAM_ID`, `APPLE_API_KEY_ID`, `APPLE_API_ISSUER_ID`, `APPLE_API_KEY_BASE64`. Rotate annually or when team membership changes; regenerate via Apple Developer portal → "Keys" → revoke old → mint new → update secrets.
- Windows code-signing certificate lives in `WINDOWS_PFX_BASE64` + `WINDOWS_PFX_PASSWORD`. Rotate when the cert nears expiry; obtain a new `.pfx`, base64-encode it (`base64 -w0 cert.pfx`), update secrets.
- After rotation, run a `workflow_dispatch` for the most recent release tag with `mark_latest=false` to validate the new keys without disturbing the live "latest" pointer. Once the smoke job passes, manually `gh release edit --latest` to promote.

---

## Schema evolution

`release-manifest.json` carries `"schema_version": 1`. The cloud manifest endpoint pre-dates this file and synthesizes URLs from env vars — it does not currently consume `release-manifest.json`. If a future cloud version reads the file (e.g. for sha256 verification), bump the schema version and document the delta here. Never change field semantics in place.

---

## Failure modes — what to check first

| Symptom | Likely cause | Fix |
|---|---|---|
| Job `build (windows arm64)` fails on `go build` | Toolchain regression on a new Go release | Pin `go-version-file: go.mod` to a known-good Go in `go.mod` |
| Smoke validation reports a missing asset | Upload step retried mid-stream | Re-run `workflow_dispatch` — uploads use `--clobber` so re-runs are idempotent |
| `gh release create` fails with `tag not found` | Tag was force-pushed during the run | Re-tag the same SHA, push, run dispatch with the new tag |
| Cloud `<DownloadAppCard />` shows 404s after publish | Cloud env vars stale | `ENGINE_TOOLS_LATEST_VERSION` and `ENGINE_TOOLS_RELEASE_BASE_URL` need redeploy on the cloud after each release |

---

## Related

- Cloud-side manifest endpoint: `wiki/architecture/engine-tools-release-manifest.md` in `AsteryVN/astery_engine`.
- Updater roadmap (delta updates, channels, staged rollout): `wiki/architecture/engine-tools-updater-roadmap.md` in `AsteryVN/astery_engine`.
- Frontend consumer: `frontend/src/components/desktop/DownloadAppCard.tsx` in `AsteryVN/astery_engine`.

## v0.2.x Tauri shell — manual E2E runbook

Run on each platform after `release-installer.yml` produces artifacts. The
goal is to catch sidecar lifecycle, pairing, and SSE regressions that unit
tests can't reach.

### Smoke test (per-platform — macOS, Windows, Linux)

1. **Install.** Open the installer (`.dmg` / `.msi` / `.AppImage`) and
   accept the unsigned-artifact warning per the documented workaround
   (Gatekeeper → right-click Open; SmartScreen → More info → Run anyway).
2. **First launch.** Double-click the app. Expected: window opens within
   3s; `engine-toold` child PID visible in `ps`/Task Manager.
3. **Pairing.** Generate a display code from the cloud admin UI →
   navigate to `/pairing` in the shell → enter the code → submit. Expect
   green "Paired" badge and auto-redirect to Dashboard.
4. **Dashboard polling.** Watch Status card refresh every 5s. Click
   Pause; verify status flips to `paused`; click Resume.
5. **Logs SSE.** Navigate to `/logs`. Expect `open` connection-state
   badge within 1s. Trigger a workload from the cloud → log lines stream
   in. Scroll up; verify auto-scroll pauses; scroll back to bottom;
   verify it resumes.
6. **Daemon-crash reconnect.** Find the daemon PID and `kill -9` it.
   Logs page should transition `open → reconnecting → give-up` within
   ~30s following the documented backoff schedule. Click "Reconnect"
   button → state should return to `connecting`. Click "Start daemon"
   on the error page (or relaunch); SSE resumes.
7. **App quit.** Close the window. Verify the daemon child PID exits
   within 2s (no orphaned processes).

### Pass/fail criteria

- All 7 steps complete without uncaught JS errors in devtools.
- No `ipc.token` value appears in the devtools network panel preview.
- No orphaned daemon process after window close.

### Known caveats (v0.2.x)

- Unsigned installers: Gatekeeper / SmartScreen warnings expected on
  first launch. Document for end users in release notes until the
  signing follow-up lands.
- `--with-ui` (daemon-spawns-shell) path is documented but not part of
  the default install flow; manual smoke optional.
