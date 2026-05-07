// Prevents an additional console window from appearing on Windows release
// builds. Has no effect on macOS / Linux / debug builds.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

mod daemon;

use tauri::Manager;

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .setup(|app| {
            // Best-effort sidecar spawn. If the daemon is already running
            // (port file present + responsive on /v1/status) we attach
            // instead — this is the --with-ui=true inverted flow.
            let handle = app.handle().clone();
            tauri::async_runtime::spawn_blocking(move || {
                if let Err(err) = daemon::spawn_daemon_once(&handle) {
                    eprintln!("[engine-tools-ui] sidecar spawn skipped: {err}");
                }
            });
            Ok(())
        })
        .on_window_event(|_window, event| {
            if let tauri::WindowEvent::CloseRequested { .. } = event {
                daemon::shutdown_child();
            }
        })
        .invoke_handler(tauri::generate_handler![
            daemon::ipc_token,
            daemon::ipc_port,
            daemon::start_daemon_sidecar,
        ])
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
