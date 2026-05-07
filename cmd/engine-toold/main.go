// Command engine-toold is the Astery Engine Tools daemon.
//
// It runs as the primary local compute runtime: schedules workloads claimed
// from the Astery cloud control plane, executes them via pluggable executors
// (FFmpeg, future AI inference, GPU), and uploads outputs back to cloud
// storage. Tauri shell is an optional UI surface that talks to this daemon
// over loopback HTTP JSON; the daemon also runs headless for server / NAS
// deployments.
//
// This is the v0.1 skeleton — it boots, logs, handles SIGTERM, and exits.
// Real lifecycle wiring (sync, scheduler, registry) lands in subsequent PRs
// per the implementation roadmap in the cloud repo's
// wiki/architecture/engine-tools-hybrid.md.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version is overridden at build time via -ldflags "-X main.version=..."
var version = "0.0.0-dev"

func main() {
	var (
		headless = flag.Bool("headless", false, "run without spawning the Tauri shell (server/NAS mode)")
		dataDir  = flag.String("data-dir", "", "override the data directory (default: per-OS XDG/AppData/Library)")
		listen   = flag.String("listen", "127.0.0.1:0", "address for the loopback IPC server")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}).WithAttrs([]slog.Attr{
		slog.String("service", "engine-toold"),
		slog.String("version", version),
	}))
	slog.SetDefault(logger)

	logger.Info("engine-toold starting",
		"headless", *headless,
		"data_dir", *dataDir,
		"listen", *listen,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	<-ctx.Done()

	logger.Info("engine-toold shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = shutdownCtx

	logger.Info("engine-toold exited cleanly")
}
