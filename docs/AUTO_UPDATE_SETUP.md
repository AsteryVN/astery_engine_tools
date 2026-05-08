# Auto-update setup (one-time, per fresh checkout)

The engine-tools desktop app uses Tauri v2's `tauri-plugin-updater` to check
for new releases on launch, prompt the user to restart, and atomically swap
to the new version. The plugin verifies an ed25519 signature on the
`latest.json` manifest before downloading anything, so a tampered release
asset cannot trigger an install.

This is a one-time setup. Once the keypair is generated and the public key
embedded in `tauri.conf.json`, every subsequent release that
`.github/workflows/release.yml` builds will be auto-discoverable by the
desktop app on launch.

---

## 1. Generate the signing keypair

On your laptop (NOT in CI), run:

```bash
cd engine-tools/tauri-app
pnpm tauri signer generate -w ~/.tauri/astery-engine-tools.key
```

The command emits two values:
- A **password-protected private key** at `~/.tauri/astery-engine-tools.key`.
  Keep this on your machine + a sealed offline backup. Never commit. If
  you lose it you cannot ship updates that the existing v0.2.1+ installs
  will accept — every device must reinstall manually.
- A **base64-encoded public key** printed to stdout (line starts with
  `dW50cnVzdGVkIGNvbW1lbnQ6...` or similar). This is what gets embedded in
  the app and CAN be committed.

## 2. Embed the public key in `tauri.conf.json`

Open `engine-tools/tauri-app/src-tauri/tauri.conf.json` and add a `plugins`
section at the bottom, alongside `app`, `build`, `bundle`:

```jsonc
"plugins": {
  "updater": {
    "endpoints": [
      "https://github.com/AsteryVN/astery_engine_tools/releases/latest/download/latest.json"
    ],
    "pubkey": "<paste the base64 public key from step 1>",
    "windows": {
      "installMode": "passive"
    }
  }
}
```

Commit this change. The pubkey is NOT a secret — it ships with the binary so
the binary can verify signatures.

## 3. Add the private key + password to GitHub Secrets

Repository secrets:
- `TAURI_SIGNING_PRIVATE_KEY` — full contents of
  `~/.tauri/astery-engine-tools.key`
- `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` — the password you set in step 1

These are read by the release workflow when it produces the signed
manifest.

## 4. Release pipeline integration

The Tauri shell installers ship from `.github/workflows/release-installer.yml`
(not `release.yml` — that one ships the headless `engine-toold` daemon
archives). When `TAURI_SIGNING_PRIVATE_KEY` + `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`
are set in the build env, `pnpm tauri build` automatically:

1. Produces the user-facing installer (`.dmg` / `.msi` / `.AppImage`).
2. On macOS, also produces the updater bundle `*.app.tar.gz` under
   `bundle/macos/` (the `.dmg` is install-only — the updater swaps the
   `.app` directly).
3. Writes a `.sig` sidecar next to each updater bundle (one base64 line of
   minisign output).

The `release-installer.yml` `publish` job then composes `latest.json` from
those `.sig` files and the predictable updater-bundle URLs:

```json
{
  "version": "0.2.1",
  "notes": "<release notes>",
  "pub_date": "2026-05-08T12:00:00Z",
  "platforms": {
    "darwin-aarch64": { "signature": "...", "url": ".../astery-engine-tools_v0.2.1_darwin_arm64_updater.app.tar.gz" },
    "windows-x86_64": { "signature": "...", "url": ".../astery-engine-tools_v0.2.1_windows_x64_installer.msi" },
    "linux-x86_64":   { "signature": "...", "url": ".../astery-engine-tools_v0.2.1_linux_x64_installer.AppImage" },
    "linux-aarch64":  { "signature": "...", "url": ".../astery-engine-tools_v0.2.1_linux_arm64_installer.AppImage" }
  }
}
```

`darwin-x86_64` is intentionally absent — `release-installer.yml` does not
build that target (macos-13 runners are not allocated on the current GitHub
plan). darwin-x64 users stay on the headless daemon archive from
`release.yml` until that runner is available.

The composed `latest.json` is uploaded via `gh release upload`, so it
lands at:
`https://github.com/AsteryVN/astery_engine_tools/releases/latest/download/latest.json`
— the URL embedded in `tauri.conf.json::endpoints`.

## 5. First release rollout

The first signed release (v0.2.1 or whatever you tag) will NOT auto-update
to existing v0.2.0 installs — those installs were built without the
updater plugin embedded. Users on v0.2.0 must download and install the new
version manually once. From v0.2.1 onward, every install auto-updates.

This is a known one-time tax of introducing the auto-update plugin to an
already-shipped app. Note it in the v0.2.1 release notes:

> **Manual install required.** This is the first release with auto-update.
> If you're upgrading from v0.2.0, please download and reinstall once;
> future updates will install themselves.

## 6. Test the flow before broad rollout

1. On your laptop, install the v0.2.0 build manually.
2. Bump the local checkout to v0.2.1, build, sign, publish to a private
   draft release with a custom `latest.json` URL.
3. Override the prod endpoint locally: edit `tauri.conf.json::endpoints` to
   point at the draft release's `latest.json`.
4. Launch v0.2.0 — within ~10s the toast should appear.
5. Click restart → v0.2.1 boots.
6. Revert the endpoint override before tagging the public release.

## Threat model

- **Compromised GitHub account** → attacker pushes a tampered `latest.json`
  but cannot sign new archives → updater rejects manifest → no install.
- **Lost private key** → cannot sign new releases → existing installs go
  stale until users manually upgrade. Mitigation: keep an offline backup
  AND consider a 6-month key rotation cadence (each rotation requires a
  one-time manual reinstall, so don't rotate often).
- **Compromised pubkey** in tauri.conf.json → not a vulnerability; pubkeys
  are not secrets. An attacker who modifies the embedded pubkey is
  effectively building a different app and can't push updates to existing
  installs.

## Related

- Tauri docs: https://v2.tauri.app/plugin/updater/
- Plugin source: https://github.com/tauri-apps/plugins-workspace/tree/v2/plugins/updater
