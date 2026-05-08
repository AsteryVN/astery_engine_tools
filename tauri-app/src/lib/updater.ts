// Auto-update gate. Runs once at app boot; non-blocking; fail-quiet.
//
// Lifecycle:
//   1. checkForUpdate() runs in App.tsx useEffect.
//   2. If the updater plugin throws (not configured / no pubkey embedded /
//      endpoint unreachable / signature mismatch) we swallow and resolve
//      with status 'unavailable'. The app launches normally.
//   3. If an update IS available, returns { status: 'available', version,
//      apply } where apply() runs downloadAndInstall + relaunch.
//   4. The renderer renders a toast offering "Restart to update" and calls
//      apply() on click.
//
// Why a wrapper module: the @tauri-apps/plugin-updater + plugin-process
// imports throw if the plugins aren't initialized (e.g. when running under
// vite dev outside Tauri). Centralizing the dynamic-import + try/catch
// keeps the rest of the app oblivious to "is the plugin available right
// now".

export type UpdateStatus =
  | { status: 'unavailable' }
  | {
      status: 'available';
      version: string;
      currentVersion: string;
      apply: () => Promise<void>;
    };

export async function checkForUpdate(): Promise<UpdateStatus> {
  try {
    // Dynamic import — plugin throws synchronously when not bundled (vite
    // dev mode without Tauri shell, vitest jsdom env). Catching at this
    // boundary keeps the rest of the app cleanly typed.
    const updaterMod = await import('@tauri-apps/plugin-updater');
    const processMod = await import('@tauri-apps/plugin-process');

    const update = await updaterMod.check();
    if (!update) {
      return { status: 'unavailable' };
    }

    return {
      status: 'available',
      version: update.version,
      currentVersion: update.currentVersion,
      apply: async () => {
        await update.downloadAndInstall();
        await processMod.relaunch();
      },
    };
  } catch (err) {
    // Plugin missing, endpoint down, signature mismatch, network down,
    // running outside Tauri — all map to "no update available". Logged
    // for diagnostics; never surfaced to the user.
    if (typeof console !== 'undefined' && typeof console.debug === 'function') {
      console.debug('[updater] check skipped:', (err as Error).message ?? err);
    }
    return { status: 'unavailable' };
  }
}
