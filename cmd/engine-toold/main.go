// Command engine-toold is the Astery Engine Tools daemon — primary local
// compute runtime. Pairs with the cloud, claims workloads, executes via
// pluggable executors (FFmpeg today, AI/GPU later), uploads outputs.
//
// Modes:
//   --headless         daemon only; no Tauri shell.
//   --pair <code>      one-shot: exchange the pairing display code, store
//                      the session, exit. Use to pair a fresh install via CLI.
//   --listen <addr>    IPC listen address (default 127.0.0.1:0).
//   --data-dir <path>  override the per-OS data directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/AsteryVN/astery_engine_tools/internal/auth"
	"github.com/AsteryVN/astery_engine_tools/internal/executors/clipvideo"
	"github.com/AsteryVN/astery_engine_tools/internal/ipc"
	"github.com/AsteryVN/astery_engine_tools/internal/jobqueue"
	"github.com/AsteryVN/astery_engine_tools/internal/observability"
	rt "github.com/AsteryVN/astery_engine_tools/internal/runtime"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/registry"
	"github.com/AsteryVN/astery_engine_tools/internal/runtime/resources"
	"github.com/AsteryVN/astery_engine_tools/internal/storage"
	cloudsync "github.com/AsteryVN/astery_engine_tools/internal/sync"
	"github.com/AsteryVN/astery_engine_tools/internal/tools"
	"github.com/AsteryVN/astery_engine_tools/internal/upload"
)

// version is overridden at build time via -ldflags.
var version = "0.1.0-dev"

func main() {
	var (
		headless    = flag.Bool("headless", true, "run without spawning the Tauri shell")
		pairCode    = flag.String("pair", "", "exchange a pairing display code, store the session, then exit")
		dataDir     = flag.String("data-dir", "", "override the data directory")
		listen      = flag.String("listen", "127.0.0.1:0", "IPC listen address")
		cloudURL    = flag.String("cloud-url", envOr("ENGINE_CLOUD_URL", "http://localhost:8080/api"), "cloud control plane base URL")
		displayName = flag.String("display-name", envOr("ENGINE_DISPLAY_NAME", defaultDisplayName()), "device display name")
	)
	flag.Parse()

	layout, err := storage.Resolve(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve data dir:", err)
		os.Exit(1)
	}
	if err := layout.EnsureLayout(); err != nil {
		fmt.Fprintln(os.Stderr, "ensure layout:", err)
		os.Exit(1)
	}

	installID, err := rt.LoadOrCreateInstallationID(layout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "installation id:", err)
		os.Exit(1)
	}
	logger := observability.Setup(slog.LevelInfo, observability.Attrs{
		AppVersion:     version,
		RuntimeVersion: version,
		InstallationID: installID.String(),
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
	})
	logger.Info("engine-toold boot",
		"data_dir", layout.Root,
		"cloud_url", *cloudURL,
		"headless", *headless,
	)

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	keystore := auth.NewKeystore(layout.Secrets)
	pairingClient := auth.NewPairingClient(*cloudURL)

	// One-shot pairing path.
	if *pairCode != "" {
		if err := runPair(rootCtx, *pairCode, *displayName, installID.String(), pairingClient, keystore); err != nil {
			fmt.Fprintln(os.Stderr, "pair:", err)
			os.Exit(1)
		}
		fmt.Println("pairing successful")
		return
	}

	// Daemon path.
	tokenSrc := auth.NewTokenSource(keystore, pairingClient)
	bundle, err := keystore.Load()
	if err == nil {
		_ = tokenSrc.Set(bundle)
	}

	store, err := jobqueue.Open(layout.JobsDB)
	if err != nil {
		logger.Error("open jobs db", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	tm := tools.New()
	if t, lerr := tm.Locate(rootCtx, "ffmpeg"); lerr == nil {
		logger.Info("ffmpeg located", "path", t.Path, "version", t.Version)
	} else {
		logger.Warn("ffmpeg not found — clip-video executor will fail at run time", "error", lerr)
	}

	reg := registry.New()
	reg.Register(clipvideo.New(tm))

	resMgr := resources.New(resources.Limits{}, layout.Root)

	cloud := cloudsync.New(cloudsync.Config{
		BaseURL:     *cloudURL,
		TokenSource: tokenSrc,
		Fingerprint: rt.HardwareFingerprint(),
	})

	uploader := upload.New()

	scheduler := rt.NewScheduler(rt.SchedulerDeps{
		Registry:  reg,
		Resources: resMgr,
		Cloud:     cloud,
		Upload:    uploader,
		Store:     store,
		BaseTmp:   layout.Tmp,
	})

	ipcToken, err := rt.LoadOrCreateIPCToken(layout)
	if err != nil {
		logger.Error("ipc token", "error", err)
		os.Exit(1)
	}
	ipcServer, err := ipc.Listen(*listen, ipcToken, layout.IpcPort, ipc.Deps{
		Store:      store,
		Resources:  resMgr,
		Pause:      scheduler.Pause,
		Resume:     scheduler.Resume,
		Paused:     scheduler.Paused,
		AppVersion: version,
	})
	if err != nil {
		logger.Error("ipc listen", "error", err)
		os.Exit(1)
	}
	logger.Info("ipc listening", "addr", ipcServer.Addr())

	go scheduler.Run(rootCtx)
	go func() {
		if err := ipcServer.Serve(rootCtx); err != nil && err != context.Canceled {
			logger.Warn("ipc server stopped", "error", err)
		}
	}()

	<-rootCtx.Done()
	logger.Info("engine-toold shutdown signal received")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = shutdownCtx
	logger.Info("engine-toold exited cleanly")
}

// runPair performs the pairing exchange and persists the bundle.
func runPair(ctx context.Context, code, displayName, installationID string, pc *auth.PairingClient, ks auth.Keystore) error {
	host, _ := os.Hostname()
	resp, err := pc.Exchange(ctx, auth.ExchangeRequest{
		DisplayCode: code,
		Device: auth.Device{
			InstallationID: installationID,
			DisplayName:    displayName,
			Hostname:       host,
			OS:             runtime.GOOS,
			Arch:           runtime.GOARCH,
			AppVersion:     version,
			RuntimeVersion: version,
		},
		HwFingerprint: rt.HardwareFingerprint(),
	})
	if err != nil {
		return fmt.Errorf("exchange: %w", err)
	}
	bundle := auth.SessionBundle{
		DeviceID:         resp.Device.ID,
		OrganizationID:   resp.Device.OrganizationID,
		SessionJWT:       resp.Session.Token,
		SessionExpiresAt: resp.Session.ExpiresAt,
		RefreshToken:     resp.Refresh.Token,
		RefreshExpiresAt: resp.Refresh.ExpiresAt,
		HwFingerprint:    rt.HardwareFingerprint(),
	}
	return ks.Save(bundle)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultDisplayName() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "engine-tools"
	}
	return host
}
