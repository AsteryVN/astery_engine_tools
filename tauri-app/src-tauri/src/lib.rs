//! Library root. The actual binary entry point is in `main.rs`; we re-export
//! the daemon module so the crate also exposes a stable surface for unit
//! tests that need to call `spawn_daemon_once` directly.

pub mod daemon;
